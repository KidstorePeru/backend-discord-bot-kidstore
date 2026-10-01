package discordbot

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/google/uuid"
)

// ==================== ALERTAS OPERATIVAS AL ADMIN ====================
//
// A diferencia de las notificaciones normales (bienvenida, compra, recarga),
// estas van por DM directo a DISCORD_ADMIN_USER_ID — son avisos de "algo
// necesita tu atención" (un bot se cayó, se quedó sin fondos, o ya no hay
// ninguno disponible para procesar pedidos). Antes de esto, la única forma
// de enterarse era revisar el panel admin manualmente.

const (
	colorAlert    = 0xEF4444 // rojo — algo dejó de funcionar
	colorWarnSoft = 0xF59E0B // ámbar — advertencia, todavía funciona
)

// alertState evita mandar el mismo aviso una y otra vez mientras la
// condición sigue activa — se manda una vez, y no se repite hasta que la
// condición se resuelva (y vuelva a ocurrir). Vive solo en memoria: un
// reinicio del servidor resetea los avisos ya mandados, lo cual está bien —
// peor es quedarse callado para siempre por un bug.
var (
	alertState   = map[string]bool{}
	alertStateMu sync.Mutex
	// Cooldown aparte para la alerta de "cero bots disponibles", que puede
	// dispararse una vez por cada pedido en cola durante una caída — sin
	// esto, un solo bache de 10 pedidos mandaría 10 DMs idénticos seguidos.
	lastNoActiveBotsAlert time.Time
)

func shouldAlert(key string) bool {
	alertStateMu.Lock()
	defer alertStateMu.Unlock()
	if alertState[key] {
		return false
	}
	alertState[key] = true
	return true
}

func clearAlert(key string) {
	alertStateMu.Lock()
	defer alertStateMu.Unlock()
	delete(alertState, key)
}

func sendAdminAlert(title, description string, color int) {
	if !Enabled() || session == nil || cfg.DiscordAdminUserID == "" {
		return
	}
	embed := &discordgo.MessageEmbed{
		Title:       title,
		Description: description,
		Color:       color,
	}
	sendDM(cfg.DiscordAdminUserID, embed)
}

// AlertBotDeactivated se llama cuando una cuenta bot se marca inactiva (el
// health-check periódico no pudo refrescar su token varias veces seguidas,
// o falló al enviar un regalo por un error de autenticación). Es la señal
// más urgente: ese bot dejó de poder cumplir pedidos hasta que alguien
// vuelva a vincularlo.
func AlertBotDeactivated(botID uuid.UUID, displayName, reason string) {
	key := "deactivated_" + botID.String()
	if !shouldAlert(key) {
		return
	}
	sendAdminAlert(
		"🔴 Bot desconectado: "+displayName,
		fmt.Sprintf("La cuenta **%s** se marcó como inactiva y ya no puede enviar regalos.\n\n**Motivo:** %s\n\nVuelve a vincularla desde el panel admin (Cuentas Bot) cuando puedas.", displayName, reason),
		colorAlert,
	)
	slog.Info("Discord alert: bot desactivado", "bot", displayName)
}

// ClearBotDeactivatedAlert se llama cuando una cuenta vuelve a vincularse
// correctamente — así, si se vuelve a desactivar más adelante, el aviso se
// manda de nuevo en vez de quedar silenciado para siempre por la vez anterior.
func ClearBotDeactivatedAlert(botID uuid.UUID) {
	clearAlert("deactivated_" + botID.String())
}

// vbucksLowThreshold: por debajo de esto se avisa "quedan pocos pavos" (a
// tiempo de recargar antes de que llegue a 0). La mayoría de items de la
// tienda cuestan entre 300 y 3000 V-Bucks.
const vbucksLowThreshold = 500

