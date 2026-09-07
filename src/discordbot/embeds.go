package discordbot

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/types"
	"fmt"
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"
)

// emailSender manda el correo de "pago aprobado" desde el comando /kc add
// del bot sin que este paquete tenga que importar "store" directamente —
// store ya importa discordbot para las notificaciones (NotifyRecharge, etc.),
// así que importar al revés crearía un ciclo. main.go conecta esta función
// una sola vez al arrancar, con store.SendPaymentApprovedEmail (misma firma).
var emailSender func(cfg types.EnvConfig, toEmail, productName string, amountPEN float64, kcAmount int, gateway, voucherURL, lang string)

// SetEmailSender registra la función que manda el correo de pago aprobado.
func SetEmailSender(fn func(cfg types.EnvConfig, toEmail, productName string, amountPEN float64, kcAmount int, gateway, voucherURL, lang string)) {
	emailSender = fn
}

const (
	colorAccent  = 0x6C5CE7
	colorSuccess = 0x22C55E
	colorGold    = 0xF59E0B
	colorLose    = 0xEF4444

	siteURL = "https://www.kidstoreperu.net"
	iconURL = "https://www.kidstoreperu.net/isotipo-kidstore.png"
	logoURL = "https://www.kidstoreperu.net/logotipo.png"
)

// brand aplica el estilo consistente de KidStorePeru a cualquier embed:
// autor con logo enlazando al sitio, pie de página, y timestamp si no tiene uno ya.
func brand(embed *discordgo.MessageEmbed) *discordgo.MessageEmbed {
	embed.Author = &discordgo.MessageEmbedAuthor{Name: "KidStorePeru", IconURL: iconURL, URL: siteURL}
	embed.Footer = &discordgo.MessageEmbedFooter{Text: "KidStorePeru · Tienda de Fortnite", IconURL: iconURL}
	if embed.Timestamp == "" {
		embed.Timestamp = time.Now().Format(time.RFC3339)
	}
	return embed
}

func sendChannelEmbed(channelID string, embed *discordgo.MessageEmbed, files ...*discordgo.File) {
	if !Enabled() || session == nil || channelID == "" {
		return
	}
	brand(embed)
	msg := &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}, Files: files}
	if _, err := session.ChannelMessageSendComplex(channelID, msg); err != nil {
		slog.Error("Discord bot: error enviando embed a canal", "channel", channelID, "error", err)
	}
}

func sendDM(discordUserID string, embed *discordgo.MessageEmbed) {
	if !Enabled() || session == nil || discordUserID == "" {
		return
	}
	dm, err := session.UserChannelCreate(discordUserID)
	if err != nil {
		slog.Warn("Discord bot: no se pudo abrir DM", "user", discordUserID, "error", err)
		return
	}
	if _, err := session.ChannelMessageSendEmbed(dm.ID, brand(embed)); err != nil {
		slog.Warn("Discord bot: no se pudo enviar DM", "user", discordUserID, "error", err)
	}
}

