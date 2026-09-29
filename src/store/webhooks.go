package store

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"
	"KidStoreStore/src/safe"
	"KidStoreStore/src/types"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ==================== COMMON ====================

// ProcessApprovedPayment es la version exportada de processApprovedPayment —
// la usa el panel de admin (HandlerUpdatePayment) para que el botón
// "Aprobar" realmente acredite el KC al cliente (email, notificación de
// Discord, todo) en vez de solo cambiarle la etiqueta al pago.
func ProcessApprovedPayment(database *sql.DB, txID uuid.UUID) error {
	return processApprovedPayment(database, txID)
}

func processApprovedPayment(database *sql.DB, txID uuid.UUID) error {
	// Acreditación atómica: marcar el pago como aprobado, sumar el KC e
	// insertar el movimiento pasan los tres juntos en una sola transacción
	// (ver CreditPaymentOnce) — una caída a mitad de camino no puede dejar
	// un pago "aprobado" sin su KC acreditado, y la unicidad no depende del
	// status visible del pago (que un admin puede cambiar de ida y vuelta),
	// así que reintentos, webhooks duplicados o un pago reabierto y vuelto
	// a aprobar nunca acreditan el mismo KC dos veces.
	credited, tx, err := db.CreditPaymentOnce(database, txID)
	if err != nil {
		return fmt.Errorf("crediting payment: %w", err)
	}
	if !credited {
		return nil // ya se había acreditado antes (o alguien más lo está procesando ahora) — idempotente
	}
	slog.Info("KC credited via payment", "customer", tx.CustomerID, "kc", tx.KCAmount, "gateway", tx.Gateway)

	// Send payment approved email notification — chargedAmount/chargedCurrency
	// son el monto y la divisa REALMENTE cobrados (misma función y mismos
	// datos que usa el comprobante, ChargedAmountAndCurrency): antes este
	// correo mostraba siempre tx.AmountPEN como "S/ …" aunque la pasarela
	// hubiera cobrado en USD (PayPal/NOWPayments) o en la divisa real del
	// cliente (dLocal Go).
	if customer, err := db.GetCustomerByID(database, tx.CustomerID); err == nil {
		if customer.Email != nil && *customer.Email != "" {
			voucherURL := fmt.Sprintf("https://www.kidstoreperu.net/dashboard/comprobantes/pago/%s", tx.ID)
			chargedAmount, chargedCurrency := ChargedAmountAndCurrency(tx.Gateway, tx.AmountPEN, tx.AmountUSD, tx.AmountLocal, tx.CurrencyCode)
			go SendPaymentApprovedEmail(smtpConfig, *customer.Email, tx.ProductName, chargedAmount, chargedCurrency, tx.KCAmount, tx.Gateway, voucherURL, "es")
		}
		if tx.PaymentType == "kc_recharge" && tx.KCAmount > 0 {
			discordbot.NotifyRecharge(customer, tx.KCAmount, customer.KCBalance, tx.Gateway)
		}
	}

	return nil
}

// abandonedPaymentCreationAfter: margen antes de dar por abandonada la
// creación de un pago sin external_id (muy por encima de los timeouts de
// las pasarelas, 15 s).
const abandonedPaymentCreationAfter = 15 * time.Minute

// ReconcilePendingPayments es la red de seguridad para cuando un webhook
// nunca llega o se pierde en el camino — todos los webhooks de esta misma
// familia responden "received: true" de inmediato y procesan en una
// goroutine aparte (necesario para no bloquear la respuesta HTTP a la
// pasarela), así que si el proceso se cae, o simplemente la notificación
// nunca llega (problema de red del lado de la pasarela, por ejemplo), el
// pago queda "pending" sin que nada vuelva a intentarlo — antes de esto,
// solo MercadoPago tenía un mecanismo de repesca (el poll de
// HandlerPaymentStatus), y solo mientras el cliente seguía mirando la
// página. Se llama periódicamente desde main.go.
func ReconcilePendingPayments(database *sql.DB) {
	// Pagos que quedaron 'pending' SIN external_id (el proceso se cayó entre
	// guardar el registro local y crear/persistir la sesión — ver
	// HandlerCreatePayment): el cliente nunca recibió un checkout, así que se
	// cierran como 'failed'. GetStalePendingPayments los ignora a propósito
	// (exige external_id), por eso este barrido va aparte.
	if n, err := db.ExpireAbandonedPaymentCreations(database, abandonedPaymentCreationAfter); err != nil {
		slog.Error("reconciliación de pagos: error cerrando creaciones abandonadas", "error", err)
	} else if n > 0 {
		slog.Warn("reconciliación de pagos: creaciones abandonadas cerradas", "count", n)
	}

	stale, err := db.GetStalePendingPayments(database)
	if err != nil {
		slog.Error("reconciliación de pagos: error listando pendientes", "error", err)
		return
	}
	for _, p := range stale {
		p := p
		safe.Run("ReconcilePendingPayments."+p.Gateway, func() { reconcileOnePayment(database, p) })
	}
}

// gatewayOutcome resume lo que la pasarela dice REALMENTE sobre un pago —
// nunca se infiere ninguno de los dos extremos (aprobado/rechazado) de
// señales del lado del cliente (cerrar la ventana, un timeout, la URL de
// retorno): esas solo pueden llevar a gatewayStillPending.
type gatewayOutcome int

const (
	gatewayStillPending gatewayOutcome = iota
	gatewayApproved
	gatewayRejected
)

// classifyMercadoPagoStatus, classifyPayPalStatus, classifyDLocalGoStatus y
// classifyNOWPaymentsStatus son funciones puras (sin red ni base de datos)
// que traducen el status crudo que devuelve cada pasarela a un
// gatewayOutcome — separadas de checkGatewayOutcome para poder cubrir con
// tests unitarios simples la parte que de verdad importa: qué se considera
// "aprobado", qué se considera "rechazado de forma definitiva", y qué se
// trata como "todavía sin resolver" (nunca se infiere ninguno de los dos
// primeros de que el cliente haya cerrado la ventana de pago o de un
// timeout del lado del cliente — eso solo puede llegar acá como "sin
// resolver").
func classifyMercadoPagoStatus(status string) gatewayOutcome {
	switch status {
	case "approved":
		return gatewayApproved
	case "rejected", "cancelled":
		return gatewayRejected
	default: // "", "pending", "in_process", "authorized", etc.
		return gatewayStillPending
	}
}

func classifyPayPalStatus(status string) gatewayOutcome {
	switch status {
	case "COMPLETED":
		return gatewayApproved
	case "VOIDED":
		return gatewayRejected
	default: // CREATED, SAVED, APPROVED (todavía se puede capturar), PAYER_ACTION_REQUIRED, etc.
		return gatewayStillPending
	}
}

