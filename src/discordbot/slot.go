package discordbot

import (
	"KidStoreStore/src/db"
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"

	"github.com/bwmarrin/discordgo"
)

const (
	minBet           = 10
	maxBet           = 10000
	winChancePercent = 3 // 3% de probabilidad de ganar
	payoutMultiplier = 2 // paga 2x lo apostado
)

var slotSymbols = []string{"🍒", "🍋", "🍇", "🔔", "⭐"}

const responsiblePlayFooter = "🔞 Juega con responsabilidad — es un juego de azar con KidCoins"

// rollWin decide si la tirada gana, usando crypto/rand para que el resultado
// no sea predecible ni manipulable por el cliente.
func rollWin() bool {
	n, err := rand.Int(rand.Reader, big.NewInt(100))
	if err != nil {
		return false // ante cualquier duda, no gana
	}
	return n.Int64() < winChancePercent
}

func randomSymbol() string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(slotSymbols))))
	if err != nil {
		return slotSymbols[0]
	}
	return slotSymbols[n.Int64()]
}

func handleSlotCommand(s *discordgo.Session, i *discordgo.InteractionCreate, data discordgo.ApplicationCommandInteractionData) {
	var amount int64
	for _, opt := range data.Options {
		if opt.Name == "amount" {
			amount = opt.IntValue()
		}
	}

	// Responder rápido con un "pensando..." — la consulta a la base de datos
	// puede tardar más de los 3 segundos que Discord da para responder directo.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		slog.Error("Discord bot: error confirmando interacción /slot", "error", err)
		return
	}

	respond := func(embed *discordgo.MessageEmbed) {
		brand(embed)
		embed.Footer = &discordgo.MessageEmbedFooter{Text: responsiblePlayFooter, IconURL: iconURL}
		content := ""
		edit := &discordgo.WebhookEdit{
			Content: &content,
			Embeds:  &[]*discordgo.MessageEmbed{embed},
		}
		if embed.Thumbnail != nil && embed.Thumbnail.URL == kidcoinAttachment {
			edit.Files = []*discordgo.File{kidcoinFile()}
		}
		if _, err := s.InteractionResponseEdit(i.Interaction, edit); err != nil {
			slog.Error("Discord bot: error respondiendo /slot", "error", err)
		}
	}

	var discordUserID string
	if u := interactionUser(i); u != nil {
		discordUserID = u.ID
	}

	customer, err := db.GetCustomerByDiscordID(database, discordUserID)
	if err != nil {
		respond(&discordgo.MessageEmbed{
			Title:       "❌ Cuenta no vinculada",
			Description: fmt.Sprintf("Vincula tu cuenta de Discord en [kidstoreperu.net](%s/account/security) (Mi Cuenta → Seguridad) antes de jugar.", siteURL),
			Color:       colorLose,
		})
		return
	}

	if amount < minBet || amount > maxBet {
		respond(&discordgo.MessageEmbed{
			Title:       "❌ Monto inválido",
			Description: fmt.Sprintf("Debes apostar entre **%d** y **%d** KC.", minBet, maxBet),
			Color:       colorLose,
		})
		return
	}

	won := rollWin()
	payout := 0
	if won {
		payout = int(amount) * payoutMultiplier
	}

	balanceBefore := customer.KCBalance
	newBalance, err := db.PlaceSlotBet(database, customer.ID, int(amount), payout, won)
	if err != nil {
		respond(&discordgo.MessageEmbed{
			Title:       "❌ No se pudo procesar la apuesta",
			Description: "Balance insuficiente o error interno. Verifica tu saldo con `/perfil`.",
			Color:       colorLose,
		})
		return
	}
	db.RecordSlotPlay(database, customer.ID, int(amount), won, payout)

	reels := fmt.Sprintf("%s ┃ %s ┃ %s", randomSymbol(), randomSymbol(), randomSymbol())
	title := "🎰 Sin suerte esta vez"
	color := colorLose
	resultLine := fmt.Sprintf("😢 Perdiste **%d KC**. ¡Inténtalo de nuevo!", amount)
	if won {
		reels = "⭐ ┃ ⭐ ┃ ⭐"
		title = "🎉 ¡¡JACKPOT!!"
		color = colorSuccess
		resultLine = fmt.Sprintf("🎉 ¡Ganaste **%d KC**! (apuesta x%d)", payout, payoutMultiplier)
	}

	embed := &discordgo.MessageEmbed{
		Title:       title,
		Description: fmt.Sprintf("```\n %s \n```\n%s", reels, resultLine),
		Color:       color,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "🎲 Apostado", Value: fmt.Sprintf("%d KC", amount), Inline: true},
			{Name: "💼 Balance anterior", Value: fmt.Sprintf("%d KC", balanceBefore), Inline: true},
			{Name: "✅ Balance nuevo", Value: fmt.Sprintf("**%d KC**", newBalance), Inline: true},
		},
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: kidcoinAttachment},
	}
	respond(embed)
}
