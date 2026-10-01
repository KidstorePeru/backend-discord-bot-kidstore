package discordbot

// Aviso al admin de un comprobante de pago manual nuevo, con botones para
// verlo, aprobarlo o rechazarlo directamente desde Discord.

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/bwmarrin/discordgo"
	"github.com/google/uuid"
)

// ManualPaymentAlert — datos del comprobante para el aviso.
type ManualPaymentAlert struct {
	ID           uuid.UUID
	Customer     string
	Email        string
	Package      string
	KC           int
	Amount       string
	Method       string
	Operation    string
	DuplicateOps int
	ViewURL      string
}

// ManualAction aprueba o rechaza una solicitud y devuelve el texto del
// resultado. reason solo se usa al rechazar.
type ManualAction func(id uuid.UUID, reason, reviewer string) (string, error)

var (
	manualActionsMu sync.RWMutex
	manualApprove   ManualAction
	manualReject    ManualAction
)

// SetManualPaymentActions conecta los botones con la lógica de la tienda
// (se pasa desde main.go para no crear un ciclo de paquetes).
func SetManualPaymentActions(approve, reject ManualAction) {
	manualActionsMu.Lock()
	manualApprove, manualReject = approve, reject
	manualActionsMu.Unlock()
}

const (
	mpApprovePrefix = "mp:ok:"
	mpRejectPrefix  = "mp:no:"
	mpModalPrefix   = "mp:reason:"
	mpReasonInput   = "reason"
)

// ErrManualReviewed lo usa la tienda para "ya revisada" (texto para el admin).
var ErrManualReviewed = errors.New("ya revisada")

func manualPaymentEmbed(a ManualPaymentAlert) *discordgo.MessageEmbed {
	fields := []*discordgo.MessageEmbedField{
		{Name: "👤 Cliente", Value: orDash(a.Customer), Inline: true},
		{Name: "💳 Método", Value: orDash(a.Method), Inline: true},
		{Name: "💰 Monto a verificar", Value: orDash(a.Amount), Inline: true},
		{Name: "🪙 KC a acreditar", Value: fmt.Sprintf("%d KC (%s)", a.KC, a.Package), Inline: false},
	}
	if a.Operation != "" {
		op := "`" + a.Operation + "`"
		if a.DuplicateOps > 0 {
			op += fmt.Sprintf("\n⚠️ Este número de operación ya se usó en %d solicitud(es): posible comprobante repetido.", a.DuplicateOps)
		}
		fields = append(fields, &discordgo.MessageEmbedField{Name: "🔢 N.º de operación", Value: op})
	}
	if a.Email != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "📧 Correo", Value: a.Email, Inline: true})
	}
	fields = append(fields, &discordgo.MessageEmbedField{Name: "🆔 Solicitud", Value: "`" + strings.ToUpper(a.ID.String()[:8]) + "`", Inline: true})
	color := colorWarnSoft
	if a.DuplicateOps > 0 {
		color = colorAlert
	}
	return &discordgo.MessageEmbed{
		Title:       "🧾 Nuevo comprobante de pago manual",
		Description: "Abre el comprobante, verifica que el pago llegó por el **monto exacto** y apruébalo o recházalo. El enlace vence en 24 h (siempre puedes verlo en el panel admin → Comprobantes).",
		Color:       color,
		Fields:      fields,
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func manualPaymentButtons(a ManualPaymentAlert) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "Ver comprobante", Style: discordgo.LinkButton, URL: a.ViewURL},
			discordgo.Button{Label: "Aprobar y acreditar", Style: discordgo.SuccessButton, CustomID: mpApprovePrefix + a.ID.String()},
			discordgo.Button{Label: "Rechazar", Style: discordgo.DangerButton, CustomID: mpRejectPrefix + a.ID.String()},
		}},
	}
}

// AlertManualPayment manda el aviso por mensaje privado al admin.
func AlertManualPayment(a ManualPaymentAlert) {
	if !Enabled() || session == nil || cfg.DiscordAdminUserID == "" {
		return
	}
	dm, err := session.UserChannelCreate(cfg.DiscordAdminUserID)
	if err != nil {
		slog.Warn("Discord: no se pudo abrir el DM del admin para un comprobante", "error", err)
		return
	}
	_, err = session.ChannelMessageSendComplex(dm.ID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{brand(manualPaymentEmbed(a))},
		Components: manualPaymentButtons(a),
	})
	if err != nil {
		slog.Warn("Discord: no se pudo enviar el aviso de comprobante", "error", err)
	}
}

