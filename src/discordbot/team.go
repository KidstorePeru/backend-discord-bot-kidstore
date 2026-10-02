package discordbot

// El equipo de administración en Discord: el dueño (DISCORD_ADMIN_USER_ID) y
// las cuentas marcadas como admin en el panel web que tienen su Discord
// vinculado. Todos reciben los avisos por mensaje privado y pueden aprobar
// comprobantes o usar /kc. Para sumar o quitar a alguien basta con marcarlo
// o desmarcarlo como admin en el panel; no hay que tocar Railway.

import (
	"log/slog"
	"strings"

	"KidStoreStore/src/db"

	"github.com/bwmarrin/discordgo"
)

// adminDiscordIDs: el dueño primero, sin repetidos.
func adminDiscordIDs() []string {
	var ids []string
	seen := map[string]bool{}
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	add(cfg.DiscordAdminUserID)
	if database != nil {
		team, err := db.AdminDiscordIDs(database)
		if err != nil {
			slog.Warn("Discord: no se pudo leer el equipo de admins; solo se avisa al dueño", "error", err)
		}
		for _, id := range team {
			add(id)
		}
	}
	return ids
}

// isAdmin autoriza al dueño y al equipo. Se revisa aquí además de los
// permisos de Discord (DefaultMemberPermissions) por si un rol del servidor
// cambia o se asigna mal.
func isAdmin(userID string) bool {
	if userID == "" {
		return false
	}
	if cfg.DiscordAdminUserID != "" && userID == cfg.DiscordAdminUserID {
		return true
	}
	if database == nil {
		return false
	}
	ok, err := db.IsAdminDiscordID(database, userID)
	if err != nil {
		slog.Warn("Discord: no se pudo verificar si es admin", "user", userID, "error", err)
	}
	return err == nil && ok
}

// sendToAdmins manda el mismo aviso por privado a cada miembro del equipo.
func sendToAdmins(embed *discordgo.MessageEmbed) {
	for _, id := range adminDiscordIDs() {
		sendDM(id, embed)
	}
}

// NotifyNewAdmin le da la bienvenida por privado a quien se acaba de sumar al
// equipo desde el panel (así también se comprueba que sus mensajes privados
// están abiertos). Devuelve false si no se pudo enviar.
func NotifyNewAdmin(discordUserID, epicUsername string) bool {
	if !Enabled() || session == nil || discordUserID == "" {
		return false
	}
	dm, err := session.UserChannelCreate(discordUserID)
	if err != nil {
		slog.Warn("Discord: no se pudo abrir el DM del nuevo admin", "user", discordUserID, "error", err)
		return false
	}
	embed := &discordgo.MessageEmbed{
		Title: "🛡️ Ahora eres parte del equipo de KidStorePeru",
		Description: "Hola **" + epicUsername + "**, ya tienes acceso de administrador.\n\n" +
			"• Por aquí te llegarán los **comprobantes de pago** con botones para aprobarlos o rechazarlos, y las **alertas** de la tienda.\n" +
			"• Puedes acreditar o quitar KC con **/kc** en el servidor.\n" +
			"• Todo lo demás está en el panel: https://www.kidstoreperu.net/admin\n\n" +
			"Lo que apruebes o rechaces queda registrado con tu nombre.",
		Color: colorSuccess,
	}
	if _, err := session.ChannelMessageSendEmbed(dm.ID, brand(embed)); err != nil {
		slog.Warn("Discord: no se pudo enviar la bienvenida al nuevo admin", "user", discordUserID, "error", err)
		return false
	}
	return true
}