// CheckVBucksAlert se llama tras sincronizar el balance real de una cuenta
// (ver healthcheck.go). Avisa una vez al cruzar por debajo del umbral, y
// limpia el aviso apenas vuelve a estar por encima (para poder avisar de
// nuevo si vuelve a bajar más adelante).
func CheckVBucksAlert(botID uuid.UUID, displayName string, vbucks int) {
	lowKey := "vbucks_low_" + botID.String()
	zeroKey := "vbucks_zero_" + botID.String()

	if vbucks <= 0 {
		if shouldAlert(zeroKey) {
			sendAdminAlert(
				"🔴 Bot sin V-Bucks: "+displayName,
				fmt.Sprintf("**%s** se quedó en **0 V-Bucks**. No puede enviar ningún regalo pagado hasta que le cargues más.\n\nLos pedidos que le toquen automáticamente pasan a probar otro bot con fondos — no se cancelan por esto solo. Si **ningún** bot tiene fondos suficientes para un pedido, recién ahí te avisamos aparte y el pedido queda pendiente (nunca se cancela, se reintenta solo apenas recargues a alguno). De todos modos, mejor evitarlo cargándole pavos ahora.", displayName),
				colorAlert,
			)
			slog.Info("Discord alert: bot sin V-Bucks", "bot", displayName)
		}
		return
	}

	// Se recuperó de 0 — permitir que la alerta crítica vuelva a dispararse
	// si en el futuro llega a 0 de nuevo.
	clearAlert(zeroKey)

	if vbucks < vbucksLowThreshold {
		if shouldAlert(lowKey) {
			sendAdminAlert(
				"🟡 Pocos V-Bucks: "+displayName,
				fmt.Sprintf("**%s** tiene **%d V-Bucks** — por debajo de %d. Todavía puede cumplir pedidos baratos, pero conviene recargarle pronto.", displayName, vbucks, vbucksLowThreshold),
				colorWarnSoft,
			)
			slog.Info("Discord alert: bot con pocos V-Bucks", "bot", displayName, "vbucks", vbucks)
		}
		return
	}

	// Volvió a estar cómodo — permitir que el aviso de "pocos" se repita si
	// vuelve a bajar más adelante.
	clearAlert(lowKey)
}

// AlertNoGiftSlots avisa cuando una cuenta agota sus regalos del día. No es
// grave (se resetea solo al día siguiente) pero ayuda a entender por qué la
// capacidad bajó de golpe si hay varios pedidos pendientes.
func AlertNoGiftSlots(botID uuid.UUID, displayName string) {
	key := "no_slots_" + botID.String()
	if !shouldAlert(key) {
		return
	}
	sendAdminAlert(
		"🟡 Bot sin regalos disponibles: "+displayName,
		fmt.Sprintf("**%s** agotó sus regalos del día. Se resetea automáticamente mañana — no requiere que hagas nada, es solo un aviso informativo.", displayName),
		colorWarnSoft,
	)
}

// ClearNoGiftSlotsAlert se llama cuando se le restablecen los slots a una
// cuenta (reset diario), para que el aviso pueda repetirse si se agotan otra vez.
func ClearNoGiftSlotsAlert(botID uuid.UUID) {
	clearAlert("no_slots_" + botID.String())
}

// ClearAllNoGiftSlotsAlerts se llama después del reset diario masivo de
// gifts (ResetDailyGifts actualiza todas las cuentas en una sola consulta,
// sin devolver IDs individuales) — así el aviso de "sin regalos" puede
// volver a dispararse si alguna cuenta se queda sin slots otra vez más
// adelante, en vez de quedar silenciado para siempre tras la primera vez.
func ClearAllNoGiftSlotsAlerts() {
	alertStateMu.Lock()
	defer alertStateMu.Unlock()
	for key := range alertState {
		if strings.HasPrefix(key, "no_slots_") {
			delete(alertState, key)
		}
	}
}

// AlertOrderNeedsReview se llama cuando un pedido queda en 'review' —
// entrega incierta tras una caída durante el envío a Epic Games (ver
// MarkOrderSendAttempted en db.go). No se puede confirmar ni descartar la
// entrega automáticamente sin arriesgarse a duplicarla o a reembolsar dos
// veces, así que necesita que un admin lo revise a mano contra Epic Games.
func AlertOrderNeedsReview(orderID, epicUsername, itemName string) {
	key := "order_review_" + orderID
	if !shouldAlert(key) {
		return
	}
	sendAdminAlert(
		"🟡 Pedido necesita revisión manual",
		fmt.Sprintf("El pedido **%s** (%s → %s) quedó en revisión: no se pudo confirmar si un envío anterior llegó a completarse en Epic Games antes de una caída del proceso. Revísalo en el panel admin (pestaña Pedidos) y resuélvelo confirmando en Epic si el ítem llegó o no.", orderID, itemName, epicUsername),
		colorWarnSoft,
	)
}