func classifyDLocalGoStatus(status string) gatewayOutcome {
	switch status {
	case "PAID":
		return gatewayApproved
	case "REJECTED", "CANCELLED", "EXPIRED":
		return gatewayRejected
	default: // PENDING
		return gatewayStillPending
	}
}

func classifyNOWPaymentsStatus(status string) gatewayOutcome {
	switch status {
	case "confirmed", "finished":
		return gatewayApproved
	case "failed", "expired", "refunded":
		return gatewayRejected
	default: // waiting, confirming, sending, partially_paid
		return gatewayStillPending
	}
}

// checkGatewayOutcome consulta directamente a la pasarela (con nuestras
// propias credenciales, nunca confiando en datos que vengan del cliente o de
// un webhook sin firmar) si un pago pendiente ya se resolvió. La usan tanto
// HandlerCancelPayment (cuando el cliente cierra la ventana o se agota el
// timeout) como reconcileOnePayment (el barrido automático) — MISMA fuente
// de verdad para las dos rutas, así nunca dan respuestas distintas sobre el
// mismo pago.
func checkGatewayOutcome(p types.PaymentTransaction) (gatewayOutcome, error) {
	switch p.Gateway {
	case "mercadopago":
		status, err := mercadoPagoPaymentStatus(p.ID.String())
		if err != nil {
			return gatewayStillPending, err
		}
		return classifyMercadoPagoStatus(status), nil
	case "paypal":
		if p.ExternalID == "" {
			return gatewayStillPending, nil
		}
		order, err := getPayPalOrder(p.ExternalID)
		if err != nil {
			return gatewayStillPending, err
		}
		outcome := classifyPayPalStatus(order.Status)
		if outcome == gatewayApproved && !payPalOrderMatchesTx(order, p.AmountUSD) {
			// La orden dice COMPLETED, pero la captura real y/o el importe
			// cobrado no coinciden con lo que esta transacción esperaba —
			// nunca se acredita a ciegas solo por el status de la orden (ver
			// payPalOrderMatchesTx). Se trata como "todavía sin resolver",
			// nunca como rechazo definitivo: puede ser un instante en que la
			// captura aún no propagó su status.
			slog.Warn("PayPal: orden COMPLETED pero no coincide con la transacción, no se acredita",
				"txID", p.ID, "orderID", p.ExternalID, "orderAmount", order.AmountValue,
				"orderCurrency", order.CurrencyCode, "captureStatus", order.CaptureStatus, "expectedUSD", p.AmountUSD)
			return gatewayStillPending, nil
		}
		return outcome, nil
	case "dlocalgo":
		if p.ExternalID == "" {
			return gatewayStillPending, nil
		}
		status, _, err := dlocalGoPaymentStatus(p.ExternalID)
		if err != nil {
			return gatewayStillPending, err
		}
		return classifyDLocalGoStatus(status), nil
	case "nowpayments":
		// external_id acá es el ID de la FACTURA (invoice), no el del pago —
		// para consultar el pago real hace falta provider_payment_id, que solo
		// se conoce cuando llega al menos un IPN (ver HandlerNOWPaymentsWebhook).
		// Sin él, no hay nada que consultar todavía: sigue "pendiente", ni
		// aprobado ni rechazado.
		if p.ProviderPaymentID == "" {
			return gatewayStillPending, nil
		}
		paymentID, err := strconv.ParseInt(p.ProviderPaymentID, 10, 64)
		if err != nil {
			return gatewayStillPending, fmt.Errorf("provider_payment_id inválido: %w", err)
		}
		status, orderID, err := nowPaymentsStatus(paymentID)
		if err != nil {
			return gatewayStillPending, err
		}
		if orderID != p.ID.String() {
			// No debería pasar nunca (provider_payment_id se guarda junto con
			// el propio orderID que NOWPayments devolvió) — si pasa, algo está
			// mal y es mejor no acreditar ni rechazar nada a ciegas.
			return gatewayStillPending, fmt.Errorf("nowpayments: order_id de la pasarela (%s) no coincide con la transacción (%s)", orderID, p.ID.String())
		}
		return classifyNOWPaymentsStatus(status), nil
	default:
		return gatewayStillPending, fmt.Errorf("pasarela desconocida: %s", p.Gateway)
	}
}

// reconcileDeadLetterAfter — si un pago con sesión real en la pasarela lleva
// todo este tiempo sin que la pasarela confirme nada (ni a favor ni en
// contra), pasa a 'review' (nunca a 'failed'/'expired' — eso afirmaría un
// rechazo confirmado que no existe). Deliberadamente generoso: una
// confirmación cripto (NOWPayments) puede tardar horas en la blockchain en
// momentos de congestión, mucho más que cualquier tarjeta o transferencia.
const reconcileDeadLetterAfter = 6 * time.Hour

