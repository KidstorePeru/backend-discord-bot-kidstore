package discordbot

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/types"
	"fmt"
	"log/slog"

	"github.com/bwmarrin/discordgo"
)

// isAdmin solo autoriza al Discord ID configurado como administrador — se
// revisa aquí además de los permisos de Discord (DefaultMemberPermissions)
// por si el rol de administrador del servidor cambia o se asigna mal.
func isAdmin(userID string) bool {
	return cfg.DiscordAdminUserID != "" && userID == cfg.DiscordAdminUserID
}

func adminPermission() *int64 {
	perm := int64(discordgo.PermissionAdministrator)
	return &perm
}

func registerKCCommand() {
	if session == nil || cfg.DiscordGuildID == "" {
		return
	}
	userOpt := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionUser, Name: name, Description: desc}
	}
	epicOpt := func() *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString, Name: "epic_user", Description: "Usuario Epic del cliente (si no vinculó Discord)"}
	}
	amountOpt := func() *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionInteger, Name: "amount", Description: "Cantidad de KC", Required: true, MinValue: floatPtr(1)}
	}
	noteOpt := func() *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString, Name: "note", Description: "Nota (opcional)"}
	}

	cmd := &discordgo.ApplicationCommand{
		Name:                     "kc",
		Description:              "Administrar KidCoins de un cliente (solo admin)",
		DefaultMemberPermissions: adminPermission(),
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "add", Description: "Sumar KC a un cliente",
				Options: []*discordgo.ApplicationCommandOption{
					amountOpt(),
					userOpt("discord_user", "Cliente por mención de Discord (si vinculó su cuenta)"),
					epicOpt(), noteOpt(),
				},
			},
			{
				Type: discordgo.ApplicationCommandOptionSubCommand, Name: "remove", Description: "Quitar KC a un cliente",
				Options: []*discordgo.ApplicationCommandOption{
					amountOpt(),
					userOpt("discord_user", "Cliente por mención de Discord (si vinculó su cuenta)"),
					epicOpt(), noteOpt(),
				},
			},
		},
	}
	if _, err := session.ApplicationCommandCreate(session.State.User.ID, cfg.DiscordGuildID, cmd); err != nil {
		slog.Error("Discord bot: error registrando /kc", "error", err)
	}
}

func handleKCCommand(s *discordgo.Session, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	var callerID, callerTag string
	if u := interactionUser(i); u != nil {
		callerID, callerTag = u.ID, u.Username
	}
	if !isAdmin(callerID) {
		respondEphemeral(s, i, "❌ No tienes permiso para usar este comando.")
		return
	}

	sub := data.Options[0]
	var discordUserID, epicUser, note string
	var amount int64
	for _, opt := range sub.Options {
		switch opt.Name {
		case "discord_user":
			discordUserID = opt.UserValue(s).ID
		case "epic_user":
			epicUser = opt.StringValue()
		case "amount":
			amount = opt.IntValue()
		case "note":
			note = opt.StringValue()
		}
	}

	if discordUserID == "" && epicUser == "" {
		respondEphemeral(s, i, "❌ Debes indicar `discord_user` o `epic_user`.")
		return
	}

	var target types.Customer
	var resolveErr error
	if discordUserID != "" {
		target, resolveErr = db.GetCustomerByDiscordID(database, discordUserID)
	} else {
		target, resolveErr = db.GetCustomerByEpicUsername(database, epicUser)
	}
	if resolveErr != nil {
		respondEphemeral(s, i, "❌ No se encontró ningún cliente con esos datos.")
		return
	}

	approvedBy := "discord:" + callerTag

	var newBalance int
	var opErr error
	var verb string
	if sub.Name == "add" {
		verb = "sumaron"
		var notePtr *string
		if note != "" {
			notePtr = &note
		}
		opErr = db.RechargeKC(database, target.ID, int(amount), nil, notePtr, approvedBy, "manual_discord")
		if opErr == nil {
			updated, _ := db.GetCustomerByID(database, target.ID)
			newBalance = updated.KCBalance
			NotifyRecharge(updated, int(amount), newBalance, "Manual (Discord)")
		}
	} else {
		verb = "quitaron"
		newBalance, opErr = db.DeductKCManual(database, target.ID, int(amount))
	}

	if opErr != nil {
		respondEphemeral(s, i, "❌ "+opErr.Error())
		return
	}

	db.AddAuditLog(database, &target.ID, "KC_ADJUSTED_DISCORD",
		fmt.Sprintf("%s %d KC via /kc %s por %s — nota: %s", verb, amount, sub.Name, approvedBy, note), "discord")

	action, color := "➕ KC agregados", colorSuccess
	if sub.Name == "remove" {
		action, color = "➖ KC removidos", colorGold
	}
	embed := &discordgo.MessageEmbed{
		Title: "✅ Balance actualizado",
		Color: color,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "👤 Cliente", Value: target.EpicUsername, Inline: true},
			{Name: action, Value: fmt.Sprintf("%d KC", amount), Inline: true},
			{Name: "💼 Nuevo balance", Value: fmt.Sprintf("**%d KC**", newBalance), Inline: true},
			{Name: "🛠️ Autorizado por", Value: callerTag, Inline: true},
		},
	}
	if note != "" {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{Name: "📝 Nota", Value: note, Inline: false})
	}
	respondEmbedEphemeral(s, i, embed)
}

func respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		slog.Error("Discord bot: error respondiendo interacción", "error", err)
	}
}