// parseManualCustomID: "mp:ok:<uuid>" → ("ok", id).
func parseManualCustomID(customID string) (string, uuid.UUID, bool) {
	for _, p := range []struct{ prefix, action string }{{mpApprovePrefix, "ok"}, {mpRejectPrefix, "no"}, {mpModalPrefix, "reason"}} {
		if strings.HasPrefix(customID, p.prefix) {
			id, err := uuid.Parse(strings.TrimPrefix(customID, p.prefix))
			return p.action, id, err == nil
		}
	}
	return "", uuid.Nil, false
}

// handleManualPaymentInteraction atiende los botones y el formulario de
// motivo. Devuelve false si la interacción no es de comprobantes.
func handleManualPaymentInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) bool {
	var customID string
	switch i.Type {
	case discordgo.InteractionMessageComponent:
		customID = i.MessageComponentData().CustomID
	case discordgo.InteractionModalSubmit:
		customID = i.ModalSubmitData().CustomID
	default:
		return false
	}
	action, id, ok := parseManualCustomID(customID)
	if !ok {
		return false
	}
	user := interactionUser(i)
	if user == nil || !isAdmin(user.ID) {
		respondEphemeral(s, i, "⛔ Solo el administrador puede revisar comprobantes.")
		return true
	}
	manualActionsMu.RLock()
	approve, reject := manualApprove, manualReject
	manualActionsMu.RUnlock()
	if approve == nil || reject == nil {
		respondEphemeral(s, i, "⚠️ La revisión desde Discord no está disponible ahora. Usa el panel admin.")
		return true
	}
	reviewer := "Discord: " + user.Username

	switch action {
	case "ok":
		result, err := approve(id, "", reviewer)
		finishManualReview(s, i, result, err)
	case "no":
		// Pide el motivo (el cliente lo verá en la web y en su correo).
		err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseModal,
			Data: &discordgo.InteractionResponseData{
				CustomID: mpModalPrefix + id.String(),
				Title:    "Rechazar comprobante",
				Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					discordgo.TextInput{
						CustomID: mpReasonInput, Label: "Motivo (lo verá el cliente)", Style: discordgo.TextInputParagraph,
						Value: "No encontramos el pago con estos datos.", Required: true, MaxLength: 300,
					},
				}}},
			},
		})
		if err != nil {
			slog.Warn("Discord: no se pudo abrir el formulario de rechazo", "error", err)
		}
	case "reason":
		reason := ""
		for _, row := range i.ModalSubmitData().Components {
			if r, ok := row.(*discordgo.ActionsRow); ok {
				for _, c := range r.Components {
					if ti, ok := c.(*discordgo.TextInput); ok && ti.CustomID == mpReasonInput {
						reason = ti.Value
					}
				}
			}
		}
		result, err := reject(id, reason, reviewer)
		finishManualReview(s, i, result, err)
	}
	return true
}

// finishManualReview actualiza el aviso original: sin botones de acción y con
// el resultado, para que no se vuelva a revisar por error.
func finishManualReview(s *discordgo.Session, i *discordgo.InteractionCreate, result string, err error) {
	if err != nil {
		msg := "⚠️ No se pudo completar: " + err.Error()
		if errors.Is(err, ErrManualReviewed) || strings.Contains(err.Error(), "ya fue revisada") {
			msg = "ℹ️ Este comprobante ya fue revisado (quizás desde el panel)."
		}
		respondEphemeral(s, i, msg)
		return
	}
	var embeds []*discordgo.MessageEmbed
	var keep []discordgo.MessageComponent
	if i.Message != nil {
		embeds = i.Message.Embeds
		// Se conserva solo el botón "Ver comprobante".
		for _, row := range i.Message.Components {
			if r, ok := row.(*discordgo.ActionsRow); ok {
				var links []discordgo.MessageComponent
				for _, c := range r.Components {
					if b, ok := c.(*discordgo.Button); ok && b.Style == discordgo.LinkButton {
						links = append(links, *b)
					}
				}
				if len(links) > 0 {
					keep = append(keep, discordgo.ActionsRow{Components: links})
				}
			}
		}
	}
	if len(embeds) > 0 {
		e := *embeds[0]
		e.Fields = append(append([]*discordgo.MessageEmbedField{}, e.Fields...), &discordgo.MessageEmbedField{Name: "Resultado", Value: result})
		if strings.HasPrefix(result, "✅") {
			e.Color = colorSuccess
		} else {
			e.Color = colorAlert
		}
		embeds = []*discordgo.MessageEmbed{&e}
	}
	if keep == nil {
		keep = []discordgo.MessageComponent{}
	}
	err = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{Embeds: embeds, Components: keep},
	})
	if err != nil {
		// Si no se pudo editar el mensaje, al menos se confirma por privado.
		respondEphemeral(s, i, result)
	}
}