func reconcileOnePayment(database *sql.DB, p types.PaymentTransaction) {
	outcome, err := checkGatewayOutcome(p)
	if err != nil {
		slog.Warn("reconciliación: consulta a la pasarela falló", "txID", p.ID, "gateway", p.Gateway, "error", err)
	}

	switch outcome {
	case gatewayApproved:
		// PayPal: si la orden está aprobada pero todavía no capturada, capturarla
		// primero es lo mismo que hace HandlerPayPalWebhook al recibir
		// CHECKOUT.ORDER.APPROVED — checkGatewayOutcome no la cuenta como
		// aprobada hasta que además está COMPLETED, así que llegar acá con
		// PayPal ya significa COMPLETED de verdad.
		if err := processApprovedPayment(database, p.ID); err != nil {
			slog.Error("reconciliación: error acreditando pago", "txID", p.ID, "gateway", p.Gateway, "error", err)
			// La pasarela YA confirmó el cobro — este intento falló por algo de
			// nuestro lado (p. ej. una cuenta inactiva o un error transitorio de
			// base de datos), no porque el pago no exista. No se marca 'failed'
			// ni 'review' (CreditPaymentOnce sigue garantizando que, cuando la
			// acreditación finalmente funcione, sea exactamente una vez), pero
			// SÍ hay que rotar updated_at: si no, este mismo pago vuelve a ser
			// el primero de GetStalePendingPayments en cada pasada y un solo
			// pago que falla sistemáticamente (cuenta inactiva, por ejemplo)
			// puede acaparar el barrido entero e impedir que se reintenten los
			// pagos siguientes.
			if terr := db.TouchPaymentReconciled(database, p.ID); terr != nil {
				slog.Warn("reconciliación: no se pudo actualizar la marca de rotación tras fallo de acreditación", "txID", p.ID, "error", terr)
			}
			return
		}
		slog.Info("reconciliación: pago acreditado sin depender del webhook", "txID", p.ID, "gateway", p.Gateway)
	case gatewayRejected:
		if err := db.AdminUpdatePaymentStatus(database, p.ID, "failed"); err != nil {
			slog.Error("reconciliación: error marcando pago rechazado", "txID", p.ID, "gateway", p.Gateway, "error", err)
			// Mismo motivo que en gatewayApproved: si esta escritura falla, no
			// dejar que el pago se quede clavado como primero de la cola.
			if terr := db.TouchPaymentReconciled(database, p.ID); terr != nil {
				slog.Warn("reconciliación: no se pudo actualizar la marca de rotación tras fallo al marcar rechazo", "txID", p.ID, "error", terr)
			}
			return
		}
		slog.Info("reconciliación: pago rechazado confirmado por la pasarela", "txID", p.ID, "gateway", p.Gateway)
	default: // gatewayStillPending — todavía nada resuelto, o PayPal APPROVED-sin-capturar
		if p.Gateway == "paypal" {
			// Intentar la captura mejora las chances de la próxima pasada, pero
			// nunca decide por sí sola el resultado (checkGatewayOutcome vuelve
			// a consultar la próxima vez).
			if order, gerr := getPayPalOrder(p.ExternalID); gerr == nil && order.Status == "APPROVED" {
				capturePayPalOrder(p.ExternalID)
			}
		}
		// Ver el paso del tiempo (o que esta consulta falle) NUNCA se trata
		// como prueba de que no hubo cobro — 'review' es explícitamente
		// distinto de 'failed'/'expired' (que sí afirman un rechazo
		// confirmado): el pago sigue en la reconciliación automática (ver
		// GetStalePendingPayments) por si llega una confirmación tardía, y
		// el frontend muestra un mensaje honesto de "no pudimos confirmar",
		// nunca "no se realizó ningún cargo". Solo se transiciona una vez
		// (status=='pending') — si ya está en 'review', no hay nada nuevo
		// que anunciar en cada pasada de 2 minutos, solo se sigue
		// reconciliando en silencio hasta que se resuelva de verdad.
		if p.Status == "pending" && time.Since(p.CreatedAt) > reconcileDeadLetterAfter {
			slog.Warn("reconciliación: pago nunca se pudo confirmar ni descartar, queda en revisión", "txID", p.ID, "gateway", p.Gateway, "edad", time.Since(p.CreatedAt))
			if err := db.AdminUpdatePaymentStatus(database, p.ID, "review"); err != nil {
				slog.Error("reconciliación: error marcando pago en revisión", "txID", p.ID, "error", err)
				return
			}
			discordbot.AlertUnresolvedPayment(p.ID.String(), p.Gateway)
			return
		}
		// Sigue sin resolverse (y todavía no pasó el margen para darlo por
		// perdido) — se actualiza updated_at para que GetStalePendingPayments
		// (que ordena por esa columna) lo empuje al final de la cola: la
		// próxima pasada del barrido atiende a otros pagos elegibles en vez
		// de volver a traer siempre los mismos primeros 100. Sin esto, un
		// grupo grande de pagos permanentemente ambiguos podía acaparar cada
		// pasada e impedir que se llegara a revisar cualquier pago más nuevo.
		if err := db.TouchPaymentReconciled(database, p.ID); err != nil {
			slog.Warn("reconciliación: no se pudo actualizar la marca de rotación", "txID", p.ID, "error", err)
		}
	}
}

// logWebhookEventOrReject registra el webhook de forma duradera ANTES de
// procesar nada — y, a diferencia de antes, si esa escritura falla, el
// handler NO confirma recepción (200) a la pasarela: responde con un error
// para que la pasarela reintente el envío más tarde. Confirmar recepción
// sin una garantía durable de que el evento quedó registrado significaba
// que, si el procesamiento en memoria también fallaba o el proceso se
// caía, el webhook se perdía sin dejar ningún rastro — ni para
// reintentarlo automáticamente, ni para diagnosticarlo después. Devuelve
// ok=false cuando ya se respondió al request y no hay que seguir.
func logWebhookEventOrReject(c *gin.Context, database *sql.DB, gateway, body string) (eventID uuid.UUID, ok bool) {
	eventID, err := db.LogWebhookEvent(database, gateway, body)
	if err != nil {
		slog.Error("webhook: no se pudo registrar el evento de forma duradera, se rechaza para que la pasarela reintente", "gateway", gateway, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"received": false, "error": "no se pudo registrar el evento, reintenta"})
		return uuid.Nil, false
	}
	return eventID, true
}

// ==================== LÍMITES Y NORMALIZACIÓN DE WEBHOOKS ====================

// maxWebhookBodyBytes acota cuánto se lee de un webhook. Las notificaciones
// reales de las pasarelas pesan unos cientos de bytes; el límite global de
// 4 MB de main.go es demasiado para un endpoint público (y, en dos de ellos,
// sin firma): cada cuerpo se leía completo a memoria, se escribía al log del
// proceso y se guardaba entero en webhook_events.
const maxWebhookBodyBytes = 64 << 10 // 64 KiB

// webhookRefPattern es el formato que aceptamos para los identificadores
// externos (payment_id, order_id) que viajan en un webhook. Los reales son
// alfanuméricos cortos; cualquier otra cosa (espacios, controles, cientos de
// caracteres) no es un identificador de pasarela.
var webhookRefPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// readWebhookBody lee el cuerpo del request con un tope duro y comprueba el
// error de lectura (antes MercadoPago/PayPal hacían "body, _ := io.ReadAll",
// así que un cuerpo truncado o cortado a mitad se procesaba como si fuera
// el evento completo). Si algo sale mal, ya responde al request y devuelve
// ok=false: 413 si excede el tope, 400 para cualquier otro fallo de lectura.
func readWebhookBody(c *gin.Context, gateway string) (body []byte, ok bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxWebhookBodyBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			slog.Warn("webhook: cuerpo demasiado grande, se rechaza", "gateway", gateway, "limit", maxWebhookBodyBytes)
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"received": false, "error": "cuerpo demasiado grande"})
			return nil, false
		}
		slog.Error("webhook: no se pudo leer el cuerpo completo, se rechaza para que reintente", "gateway", gateway, "error", err)
		c.JSON(http.StatusBadRequest, gin.H{"received": false, "error": "no se pudo leer el cuerpo del request"})
		return nil, false
	}
	return body, true
}

type webhookVerdict int

const (
	webhookAccept  webhookVerdict = iota // válido y relevante: se persiste (normalizado) y se procesa
	webhookIgnore                        // válido pero irrelevante: 200, no se persiste
	webhookInvalid                       // malformado: 400, no se persiste
)

