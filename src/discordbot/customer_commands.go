package discordbot

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/types"
	"fmt"
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"
)

func registerCustomerCommands() {
	if session == nil || cfg.DiscordGuildID == "" {
		return
	}
	cmds := []*discordgo.ApplicationCommand{
		{
			Name: "perfil", Description: "Ver tu perfil de KidStorePeru (o el de otro miembro)",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionUser, Name: "usuario", Description: "Ver el perfil de otro miembro (información limitada)"},
			},
		},
		{Name: "bots", Description: "Ver el estado de las cuentas bot"},
		{Name: "misordenes", Description: "Ver tus últimos pedidos"},
		{Name: "ayuda", Description: "Ver los comandos disponibles"},
	}
	for _, cmd := range cmds {
		if _, err := session.ApplicationCommandCreate(session.State.User.ID, cfg.DiscordGuildID, cmd); err != nil {
			slog.Error("Discord bot: error registrando comando", "command", cmd.Name, "error", err)
		}
	}
}

func resolveCallerCustomer(i *discordgo.InteractionCreate) (types.Customer, error) {
	var discordUserID string
	if u := interactionUser(i); u != nil {
		discordUserID = u.ID
	}
	return db.GetCustomerByDiscordID(database, discordUserID)
}

func callerAvatarURL(i *discordgo.InteractionCreate) string {
	if u := interactionUser(i); u != nil {
		return u.AvatarURL("128")
	}
	return ""
}

var notLinkedMsg = fmt.Sprintf("❌ Tu cuenta de Discord no está vinculada a KidStorePeru. Vincúlala en [kidstoreperu.net](%s/account/security) → Mi Cuenta → Seguridad.", siteURL)

// levelFor calcula el nivel del cliente igual que la web (Profile.tsx): según
// el total de KC gastado en pedidos entregados.
func levelFor(totalSpentKC int) (name, emoji string) {
	switch {
	case totalSpentKC >= 10000:
		return "Legend", "👑"
	case totalSpentKC >= 4000:
		return "Pro", "🔥"
	case totalSpentKC >= 1000:
		return "Gamer", "🎮"
	default:
		return "Starter", "⚡"
	}
}

func handlePerfilCommand(s *discordgo.Session, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	var targetDiscordUser *discordgo.User
	for _, opt := range data.Options {
		if opt.Name == "usuario" {
			targetDiscordUser = opt.UserValue(s)
		}
	}

	var callerDiscordID string
	if u := interactionUser(i); u != nil {
		callerDiscordID = u.ID
	}

	// Sin "usuario" o pidiendo su propio perfil → vista completa.
	if targetDiscordUser == nil || targetDiscordUser.ID == callerDiscordID {
		handleOwnProfile(s, i)
		return
	}

	// Perfil de otro miembro → vista pública limitada.
	target, err := db.GetCustomerByDiscordID(database, targetDiscordUser.ID)
	if err != nil {
		respondEphemeral(s, i, fmt.Sprintf("❌ **%s** no tiene su cuenta de Discord vinculada a KidStorePeru.", targetDiscordUser.Username))
		return
	}

	_, _, totalSpentKC, _ := db.GetCustomerOrderStats(database, target.ID)
	levelName, levelEmoji := levelFor(totalSpentKC)

	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("%s %s", levelEmoji, target.EpicUsername),
		Description: fmt.Sprintf("Nivel **%s** en KidStorePeru", levelName),
		Color:       colorAccent,
		Thumbnail:   &discordgo.MessageEmbedThumbnail{URL: targetDiscordUser.AvatarURL("128")},
		Fields: []*discordgo.MessageEmbedField{
			{Name: "📅 Miembro desde", Value: target.CreatedAt.Format("2 de January de 2006"), Inline: true},
		},
	}
	respondEmbedEphemeral(s, i, embed)
}

func handleOwnProfile(s *discordgo.Session, i *discordgo.InteractionCreate) {
	customer, err := resolveCallerCustomer(i)
	if err != nil {
		respondEphemeral(s, i, notLinkedMsg)
		return
	}

	email := "—"
	if customer.Email != nil {
		email = *customer.Email
	}
	linked := []string{}
	if customer.HasPassword {
		linked = append(linked, "🔑 Contraseña")
	}
	if customer.GoogleID != nil {
		linked = append(linked, "🔵 Google")
	}
	if customer.DiscordID != nil {
		linked = append(linked, "🟣 Discord")
	}
	linkedStr := "—"
	if len(linked) > 0 {
		linkedStr = joinComma(linked)
	}

	totalOrders, sentOrders, totalSpentKC, _ := db.GetCustomerOrderStats(database, customer.ID)
	levelName, levelEmoji := levelFor(totalSpentKC)

	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("%s %s", levelEmoji, customer.EpicUsername),
		Description: fmt.Sprintf("Nivel **%s** en KidStorePeru", levelName),
		Color:       colorAccent,
		Thumbnail:   &discordgo.MessageEmbedThumbnail{URL: callerAvatarURL(i)},
		Fields: []*discordgo.MessageEmbedField{
			{Name: "💼 Balance KC", Value: fmt.Sprintf("**%d KC**", customer.KCBalance), Inline: true},
			{Name: "📦 Pedidos totales", Value: fmt.Sprintf("%d", totalOrders), Inline: true},
			{Name: "✅ Entregados", Value: fmt.Sprintf("%d", sentOrders), Inline: true},
			{Name: "💸 KC gastado", Value: fmt.Sprintf("%d KC", totalSpentKC), Inline: true},
			{Name: "📧 Email", Value: email, Inline: true},
			{Name: "🔗 Métodos vinculados", Value: linkedStr, Inline: false},
			{Name: "📅 Miembro desde", Value: customer.CreatedAt.Format("2 de January de 2006"), Inline: false},
		},
	}
	respondEmbedEphemeral(s, i, embed)
}

func handleBotsCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	accounts, err := db.GetAllGameAccounts(database, cfg.EncryptionKey)
	if err != nil {
		respondEphemeral(s, i, "❌ No se pudo consultar el estado de los bots.")
		return
	}
	inSchedule, _ := db.IsWithinSchedule(database)
	schedule, _ := db.GetBotSchedule(database)

	activeCount, availableCount := 0, 0
	for _, a := range accounts {
		if a.IsActive {
			activeCount++
			if inSchedule && a.RemainingGifts > 0 {
				availableCount++
			}
		}
	}

	status := "🟢 Disponible — puedes comprar ahora mismo"
	color := colorSuccess
	if !inSchedule {
		status = "🌙 Fuera de horario de atención"
		color = colorAccent
	} else if availableCount == 0 {
		status = "🔴 Sin envíos disponibles por ahora"
		color = colorLose
	}

	currentTime := "—"
	if loc, errLoc := time.LoadLocation(schedule.Timezone); errLoc == nil {
		currentTime = time.Now().In(loc).Format("15:04")
	}

	embed := &discordgo.MessageEmbed{
		Title:       "🤖 Estado de las cuentas bot",
		Description: status,
		Color:       color,
		Thumbnail:   &discordgo.MessageEmbedThumbnail{URL: iconURL},
		Fields: []*discordgo.MessageEmbedField{
			{Name: "✅ Cuentas activas", Value: fmt.Sprintf("%d", activeCount), Inline: true},
			{Name: "🎁 Con envíos disponibles", Value: fmt.Sprintf("%d", availableCount), Inline: true},
			{Name: "🕐 Hora actual (Lima)", Value: currentTime, Inline: true},
			{Name: "🗓️ Horario de atención", Value: fmt.Sprintf("%02d:00 — %02d:00 (%s)", schedule.StartHour, schedule.EndHour, schedule.Timezone), Inline: false},
		},
	}
	respondEmbedEphemeral(s, i, embed)
}

// "failed" y "refunded" se muestran distinto a propósito: un pedido queda
// en "failed" apenas se intenta el reembolso, y solo pasa a "refunded"
// cuando ese reembolso REALMENTE se confirma (ver failOrderAndRefund en
// shop.go) — normalmente son segundos, pero en el caso raro de que el
// reembolso falle y quede pendiente de reintento automático (unos
// minutos, ver RetryFailedRefunds), decirle al cliente "Reembolsado" sin
// que sea cierto todavía sería el mismo problema que se corrigió en el
// correo de pedido fallido.
var orderStatusLabel = map[string]string{
	"pending":    "🟡 En procesamiento",
	"processing": "🔵 En entrega",
	"sent":       "🟢 Completado",
	"failed":     "🟠 Reembolso en proceso",
	"refunded":   "⚪ Reembolsado",
}

func handleMisOrdenesCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	customer, err := resolveCallerCustomer(i)
	if err != nil {
		respondEphemeral(s, i, notLinkedMsg)
		return
	}

	orders, total, err := db.GetOrdersByCustomer(database, customer.ID, 1, 5)
	if err != nil || len(orders) == 0 {
		respondEphemeral(s, i, fmt.Sprintf("📦 No tienes pedidos todavía. Visita [la Tienda](%s/store) para comprar.", siteURL))
		return
	}

	embed := &discordgo.MessageEmbed{
		Title:       "📦 Tus últimos pedidos",
		Color:       colorAccent,
		Description: fmt.Sprintf("Mostrando %d de **%d** pedido(s) de **%s**", len(orders), total, customer.EpicUsername),
		Thumbnail:   &discordgo.MessageEmbedThumbnail{URL: iconURL},
	}
	if orders[0].ItemImage != nil && *orders[0].ItemImage != "" {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: *orders[0].ItemImage}
	}
	for _, o := range orders {
		label, ok := orderStatusLabel[o.Status]
		if !ok {
			label = o.Status
		}
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   fmt.Sprintf("🎮 %s", o.ItemName),
			Value:  fmt.Sprintf("%s · %d KC · %s", label, o.PriceKC, o.CreatedAt.Format("02/01/2006")),
			Inline: false,
		})
	}
	respondEmbedEphemeral(s, i, embed)
}

func handleAyudaCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	embed := &discordgo.MessageEmbed{
		Title:       "📖 Comandos de KidStorePeru",
		Description: fmt.Sprintf("Todo lo que puedes hacer sin salir de Discord. Visita [kidstoreperu.net](%s) para más.", siteURL),
		Color:       colorAccent,
		Thumbnail:   &discordgo.MessageEmbedThumbnail{URL: logoURL},
		Fields: []*discordgo.MessageEmbedField{
			{Name: "👤 Cuenta", Value: "`/perfil` — tu perfil, nivel, balance y métodos vinculados\n`/perfil usuario:@alguien` — ver el nivel de otro miembro (público)\n`/misordenes` — tus últimos pedidos y su estado", Inline: false},
			{Name: "🛍️ Tienda", Value: "`/bots` — si hay cuentas bot disponibles ahora mismo", Inline: false},
			{Name: "🎰 Diversión", Value: "`/slot amount:<10-10000>` — apuesta KidCoins en la tragamonedas", Inline: false},
			{Name: "❓ Ayuda", Value: "`/ayuda` — este mensaje", Inline: false},
		},
	}
	respondEmbedEphemeral(s, i, embed)
}

func respondEmbedEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed) {
	brand(embed)
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
			Flags:  discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		slog.Error("Discord bot: error respondiendo comando", "error", err)
	}
}

func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
