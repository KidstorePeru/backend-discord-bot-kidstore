package discordbot

import (
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// WishlistBackItem es un objeto de la lista de deseos que volvió a la tienda.
type WishlistBackItem struct {
	Name    string
	Image   string
	PriceKC int
	OutDate time.Time
}

var monthsES = [...]string{"ene.", "feb.", "mar.", "abr.", "may.", "jun.", "jul.", "ago.", "sep.", "oct.", "nov.", "dic."}
var monthsEN = [...]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

// NotifyWishlistBack le manda al cliente un DM con los objetos de su lista
// de deseos que volvieron a la tienda (uno solo, aunque sean varios).
func NotifyWishlistBack(discordUserID, lang string, items []WishlistBackItem) {
	if !Enabled() || discordUserID == "" || len(items) == 0 {
		return
	}
	sendDM(discordUserID, wishlistBackEmbed(lang, items))
}

func wishlistBackEmbed(lang string, items []WishlistBackItem) *discordgo.MessageEmbed {
	es := lang != "en"
	title, intro, link := "🔔 ¡Volvió a la tienda!", "Algo de tu lista de deseos está hoy en la tienda de Fortnite:", "Ver en la tienda"
	if len(items) > 1 {
		title = fmt.Sprintf("🔔 ¡Volvieron %d objetos de tu lista de deseos!", len(items))
		intro = "Esto de tu lista de deseos está hoy en la tienda de Fortnite:"
	}
	if !es {
		title, intro, link = "🔔 Back in the shop!", "Something from your wishlist is in today's Fortnite shop:", "View in the shop"
		if len(items) > 1 {
			title = fmt.Sprintf("🔔 %d items from your wishlist are back!", len(items))
			intro = "These items from your wishlist are in today's Fortnite shop:"
		}
	}
	var lines []string
	for i, it := range items {
		if i == 10 {
			if es {
				lines = append(lines, fmt.Sprintf("…y %d más", len(items)-10))
			} else {
				lines = append(lines, fmt.Sprintf("…and %d more", len(items)-10))
			}
			break
		}
		line := fmt.Sprintf("• **%s** — %d KC", escapeMarkdown(it.Name), it.PriceKC)
		if !it.OutDate.IsZero() {
			lima := it.OutDate.In(time.FixedZone("PET", -5*3600))
			if es {
				line += fmt.Sprintf(" (hasta el %d %s)", lima.Day(), monthsES[lima.Month()-1])
			} else {
				line += fmt.Sprintf(" (until %s %d)", monthsEN[lima.Month()-1], lima.Day())
			}
		}
		lines = append(lines, line)
	}
	embed := &discordgo.MessageEmbed{
		Title:       title,
		Description: fmt.Sprintf("%s\n\n%s\n\n[%s](%s/store)", intro, strings.Join(lines, "\n"), link, siteURL),
		Color:       colorAccent,
	}
	if items[0].Image != "" {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: items[0].Image}
	}
	return embed
}

// escapeMarkdown evita que un nombre con *, _ o ` rompa el formato del mensaje.
func escapeMarkdown(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `*`, `\*`, `_`, `\_`, "`", "\\`", `~`, `\~`, `|`, `\|`, `[`, `\[`, `]`, `\]`)
	return r.Replace(s)
}