// normalizeMercadoPagoEvent valida el cuerpo de un webhook de MercadoPago y
// devuelve solo lo necesario para procesarlo/reintentarlo: el tipo y el ID
// del pago. El cuerpo completo (que MercadoPago puede llenar con datos del
// comprador) nunca se guarda ni se loguea.
func normalizeMercadoPagoEvent(body []byte) (paymentID string, canonical []byte, verdict webhookVerdict) {
	var n struct {
		Type string `json:"type"`
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &n); err != nil {
		return "", nil, webhookInvalid
	}
	if n.Type != "payment" {
		return "", nil, webhookIgnore
	}
	if !webhookRefPattern.MatchString(n.Data.ID) {
		return "", nil, webhookInvalid
	}
	canonical, err := json.Marshal(n)
	if err != nil {
		return "", nil, webhookInvalid
	}
	return n.Data.ID, canonical, webhookAccept
}

// paypalWebhookEvent es la forma normalizada de un webhook de PayPal: solo
// el tipo de evento y los dos identificadores que el handler usa.
type paypalWebhookEvent struct {
	EventType string `json:"event_type"`
	Resource  struct {
		ID                string `json:"id"`
		SupplementaryData struct {
			RelatedIDs struct {
				OrderID string `json:"order_id,omitempty"`
			} `json:"related_ids"`
		} `json:"supplementary_data"`
	} `json:"resource"`
}

// normalizePayPalEvent valida un webhook de PayPal y lo reduce a los campos
// necesarios (tipo, resource.id y, para PAYMENT.CAPTURE.COMPLETED, el
// order_id relacionado). Los demás tipos de evento se ignoran sin persistir.
func normalizePayPalEvent(body []byte) (event paypalWebhookEvent, canonical []byte, verdict webhookVerdict) {
	var raw paypalWebhookEvent
	if err := json.Unmarshal(body, &raw); err != nil {
		return event, nil, webhookInvalid
	}
	switch raw.EventType {
	case "CHECKOUT.ORDER.APPROVED":
		if !webhookRefPattern.MatchString(raw.Resource.ID) {
			return event, nil, webhookInvalid
		}
		event.EventType = raw.EventType
		event.Resource.ID = raw.Resource.ID
	case "PAYMENT.CAPTURE.COMPLETED":
		orderID := raw.Resource.SupplementaryData.RelatedIDs.OrderID
		if !webhookRefPattern.MatchString(raw.Resource.ID) || !webhookRefPattern.MatchString(orderID) {
			return event, nil, webhookInvalid
		}
		event.EventType = raw.EventType
		event.Resource.ID = raw.Resource.ID
		event.Resource.SupplementaryData.RelatedIDs.OrderID = orderID
	default:
		return event, nil, webhookIgnore
	}
	canonical, err := json.Marshal(event)
	if err != nil {
		return event, nil, webhookInvalid
	}
	return event, canonical, webhookAccept
}

// respondToUnacceptedWebhook contesta a los veredictos que no se persisten:
// malformado → 400 (la pasarela verá el error), irrelevante → 200. Devuelve
// true si ya respondió (el handler debe cortar).
func respondToUnacceptedWebhook(c *gin.Context, gateway string, v webhookVerdict) bool {
	switch v {
	case webhookInvalid:
		slog.Warn("webhook: cuerpo inválido, se rechaza sin guardarlo", "gateway", gateway)
		c.JSON(http.StatusBadRequest, gin.H{"received": false, "error": "evento inválido"})
		return true
	case webhookIgnore:
		c.JSON(http.StatusOK, gin.H{"received": true})
		return true
	}
	return false
}

// normalizedPaymentIDEvent arma el registro mínimo {"payment_id":...} de un
// webhook YA autenticado por firma (NOWPayments, dLocal Go): es lo único que
// RetryFailedWebhookEvents necesita para reprocesarlo.
func normalizedPaymentIDEvent(paymentID any) []byte {
	b, _ := json.Marshal(map[string]any{"payment_id": paymentID})
	return b
}

// PurgeProcessedWebhookEvents borra de webhook_events los eventos ya
// procesados que pasaron el período de retención. Nunca toca un evento sin
// resolver (processed_at NULL) ni uno de NOWPayments cuyo resultado empieza
// con "error:" — esos los sigue reintentando RetryFailedWebhookEvents. Se
// llama periódicamente desde main.go.
func PurgeProcessedWebhookEvents(database *sql.DB) {
	n, err := db.PurgeProcessedWebhookEvents(database, webhookEventRetention)
	if err != nil {
		slog.Error("PurgeProcessedWebhookEvents: no se pudo purgar webhook_events", "error", err)
		return
	}
	if n > 0 {
		slog.Info("PurgeProcessedWebhookEvents: eventos procesados purgados", "count", n)
	}
}

// webhookEventRetention es cuánto se conserva un webhook_event ya resuelto
// (útil para diagnosticar y para auditoría reciente) antes de purgarse.
const webhookEventRetention = 30 * 24 * time.Hour

// ==================== MERCADOPAGO WEBHOOK ====================

func HandlerMercadoPagoWebhook(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := readWebhookBody(c, "mercadopago")
		if !ok { return }
		// 1) AUTENTICAR primero (x-signature, HMAC-SHA256): nada se guarda,
		// loguea ni procesa antes de saber que el evento viene de Mercado Pago.
		if verr := verifyMercadoPagoWebhook(c.Request); verr != nil {
			rejectUnauthenticatedWebhook(c, database, "mercadopago", body, verr)
			return
		}
		// 2) Validar y normalizar ANTES de guardar o loguear: nunca se escribe
		// el cuerpo crudo (solo el tipo y el ID del pago).
		paymentID, canonical, verdict := normalizeMercadoPagoEvent(body)
		if respondToUnacceptedWebhook(c, "mercadopago", verdict) { return }
		// El data.id de la URL es el que se firmó: tiene que coincidir con el
		// del cuerpo, o una firma legítima podría reutilizarse con otro pago.
		if q := c.Query("data.id"); q != "" && !strings.EqualFold(q, paymentID) {
			respondToUnacceptedWebhook(c, "mercadopago", webhookInvalid)
			return
		}
		slog.Info("MercadoPago webhook received", "paymentID", paymentID)
		// Registro duradero ANTES de intentar procesar nada — si el proceso
		// se cae a mitad de camino, queda constancia de que la notificación
		// sí llegó (ver ReconcilePendingPayments para la recuperación real).
		eventID, ok := logWebhookEventOrReject(c, database, "mercadopago", string(canonical))
		if !ok { return }

		// Query MercadoPago API for payment details
		go safe.Run("HandlerMercadoPagoWebhook", func() {
			outcome := "ignored: not approved"
			defer func() { db.MarkWebhookEventProcessed(database, eventID, outcome) }()

			// paymentID viene tal cual de un webhook público sin firmar —
			// cualquiera puede mandar este POST con lo que quiera en "data.id".
			// Si se interpolara crudo en la URL, un valor con caracteres raros
			// podría hacer que http.NewRequest fallara y dejara req en nil —
			// eso, sin chequear el error, tumbaba el proceso entero (sin
			// necesitar ninguna autenticación). url.PathEscape más el chequeo
			// de error cierran las dos puntas del problema.
			req, err := http.NewRequest("GET",
				"https://api.mercadopago.com/v1/payments/"+url.PathEscape(paymentID), nil)
			if err != nil {
				slog.Error("MP webhook: paymentID inválido, ignorando", "paymentID", paymentID, "error", err)
				outcome = "error: invalid paymentID"
				return
			}
			req.Header.Set("Authorization", "Bearer "+paymentCfg.MercadoPagoToken)

			resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
			if err != nil {
				slog.Error("MP payment query failed", "error", err)
				outcome = "error: MP query failed"
				return
			}
			defer resp.Body.Close()

			var payment struct {
				Status            string `json:"status"`
				ExternalReference string `json:"external_reference"`
			}
			json.NewDecoder(resp.Body).Decode(&payment)

			if payment.Status != "approved" {
				return
			}

			txID, err := uuid.Parse(payment.ExternalReference)
			if err != nil {
				slog.Error("MP invalid external_reference", "ref", payment.ExternalReference)
				outcome = "error: invalid external_reference"
				return
			}

			if err := processApprovedPayment(database, txID); err != nil {
				slog.Error("MP payment processing failed", "txID", txID, "error", err)
				outcome = "error: " + err.Error()
			} else {
				outcome = "processed"
			}
		})

		c.JSON(http.StatusOK, gin.H{"received": true})
	}
}