// NotifyWelcome se llama al registrarse o al vincular Discord por primera vez.
// Publica un embed en el canal de bienvenida siempre, y además un DM personal
// si ya conocemos el Discord ID del cliente.
func NotifyWelcome(customer types.Customer) {
	if !Enabled() {
		return
	}
	memberNote := ""
	if count, err := db.CountActiveCustomers(database); err == nil {
		memberNote = fmt.Sprintf("\n🎫 Eres el miembro **#%d** de la comunidad.", count)
	}

	channelEmbed := &discordgo.MessageEmbed{
		Title:       "🎉 ¡Nuevo miembro en KidStorePeru!",
		Description: fmt.Sprintf("**%s** se acaba de unir a la comunidad.%s", customer.EpicUsername, memberNote),
		Color:       colorAccent,
		Thumbnail:   &discordgo.MessageEmbedThumbnail{URL: logoURL},
	}
	go sendChannelEmbed(cfg.DiscordWelcomeChannelID, channelEmbed)

	if customer.DiscordID != nil {
		dmEmbed := &discordgo.MessageEmbed{
			Title:       "👋 ¡Bienvenido a KidStorePeru!",
			Description: fmt.Sprintf("Hola **%s**, gracias por unirte a la mejor tienda de Fortnite en español. 🎮", customer.EpicUsername),
			Color:       colorAccent,
			Thumbnail:   &discordgo.MessageEmbedThumbnail{URL: logoURL},
			Fields: []*discordgo.MessageEmbedField{
				{Name: "💰 Recargar KidCoins", Value: fmt.Sprintf("[Recarga aquí](%s/recharge)", siteURL), Inline: true},
				{Name: "🛍️ Tienda de Fortnite", Value: fmt.Sprintf("[Ver la Tienda](%s/store)", siteURL), Inline: true},
				{Name: "📖 Comandos del bot", Value: "Usa `/ayuda` para ver todo lo que puedes hacer aquí en Discord.", Inline: false},
			},
		}
		go sendDM(*customer.DiscordID, dmEmbed)
	}
}

// NotifyRecharge se llama cada vez que se acredita KC a un cliente, sin
// importar el método (pasarela automática, manual por admin, manual por Discord).
func NotifyRecharge(customer types.Customer, amountKC int, newBalance int, source string) {
	if !Enabled() {
		return
	}
	embed := &discordgo.MessageEmbed{
		Title:     "💰 Recarga de KidCoins",
		Color:     colorSuccess,
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: kidcoinAttachment},
		Fields: []*discordgo.MessageEmbedField{
			{Name: "👤 Cliente", Value: customer.EpicUsername, Inline: true},
			{Name: "➕ KC agregados", Value: fmt.Sprintf("+%d KC", amountKC), Inline: true},
			{Name: "💳 Método", Value: source, Inline: true},
			{Name: "💼 Nuevo balance", Value: fmt.Sprintf("**%d KC**", newBalance), Inline: false},
		},
	}
	go sendChannelEmbed(cfg.DiscordRechargeChannelID, embed, kidcoinFile())
}

// NotifyFriendship48h se llama cuando un cliente cumple 48h de amistad con
// alguno de los bots (requisito de Epic para poder recibir regalos).
func NotifyFriendship48h(customer types.Customer) {
	if !Enabled() {
		return
	}
	embed := &discordgo.MessageEmbed{
		Title:       "⏳ ¡Ya puedes comprar!",
		Description: fmt.Sprintf("**%s** ya cumplió las 48 horas de amistad requeridas por Epic Games con uno de nuestros bots.\n\n✅ Ya puede comprar en la [Tienda](%s/store) y recibir sus regalos normalmente.", customer.EpicUsername, siteURL),
		Color:       colorSuccess,
		Thumbnail:   &discordgo.MessageEmbedThumbnail{URL: iconURL},
	}
	go sendChannelEmbed(cfg.DiscordFriend48hChannelID, embed)
}

// NotifyPurchase se llama cuando un pedido se entrega con éxito.
func NotifyPurchase(customer types.Customer, epicUsername, itemName string, itemImage *string, priceKC, priceVBucks int) {
	if !Enabled() {
		return
	}
	embed := &discordgo.MessageEmbed{
		Title: "🛒 ¡Compra entregada!",
		Color: colorGold,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "👤 Cliente", Value: customer.EpicUsername, Inline: true},
			{Name: "🎮 Cuenta Epic", Value: epicUsername, Inline: true},
			{Name: "🛍️ Producto", Value: itemName, Inline: false},
			{Name: "💰 Costo", Value: fmt.Sprintf("%d KC", priceKC), Inline: true},
		},
	}
	if priceVBucks > 0 {
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name: "🪙 V-Bucks", Value: fmt.Sprintf("%d", priceVBucks), Inline: true,
		})
	}
	if itemImage != nil && *itemImage != "" {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: *itemImage}
	} else {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: iconURL}
	}
	go sendChannelEmbed(cfg.DiscordPurchaseChannelID, embed)
}