// AlertDeliveryNotPersisted se llama cuando Epic Games CONFIRMÓ la entrega de
// un regalo pero no se pudo guardar en la base (pedido 'sent' + evidencia). El
// worker reintenta guardarlo sin volver a enviar el regalo; si el proceso se
// reinicia antes, el pedido termina en revisión manual. La evidencia completa
// queda en los logs ("entrega sin persistir") para resolverlo.
func AlertDeliveryNotPersisted(orderID, epicUsername, itemName string) {
	key := "order_delivery_unpersisted_" + orderID
	if !shouldAlert(key) {
		return
	}
	sendAdminAlert(
		"🔴 Entrega realizada sin registrar",
		fmt.Sprintf("El pedido **%s** (%s → %s) SE ENTREGÓ en Epic Games, pero no se pudo guardar en la base de datos. El worker reintenta guardarlo sin reenviar el regalo. No lo reembolses: si queda en revisión, la evidencia de Epic está en los logs del backend (busca \"entrega sin persistir\" y el ID del pedido).", orderID, itemName, epicUsername),
		colorAlert,
	)
}

// AlertEmailFailing avisa que el proveedor de correo está rechazando los envíos
// (dominio sin verificar en Resend, clave inválida, sin proveedor configurado…).
// Mientras dure, ningún cliente recibe correos de verificación, pagos, entregas
// ni reclamos. Como mucho un aviso por hora.
func AlertEmailFailing(reason, from string) {
	if !shouldAlert("email_failing_" + time.Now().UTC().Format("2006010215")) {
		return
	}
	sendAdminAlert(
		"📧 Los correos no se están enviando",
		fmt.Sprintf("El proveedor de correo rechazó un envío desde **%s**:\n> %s\n\nMientras no se solucione, los clientes NO reciben correos de verificación de cuenta, recuperación de contraseña, pagos, entregas ni reclamos. Revisa el dominio y la clave en Resend (RESEND_API_KEY) o la configuración SMTP. Si alguien no puede registrarse, puedes activar su cuenta a mano desde el panel admin → Clientes.", from, reason),
		colorAlert,
	)
}

// complaintNoun: "un reclamo" / "una queja" (y en mayúscula, "El reclamo" / "La queja").
func complaintNoun(kind string, definite bool) string {
	switch {
	case kind == "queja" && definite:
		return "La queja"
	case kind == "queja":
		return "una queja"
	case definite:
		return "El reclamo"
	default:
		return "un reclamo"
	}
}

// AlertNewComplaint avisa que entró un reclamo o queja en el Libro de
// Reclamaciones Virtual — hay un plazo legal de 15 días hábiles para
// responderlo. emailSent=false indica que al consumidor NO le llegó la copia
// por correo (no hay proveedor de correo configurado).
func AlertNewComplaint(reference, kind, deadline string, emailSent bool) {
	if !shouldAlert("complaint_new_" + reference) {
		return
	}
	desc := fmt.Sprintf("Entró %s en el Libro de Reclamaciones: **%s**.\nPlazo legal para responder: **15 días hábiles** — vence el **%s**.\nRespóndelo desde el panel admin → pestaña Reclamos.", complaintNoun(kind, false), reference, deadline)
	if !emailSent {
		desc += "\n⚠️ Al consumidor NO le llegó la copia por correo: no hay proveedor de correo configurado (RESEND_API_KEY o SMTP_HOST)."
	}
	sendAdminAlert("📋 Nuevo reclamo en el Libro de Reclamaciones", desc, colorWarnSoft)
}

// AlertComplaintDeadline recuerda responder un reclamo cuyo plazo está por
// vencer o ya venció. day (AAAA-MM-DD) hace que se avise como mucho una vez por
// día por reclamo.
func AlertComplaintDeadline(reference, kind, deadline string, businessDaysLeft int, day string) {
	if !shouldAlert("complaint_deadline_" + reference + "_" + day) {
		return
	}
	var title, when string
	color := colorWarnSoft
	switch {
	case businessDaysLeft < 0:
		title = "🔴 Reclamo VENCIDO sin responder"
		when = fmt.Sprintf("venció el **%s** (hace %d día(s) hábil(es))", deadline, -businessDaysLeft)
		color = colorAlert
	case businessDaysLeft == 0:
		title = "🔴 Reclamo vence HOY"
		when = fmt.Sprintf("vence **hoy** (%s)", deadline)
		color = colorAlert
	default:
		title = "🟡 Reclamo por vencer"
		when = fmt.Sprintf("vence el **%s** (quedan %d día(s) hábil(es))", deadline, businessDaysLeft)
	}
	sendAdminAlert(title, fmt.Sprintf("%s **%s** sigue sin respuesta y %s. Respóndelo desde el panel admin → pestaña Reclamos.", complaintNoun(kind, true), reference, when), color)
}