// ==================== PAYPAL WEBHOOK ====================

func HandlerPayPalWebhook(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := readWebhookBody(c, "paypal")
		if !ok { return }
		// Autenticar primero con la verificación oficial de PayPal
		// (verify-webhook-signature): nada se guarda ni procesa antes.
		if verr := verifyPayPalWebhook(c.Request.Header, body); verr != nil {
			rejectUnauthenticatedWebhook(c, database, "paypal", body, verr)
			return
		}
		// Validar y normalizar ANTES de guardar o loguear: solo se conservan
		// el tipo de evento y los IDs necesarios, nunca el cuerpo crudo (que
		// PayPal llena con datos del pagador).
		event, canonical, verdict := normalizePayPalEvent(body)
		if respondToUnacceptedWebhook(c, "paypal", verdict) { return }
		slog.Info("PayPal webhook received", "eventType", event.EventType, "resourceID", event.Resource.ID)
		eventID, ok := logWebhookEventOrReject(c, database, "paypal", string(canonical))
		if !ok { return }

		go safe.Run("HandlerPayPalWebhook", func() {
			outcome := "ignored: not completed"
			defer func() { db.MarkWebhookEventProcessed(database, eventID, outcome) }()

			orderID := event.Resource.ID
			if event.EventType == "CHECKOUT.ORDER.APPROVED" {
				// Capturar el pago — esto ya de por sí solo funciona si la orden
				// es real y aprobada, PayPal la rechaza si no.
				if err := capturePayPalOrder(orderID); err != nil {
					slog.Error("PayPal capture failed", "orderID", orderID, "error", err)
					outcome = "error: capture failed"
					return
				}
			} else {
				// PAYMENT.CAPTURE.COMPLETED: resource.id es el ID del capture, no
				// el de la orden — el order_id real viene en supplementary_data.
				orderID = event.Resource.SupplementaryData.RelatedIDs.OrderID
			}
			if orderID == "" {
				slog.Warn("PayPal webhook sin order_id resoluble")
				outcome = "error: no order_id"
				return
			}

			// El cuerpo del webhook no viene firmado — cualquiera podría forjar
			// este POST. Nunca se confía en su contenido: se vuelve a consultar el
			// estado real de la orden directamente en la API de PayPal con
			// nuestras propias credenciales, igual que ya se hace con MercadoPago
			// y dLocal Go. Solo esa respuesta decide si se acredita KC.
			order, err := getPayPalOrder(orderID)
			if err != nil {
				slog.Error("PayPal order query failed", "orderID", orderID, "error", err)
				outcome = "error: order query failed"
				return
			}
			if order.Status != "COMPLETED" {
				return
			}
			if order.ReferenceID == "" {
				slog.Warn("PayPal order sin reference_id", "orderID", orderID)
				outcome = "error: no reference_id"
				return
			}

			txID, err := uuid.Parse(order.ReferenceID)
			if err != nil {
				slog.Error("PayPal invalid reference_id", "ref", order.ReferenceID)
				outcome = "error: invalid reference_id"
				return
			}

			// El status COMPLETED de la orden no basta por sí solo — hace falta
			// además que la transacción exista, que esta orden sea de verdad la
			// que se creó para ella (ExternalID coincide) y que la captura real
			// y el importe/divisa cobrados coincidan (ver payPalOrderMatchesTx).
			tx, err := db.GetPaymentTransaction(database, txID)
			if err != nil {
				slog.Error("PayPal webhook: transacción no encontrada", "txID", txID, "error", err)
				outcome = "error: transaction not found"
				return
			}
			if tx.ExternalID != orderID || !payPalOrderMatchesTx(order, tx.AmountUSD) {
				slog.Warn("PayPal webhook: la orden no corresponde a la transacción esperada, no se acredita",
					"txID", txID, "orderID", orderID, "txExternalID", tx.ExternalID,
					"orderAmount", order.AmountValue, "orderCurrency", order.CurrencyCode,
					"captureStatus", order.CaptureStatus, "expectedUSD", tx.AmountUSD)
				outcome = "ignored: amount or reference mismatch"
				return
			}

			if err := processApprovedPayment(database, txID); err != nil {
				slog.Error("PayPal payment processing failed", "txID", txID, "error", err)
				outcome = "error: " + err.Error()
			} else {
				outcome = "processed"
			}
		})

		c.JSON(http.StatusOK, gin.H{"received": true})
	}
}

// ==================== PAYPAL RETURN (capture on redirect) ====================

func HandlerPayPalCapture(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		paypalToken := c.Query("token") // PayPal order ID
		if paypalToken == "" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "token invalido"})
			return
		}

		if err := capturePayPalOrder(paypalToken); err != nil {
			slog.Error("PayPal capture on return failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error capturando pago"})
			return
		}

		// Nunca se confía en el "id" que venga en la URL — cualquiera podría
		// alterarlo manualmente para acreditar una transacción distinta a la que
		// realmente pagó. El txID a acreditar siempre sale del reference_id que la
		// propia orden de PayPal tiene guardado desde que se creó, verificado
		// directamente contra la API de PayPal.
		order, err := getPayPalOrder(paypalToken)
		if err != nil {
			slog.Error("PayPal order query on return failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error verificando pago"})
			return
		}
		if order.Status != "COMPLETED" {
			c.JSON(http.StatusOK, gin.H{"success": false, "error": "pago aun no completado"})
			return
		}

		txID, err := uuid.Parse(order.ReferenceID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "orden sin referencia valida"})
			return
		}

		// Igual que en el webhook: el status COMPLETED de la orden no basta —
		// se exige además que la transacción exista, que esta orden sea
		// realmente la suya (ExternalID coincide) y que la captura/importe/
		// divisa cobrados coincidan (ver payPalOrderMatchesTx). Sin esto, el
		// parámetro ?token= de la URL de retorno (visible y manipulable por
		// el propio cliente) podría usarse para intentar acreditar cualquier
		// orden ajena que resultara tener status COMPLETED.
		tx, err := db.GetPaymentTransaction(database, txID)
		if err != nil {
			slog.Error("PayPal return: transacción no encontrada", "txID", txID, "error", err)
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "transacción no encontrada"})
			return
		}
		if tx.ExternalID != paypalToken || !payPalOrderMatchesTx(order, tx.AmountUSD) {
			slog.Warn("PayPal return: la orden no corresponde a la transacción esperada, no se acredita",
				"txID", txID, "orderID", paypalToken, "txExternalID", tx.ExternalID,
				"orderAmount", order.AmountValue, "orderCurrency", order.CurrencyCode,
				"captureStatus", order.CaptureStatus, "expectedUSD", tx.AmountUSD)
			c.JSON(http.StatusOK, gin.H{"success": false, "error": "no se pudo verificar el pago, contacta soporte"})
			return
		}

		if err := processApprovedPayment(database, txID); err != nil {
			slog.Error("PayPal return processing failed", "txID", txID, "error", err)
			// Ruta publica sin autenticar — no devolver el error crudo.
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error procesando el pago, contacta soporte"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "message": "pago procesado"})
	}
}

// ==================== NOWPAYMENTS WEBHOOK (IPN) ====================

func HandlerNOWPaymentsWebhook(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := readWebhookBody(c, "nowpayments")
		if !ok { return }
		slog.Info("NOWPayments webhook received", "bytes", len(body))

		// La firma (x-nowpayments-sig, HMAC-SHA512 sobre el cuerpo con sus
		// claves ordenadas — ver verifyNOWPaymentsSignature) se valida ANTES
		// de registrar el evento como uno "a procesar". Una notificación sin
		// firma válida no viene de NOWPayments — cualquiera podría forjar
		// este POST — así que nunca debe poder convertirse en un trabajo de
		// recuperación: RetryFailedWebhookEvents reprocesa TODO evento
		// "nowpayments" sin resolver sin volver a exigir firma, así que
		// dejar uno de estos a medias (o tratarlo igual que un evento
		// legítimo) terminaría acreditando lo que diga un payment_id ajeno o
		// inventado en cuanto pasara el margen de reintento.
		sigHeader := c.GetHeader("x-nowpayments-sig")
		if !verifyNOWPaymentsSignature(body, sigHeader) {
			slog.Warn("NOWPayments webhook: firma inválida, se rechaza sin crear un trabajo de recuperación")
			// Una sola escritura atómica (ya resuelta desde el INSERT) — nunca
			// LogWebhookEvent + MarkWebhookEventProcessed por separado. Con dos
			// escrituras, una caída del proceso entre medio (o que la segunda
			// falle) deja el evento con processed_at NULL, que es justo la
			// condición que GetUnresolvedWebhookEvents usa para decidir qué
			// reintentar — RetryFailedWebhookEvents reprocesaría entonces un
			// evento con firma inválida como si fuera legítimo. Ver
			// LogRejectedWebhookEvent (db.go).
			if err := logRejectedWebhook(database, "nowpayments", "invalid signature", body); err != nil {
				slog.Error("NOWPayments webhook: no se pudo registrar el rechazo por firma inválida", "error", err)
			}
			// 401, no 200: si esto fuera una notificación real de NOWPayments
			// que falló por un secreto mal configurado de nuestro lado, su
			// propio mecanismo de reintentos (ver "Recurrent payment
			// notifications" en la documentación oficial) seguirá reintentando
			// la entrega — mejor eso que responder 200 y perder la
			// notificación hasta el aviso de 6h de ReconcilePendingPayments.
			c.JSON(http.StatusUnauthorized, gin.H{"received": false, "error": "firma inválida"})
			return
		}

		// Firma válida, pero se comprueba que el cuerpo traiga un payment_id
		// ANTES de persistir nada — y lo único que se guarda es ese
		// payment_id (lo único que RetryFailedWebhookEvents necesita), no el
		// IPN completo con datos de la transacción.
		paymentID, valid := parseNOWPaymentsPaymentID(body)
		if !valid {
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}
		eventID, logged := logWebhookEventOrReject(c, database, "nowpayments", string(normalizedPaymentIDEvent(paymentID)))
		if !logged { return }

		go safe.Run("HandlerNOWPaymentsWebhook", func() {
			if claimed, err := db.ClaimWebhookEvent(database, eventID, webhookProcessingLease); err != nil || !claimed {
				// Otro trabajador lo tiene, o no se pudo reclamar: el reintento
				// periódico lo recogerá si sigue sin resolver.
				return
			}
			outcome := processNOWPaymentsPaymentID(database, paymentID)
			settleNOWPaymentsEvent(database, eventID, paymentID, outcome)
		})

		c.JSON(http.StatusOK, gin.H{"received": true})
	}
}

// parseNOWPaymentsPaymentID extrae únicamente el payment_id del cuerpo
// crudo del IPN — lo único que se necesita para consultar el estado real,
// ya que el resto del cuerpo (payment_status, order_id) no viene firmado y
// nunca se usa sin antes verificarlo contra la API de NOWPayments.
func parseNOWPaymentsPaymentID(body []byte) (int64, bool) {
	var notification struct {
		PaymentID int64 `json:"payment_id"`
	}
	if err := json.Unmarshal(body, &notification); err != nil || notification.PaymentID == 0 {
		return 0, false
	}
	return notification.PaymentID, true
}