// AlertUnderpaidCryptoPayment se llama cuando NOWPayments reporta un pago
// como "partially_paid" — el cliente mandó menos cripto de lo esperado
// (comisión de red, o el precio de la cripto se movió justo en el momento
// del pago). Antes esto no se distinguía de cualquier otro pago pendiente:
// simplemente expiraba a los 30 minutos sin acreditar KC y sin que nadie de
// soporte se enterara para decidir si acreditar proporcional o contactar al
// cliente. Se alerta una sola vez por pago (no se repite si NOWPayments
// reenvía el mismo IPN).
func AlertUnderpaidCryptoPayment(paymentID int64, orderID string) {
	key := fmt.Sprintf("underpaid_%d", paymentID)
	if !shouldAlert(key) {
		return
	}
	sendAdminAlert(
		"🟡 Pago cripto incompleto",
		fmt.Sprintf("NOWPayments reportó el pago **%d** (pedido %s) como pagado parcialmente — el cliente no acreditó KC. Revisa el pago en el panel y decide si acreditar el monto proporcional o contactar al cliente.", paymentID, orderID),
		colorWarnSoft,
	)
}

// AlertUnresolvedPayment se llama cuando un pago con sesión real en una
// pasarela lleva demasiado tiempo (ver reconcileDeadLetterAfter en
// webhooks.go) sin que la pasarela confirme ni un cobro ni un rechazo — en
// vez de quedar "pending" para siempre (invisible, nadie se entera), pasa a
// 'review'. Esto NO es un rechazo ni una expiración: 'review' es
// explícitamente distinto de 'failed'/'expired' (ver el comentario grande
// en checkGatewayOutcome/webhooks.go) y GetStalePendingPayments lo sigue
// reconciliando automáticamente en cada pasada por si llega una
// confirmación tardía de la pasarela. La alerta es solo para que soporte
// tenga visibilidad manual mientras tanto (podría ser un problema con las
// credenciales de esa pasarela, no necesariamente un pago abandonado).
func AlertUnresolvedPayment(txID, gateway string) {
	key := "unresolved_payment_" + txID
	if !shouldAlert(key) {
		return
	}
	sendAdminAlert(
		"🟡 Pago sin resolver tras varias horas",
		fmt.Sprintf("El pago **%s** (%s) lleva más de 6 horas sin que la pasarela confirme ni un cobro ni un rechazo. Pasó a revisión ('review') — NO se marcó como expirado ni rechazado, y la reconciliación automática lo sigue reintentando por si llega una confirmación tardía. Si el cliente reclama, revísalo a mano en el panel de esa pasarela.", txID, gateway),
		colorWarnSoft,
	)
}

// AlertPaymentSessionUntracked avisa que se creó una sesión de cobro en una
// pasarela pero NO se pudo guardar su ID externo. Al cliente no se le
// entregó el checkout, así que no debería poder pagarla; el pago quedó en
// 'review' con el ID (si esa escritura funcionó) para que la conciliación lo
// siga consultando. Si esa segunda escritura también falló, el ID solo está
// en los logs del servidor (txID + externalID).
func AlertPaymentSessionUntracked(txID, gateway, externalID string) {
	key := "payment_untracked_" + txID
	if !shouldAlert(key) {
		return
	}
	sendAdminAlert(
		"🟠 Sesión de pago sin rastro local",
		fmt.Sprintf("Se creó una sesión en **%s** para el pago **%s** (ID externo: %s) pero no se pudo guardar ese ID. No se le entregó el checkout al cliente. Revisa el pago en el panel (estado 'review') y, si hace falta, la sesión en la pasarela.", gateway, txID, externalID),
		colorWarnSoft,
	)
}

// AlertWebhookNeedsReview avisa que un webhook agotó sus reintentos
// automáticos (o falló de forma no recuperable) y quedó en la bandeja de
// revisión manual de webhook_events. El evento se CONSERVA (con su
// payment_id) hasta que un admin lo reencole — el pago podría todavía
// cobrarse o necesitar conciliación. Una sola alerta por evento.
func AlertWebhookNeedsReview(gateway, eventID, detail string) {
	key := "webhook_review_" + eventID
	if !shouldAlert(key) {
		return
	}
	sendAdminAlert(
		"🟠 Webhook en revisión manual",
		fmt.Sprintf("Un webhook de **%s** (evento %s) no se pudo procesar automáticamente: %s. Quedó guardado en la bandeja de revisión (GET /admin/webhook-events/review); cuando se resuelva la causa, reencólalo con POST /admin/webhook-events/%s/retry.", gateway, eventID, detail, eventID),
		colorWarnSoft,
	)
}