// processNOWPaymentsPaymentID hace el trabajo real de un IPN de NOWPayments
// — consulta el estado verdadero del pago y, si corresponde, acredita el
// KC. Se extrajo del handler del webhook para que RetryFailedWebhookEvents
// pueda reejecutar EXACTAMENTE la misma lógica sobre un evento guardado que
// nunca se terminó de procesar (la consulta falló, o el proceso se cayó a
// mitad de camino) — sin depender de que NOWPayments reenvíe el IPN.
// Devuelve un outcome corto y legible para guardar en webhook_events.
func processNOWPaymentsPaymentID(database *sql.DB, paymentID int64) string {
	// El IPN no viene firmado — no se le puede creer su "payment_status" ni
	// su "order_id" a ciegas, cualquiera podría forjar este POST. Se vuelve
	// a consultar el estado real directamente en la API de NOWPayments con
	// nuestra propia API key, igual que ya se hace con MercadoPago.
	status, orderID, err := nowPaymentsStatus(paymentID)
	if err != nil {
		slog.Error("NOWPayments status query failed", "paymentID", paymentID, "error", err)
		return "error: status query failed"
	}

	txID, parseErr := uuid.Parse(orderID)
	if parseErr != nil {
		slog.Error("NOWPayments invalid order_id", "id", orderID)
		return "error: invalid order_id"
	}

	// Guardar el ID real del pago ANTES de decidir qué hacer con su
	// estado — así, aunque el proceso se caiga justo después de esta
	// línea, la reconciliación automática (cada 2 min, ver
	// reconcileOnePayment) puede seguir consultando el pago real sin
	// depender de que este webhook se repita (algo que este gateway no
	// podía hacer antes: era el único excluido de esa reconciliación).
	//
	// Si esta escritura falla, la función CORTA ACÁ (antes seguía adelante
	// y, si el status todavía no estaba confirmado, terminaba devolviendo
	// "ignored: not confirmed" — un outcome que GetUnresolvedWebhookEvents
	// nunca reintenta, dejando el pago sin provider_payment_id guardado Y
	// sin ninguna forma de recuperarlo más adelante). Al devolver un
	// outcome que empieza con "error:", RetryFailedWebhookEvents sí vuelve
	// a intentar esta misma función completa más tarde.
	if serr := db.SetProviderPaymentID(database, txID, fmt.Sprintf("%d", paymentID)); serr != nil {
		if errors.Is(serr, db.ErrPaymentTransactionNotFound) {
			// No hay transacción local de NOWPayments con ese order_id: no es un
			// fallo transitorio ni algo que se pueda dar por procesado. El evento
			// se conserva y pasa a revisión manual (permanente, sin reintentos).
			slog.Error("NOWPayments: order_id sin transacción local de NOWPayments", "txID", txID, "paymentID", paymentID)
			return "error: transaction not found"
		}
		slog.Error("NOWPayments: no se pudo guardar el payment_id real, se reintentará", "txID", txID, "error", serr)
		return "error: no se pudo guardar el payment_id, pendiente de reintento"
	}

	if status == "partially_paid" {
		// El cliente pagó menos cripto de lo esperado — antes esto se
		// trataba igual que cualquier pago pendiente y simplemente
		// expiraba en 30 min sin que nadie se enterara. Ahora se
		// avisa por Discord para que soporte decida manualmente
		// (acreditar proporcional o contactar al cliente) en vez de
		// perderlo en silencio.
		discordbot.AlertUnderpaidCryptoPayment(paymentID, orderID)
		return "ignored: partially paid, admin alerted"
	}
	if status != "confirmed" && status != "finished" {
		// Pago pendiente CORRECTAMENTE registrado (provider_payment_id ya
		// se guardó arriba) — no es un fallo interno, es el estado normal
		// mientras la pasarela todavía no confirma el cobro. No se
		// reintenta como webhook (nada que "arreglar" acá); la
		// reconciliación automática (GetStalePendingPayments, cada 2 min)
		// ya lo sigue de cerca usando el provider_payment_id guardado.
		return "pending: aún no confirmado por la pasarela"
	}

	if err := processApprovedPayment(database, txID); err != nil {
		slog.Error("NOWPayments processing failed", "txID", txID, "error", err)
		return "error: " + err.Error()
	}
	return "processed"
}

// nowPaymentsRetryMinAge acota qué eventos reintenta RetryFailedWebhookEvents:
// al menos 3 minutos de antigüedad para no competir con el propio
// procesamiento asíncrono del webhook en vivo (todavía podría estar
// corriendo). A propósito NO hay un límite superior de antigüedad — ver el
// comentario en db.GetUnresolvedWebhookEvents: un evento que lleva mucho
// tiempo fallando (típicamente porque nunca se pudo guardar su
// provider_payment_id) es exactamente el que más necesita seguir
// reintentándose, no el que hay que dejar de intentar.
const nowPaymentsRetryMinAge = 3 * time.Minute

// RetryFailedWebhookEvents reprocesa eventos de NOWPayments que quedaron
// registrados (LogWebhookEvent) pero nunca se terminaron de procesar con
// éxito — o porque la primera consulta a la pasarela falló (error de red
// transitorio, por ejemplo), o porque el proceso se cayó a mitad de camino
// y el resultado nunca se llegó a guardar. Confirmar la recepción de un
// webhook con HTTP 200 no basta por sí solo — esto es lo que convierte ese
// registro en un mecanismo de recuperación de verdad: nada se pierde solo
// porque un intento falló o el servidor se reinició. Se llama
// periódicamente desde main.go, igual que ReconcilePendingPayments.
//
// Reintentos con backoff y bandeja de revisión: un error que se repite ya no
// se reintenta cada 3 minutos para siempre. Cada fallo se clasifica (ver
// isPermanentNOWPaymentsOutcome), se cuenta el intento y se agenda el
// siguiente con backoff exponencial (3 min, 6, 12, … tope 6 h). Al agotar
// nowPaymentsMaxRetryAttempts — o de inmediato ante un error no recuperable —
// el evento pasa a la bandeja de revisión manual (webhook_events.review_at):
// se CONSERVA con su payment_id, se avisa a un admin por Discord, y
// permanece hasta que se reencole (db.RequeueWebhookEvent). Nunca se
// descarta un pago que todavía podría cobrarse o necesitar conciliación.
func RetryFailedWebhookEvents(database *sql.DB) {
	events, err := db.GetUnresolvedWebhookEvents(database, "nowpayments", nowPaymentsRetryMinAge)
	if err != nil {
		slog.Error("RetryFailedWebhookEvents: error listando eventos sin resolver", "error", err)
		return
	}
	for _, ev := range events {
		ev := ev
		safe.Run("RetryFailedWebhookEvents.nowpayments", func() {
			// Coordinación entre trabajadores/réplicas: solo quien obtiene el lease
			// procesa el evento (ver db.ClaimWebhookEvent).
			if claimed, err := db.ClaimWebhookEvent(database, ev.ID, webhookProcessingLease); err != nil || !claimed {
				if err != nil {
					slog.Error("RetryFailedWebhookEvents: no se pudo reclamar el evento", "eventID", ev.ID, "error", err)
				}
				return
			}
			paymentID, ok := parseNOWPaymentsPaymentID([]byte(ev.RawBody))
			if !ok {
				db.MarkWebhookEventProcessed(database, ev.ID, "ignored: invalid JSON or no payment_id")
				return
			}
			slog.Info("RetryFailedWebhookEvents: reintentando", "eventID", ev.ID, "paymentID", paymentID, "attempt", ev.Attempts+1)
			outcome := processNOWPaymentsPaymentID(database, paymentID)
			settleNOWPaymentsEvent(database, ev.ID, paymentID, outcome)
			if outcome == "processed" {
				slog.Info("RetryFailedWebhookEvents: evento de NOWPayments recuperado", "eventID", ev.ID, "paymentID", paymentID)
			}
		})
	}
}

// webhookProcessingLease: cuánto dura el lease de procesamiento de un evento.
// Debe superar el tiempo normal de un intento (consulta a la pasarela +
// acreditación); si el proceso se cae, el lease vence y otro trabajador lo toma.
// La acreditación sigue protegida por CreditPaymentOnce aunque dos trabajadores
// coincidieran tras un vencimiento.
const webhookProcessingLease = 5 * time.Minute

const (
	// nowPaymentsMaxRetryAttempts: intentos totales (el del webhook en vivo
	// cuenta como el primero) antes de mandar el evento a revisión manual.
	// Con el backoff de abajo son ~6 h de reintentos automáticos.
	nowPaymentsMaxRetryAttempts = 8
	nowPaymentsBackoffBase      = 3 * time.Minute
	nowPaymentsBackoffMax       = 6 * time.Hour
)

// nowPaymentsRetryBackoff: 3 min tras el 1er fallo, luego 6, 12, 24 … con tope.
func nowPaymentsRetryBackoff(attempt int) time.Duration {
	d := nowPaymentsBackoffBase
	for i := 1; i < attempt && d < nowPaymentsBackoffMax; i++ {
		d *= 2
	}
	if d > nowPaymentsBackoffMax {
		d = nowPaymentsBackoffMax
	}
	return d
}

// isPermanentNOWPaymentsOutcome: errores que reintentar no arregla (el
// order_id que devuelve la pasarela no es un UUID nuestro: no hay a qué pago
// asociarlo, hace falta una persona). Van directo a revisión manual.
// Recuperables (transitorios): la consulta a la pasarela falló, no se pudo
// guardar el payment_id, o falló la acreditación (base de datos, etc.).
func isPermanentNOWPaymentsOutcome(outcome string) bool {
	return strings.HasPrefix(outcome, "error: invalid order_id") || strings.HasPrefix(outcome, "error: transaction not found")
}

// settleNOWPaymentsEvent aplica el resultado de un intento (en vivo o de
// reintento) a su fila de webhook_events: éxito/ignorado/pendiente se marcan
// como procesados; un "error:" cuenta un intento y agenda backoff o, si
// corresponde, pasa a revisión manual y avisa a un admin.
func settleNOWPaymentsEvent(database *sql.DB, eventID uuid.UUID, paymentID int64, outcome string) {
	if !strings.HasPrefix(outcome, "error:") {
		db.MarkWebhookEventProcessed(database, eventID, outcome)
		return
	}
	permanent := isPermanentNOWPaymentsOutcome(outcome)
	moved, attempts, err := db.RecordWebhookRetryFailure(database, eventID, outcome, permanent, nowPaymentsMaxRetryAttempts, nowPaymentsRetryBackoff)
	if errors.Is(err, db.ErrWebhookEventAlreadySettled) {
		slog.Info("NOWPayments: fallo atrasado ignorado, el evento ya estaba resuelto o en revisión", "eventID", eventID, "paymentID", paymentID, "outcome", outcome)
		return
	}
	if err != nil {
		slog.Error("NOWPayments: no se pudo registrar el intento fallido, se deja el resultado tal cual", "eventID", eventID, "error", err)
		db.MarkWebhookEventProcessed(database, eventID, outcome)
		return
	}
	slog.Warn("NOWPayments: intento de procesamiento fallido", "eventID", eventID, "paymentID", paymentID, "attempt", attempts, "permanent", permanent, "review", moved, "outcome", outcome)
	if moved {
		discordbot.AlertWebhookNeedsReview("nowpayments", eventID.String(), fmt.Sprintf("payment_id %d — %s (intentos: %d)", paymentID, outcome, attempts))
	}
}

// ==================== DLOCAL GO WEBHOOK ====================

func HandlerDLocalGoWebhook(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, ok := readWebhookBody(c, "dlocalgo")
		if !ok { return }
		slog.Info("dLocal Go webhook received", "bytes", len(body))

		if !verifyDLocalGoSignature(body, c.GetHeader("Authorization")) {
			// Firma inválida: cualquiera pudo mandar este POST, no es una
			// notificación real de dLocal Go. 401, no 200 (mismo criterio que
			// NOWPayments más arriba): si esto fuera en realidad una
			// notificación legítima que falló por un secreto mal configurado
			// de nuestro lado, el propio mecanismo de reintentos de dLocal Go
			// seguirá intentando la entrega — mejor eso que responder 200 y
			// perder la notificación silenciosamente.
			slog.Warn("dLocal Go webhook: firma inválida, ignorando")
			// Escritura atómica ya resuelta (antes LogWebhookEvent la dejaba
			// con processed_at NULL para siempre: nunca se reintenta ni se
			// purgaba) y con el cuerpo acotado — es contenido de un tercero
			// no autenticado.
			if err := logRejectedWebhook(database, "dlocalgo", "invalid signature", body); err != nil {
				slog.Error("dLocal Go webhook: no se pudo registrar el rechazo por firma inválida", "error", err)
			}
			c.JSON(http.StatusUnauthorized, gin.H{"received": false, "error": "firma inválida"})
			return
		}

		// Firma válida: se comprueba el payment_id ANTES de persistir y solo
		// se guarda ese dato, no el cuerpo completo.
		var notification struct {
			PaymentID string `json:"payment_id"`
		}
		if err := json.Unmarshal(body, &notification); err != nil || !webhookRefPattern.MatchString(notification.PaymentID) {
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}
		eventID, ok := logWebhookEventOrReject(c, database, "dlocalgo", string(normalizedPaymentIDEvent(notification.PaymentID)))
		if !ok { return }

		go safe.Run("HandlerDLocalGoWebhook", func() {
			outcome := "ignored: not paid"
			defer func() { db.MarkWebhookEventProcessed(database, eventID, outcome) }()

			status, orderID, err := dlocalGoPaymentStatus(notification.PaymentID)
			if err != nil {
				slog.Error("dLocal Go status query failed", "paymentID", notification.PaymentID, "error", err)
				outcome = "error: status query failed"
				return
			}
			if status != "PAID" {
				return
			}

			txID, err := uuid.Parse(orderID)
			if err != nil {
				slog.Error("dLocal Go invalid order_id", "id", orderID)
				outcome = "error: invalid order_id"
				return
			}
			if err := processApprovedPayment(database, txID); err != nil {
				slog.Error("dLocal Go processing failed", "txID", txID, "error", err)
				outcome = "error: " + err.Error()
			} else {
				outcome = "processed"
			}
		})

		c.JSON(http.StatusOK, gin.H{"received": true})
	}
}