// AlertNoActiveBots es la más urgente de todas: el worker de pedidos no
// encontró NINGÚN bot disponible para procesar la cola. Los pedidos quedan
// pendientes hasta que se resuelva. Tiene cooldown propio en vez de
// dispararse una vez y quedar callada, porque mientras el problema persista
// vale la pena que te recuerde cada cierto tiempo — pero sin saturar el DM
// con un mensaje por cada pedido en cola.
const noActiveBotsCooldown = 20 * time.Minute

func AlertNoActiveBots(pendingOrders int) {
	alertStateMu.Lock()
	elapsed := time.Since(lastNoActiveBotsAlert)
	if elapsed < noActiveBotsCooldown {
		alertStateMu.Unlock()
		return
	}
	lastNoActiveBotsAlert = time.Now()
	alertStateMu.Unlock()

	sendAdminAlert(
		"🔴 Sin bots disponibles para enviar regalos",
		fmt.Sprintf("No hay ninguna cuenta bot activa con capacidad para procesar pedidos ahora mismo. Hay **%d pedido(s)** esperando.\n\nRevisa el panel admin (Cuentas Bot): probablemente todas están desactivadas, sin V-Bucks, o sin regalos disponibles hoy.", pendingOrders),
		colorAlert,
	)
	slog.Info("Discord alert: sin bots activos", "pedidos_pendientes", pendingOrders)
}

// AlertBackupFailed avisa que el respaldo diario de la base de datos falló
// (como mucho una vez por día, hasta que vuelva a funcionar).
func AlertBackupFailed(reason string) {
	if !shouldAlert("backup_failed_" + time.Now().UTC().Format("20060102")) {
		return
	}
	sendAdminAlert(
		"💾 El respaldo de la base de datos falló",
		fmt.Sprintf("No se pudo guardar la copia de seguridad diaria:\n> %s\n\nLa tienda sigue funcionando normal, pero si se pierde la base de datos no habría una copia reciente de los saldos de KC, pedidos y cuentas. Revisa las variables BACKUP_* en Railway y el almacenamiento (bucket). Se reintenta cada hora.", reason),
		colorAlert,
	)
}

// ClearBackupFailedAlert — el respaldo volvió a funcionar: si falla de
// nuevo hoy, se vuelve a avisar.
func ClearBackupFailedAlert() {
	clearAlert("backup_failed_" + time.Now().UTC().Format("20060102"))
}

// AlertBackupsWorking confirma la primera copia guardada en el almacenamiento.
func AlertBackupsWorking(sizeBytes, rows, retentionDays int) {
	sendAdminAlert(
		"💾 Respaldos de la base de datos activados",
		fmt.Sprintf("Se guardó la primera copia de seguridad (%d filas, %s, cifrada). Desde ahora se guarda una copia por día y se conservan las de los últimos %d días.", rows, humanSize(sizeBytes), retentionDays),
		0x22C55E,
	)
}

// humanSize: 850 B, 46 KB, 1.2 MB ("0.0 MB" no le dice nada a nadie).
func humanSize(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%d KB", (n+512)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

// AlertNewReview avisa al admin que hay una reseña nueva para revisar en el
// panel (no se publica hasta que la apruebe).
func AlertNewReview(rating int, itemName, displayName, comment string) {
	stars := strings.Repeat("★", rating) + strings.Repeat("☆", 5-rating)
	text := comment
	if r := []rune(text); len(r) > 300 {
		text = string(r[:300]) + "…"
	}
	if text == "" {
		text = "(sin comentario)"
	}
	color := colorSuccess
	if rating <= 3 {
		color = colorWarnSoft
	}
	sendAdminAlert(
		"⭐ Nueva reseña para revisar",
		fmt.Sprintf("%s — **%s** sobre *%s*:\n> %s\n\nApruébala o recházala en el panel admin → Reseñas. Responder también las negativas genera confianza.", stars, escapeMarkdown(displayName), escapeMarkdown(itemName), escapeMarkdown(text)),
		color,
	)
}
