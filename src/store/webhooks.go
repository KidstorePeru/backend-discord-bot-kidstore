package store

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"
	"KidStoreStore/src/safe"
	"KidStoreStore/src/types"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
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

	// Send payment approved email notification
	if customer, err := db.GetCustomerByID(database, tx.CustomerID); err == nil {
		if customer.Email != nil && *customer.Email != "" {
			voucherURL := fmt.Sprintf("https://www.kidstoreperu.net/dashboard/comprobantes/pago/%s", tx.ID)
			go SendPaymentApprovedEmail(smtpConfig, *customer.Email, tx.ProductName, tx.AmountPEN, tx.KCAmount, tx.Gateway, voucherURL, "es")
		}
		if tx.PaymentType == "kc_recharge" && tx.KCAmount > 0 {
			discordbot.NotifyRecharge(customer, tx.KCAmount, customer.KCBalance, tx.Gateway)
		}
	}

	return nil
}

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
		status, _, err := getPayPalOrder(p.ExternalID)
		if err != nil {
			return gatewayStillPending, err
		}
		return classifyPayPalStatus(status), nil
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
// contra), se da por perdido. Deliberadamente generoso: una confirmación
// cripto (NOWPayments) puede tardar horas en la blockchain en momentos de
// congestión, mucho más que cualquier tarjeta o transferencia.
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
			return
		}
		slog.Info("reconciliación: pago acreditado sin depender del webhook", "txID", p.ID, "gateway", p.Gateway)
	case gatewayRejected:
		if err := db.AdminUpdatePaymentStatus(database, p.ID, "failed"); err != nil {
			slog.Error("reconciliación: error marcando pago rechazado", "txID", p.ID, "gateway", p.Gateway, "error", err)
			return
		}
		slog.Info("reconciliación: pago rechazado confirmado por la pasarela", "txID", p.ID, "gateway", p.Gateway)
	default: // gatewayStillPending — todavía nada resuelto, o PayPal APPROVED-sin-capturar
		if p.Gateway == "paypal" {
			// Intentar la captura mejora las chances de la próxima pasada, pero
			// nunca decide por sí sola el resultado (checkGatewayOutcome vuelve
			// a consultar la próxima vez).
			if status, _, gerr := getPayPalOrder(p.ExternalID); gerr == nil && status == "APPROVED" {
				capturePayPalOrder(p.ExternalID)
			}
		}
		if time.Since(p.CreatedAt) > reconcileDeadLetterAfter {
			slog.Warn("reconciliación: pago nunca se pudo confirmar ni descartar, se da por perdido", "txID", p.ID, "gateway", p.Gateway, "edad", time.Since(p.CreatedAt))
			if err := db.AdminUpdatePaymentStatus(database, p.ID, "expired"); err != nil {
				slog.Error("reconciliación: error expirando pago sin resolver", "txID", p.ID, "error", err)
				return
			}
			discordbot.AlertUnresolvedPayment(p.ID.String(), p.Gateway)
		}
	}
}

// ==================== MERCADOPAGO WEBHOOK ====================

func HandlerMercadoPagoWebhook(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		slog.Info("MercadoPago webhook received", "body", string(body))
		// Registro duradero ANTES de intentar procesar nada — si el proceso
		// se cae a mitad de camino, queda constancia de que la notificación
		// sí llegó (ver ReconcilePendingPayments para la recuperación real).
		eventID := db.LogWebhookEvent(database, "mercadopago", string(body))

		var notification struct {
			Type string `json:"type"`
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &notification); err != nil {
			db.MarkWebhookEventProcessed(database, eventID, "ignored: invalid JSON")
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}

		// Only process payment notifications
		if notification.Type != "payment" {
			db.MarkWebhookEventProcessed(database, eventID, "ignored: type="+notification.Type)
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}

		// Query MercadoPago API for payment details
		paymentID := notification.Data.ID
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
		body, _ := io.ReadAll(c.Request.Body)
		slog.Info("PayPal webhook received", "body", string(body))
		eventID := db.LogWebhookEvent(database, "paypal", string(body))

		var event struct {
			EventType string `json:"event_type"`
			Resource  struct {
				ID                string `json:"id"`
				SupplementaryData struct {
					RelatedIDs struct {
						OrderID string `json:"order_id"`
					} `json:"related_ids"`
				} `json:"supplementary_data"`
			} `json:"resource"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			db.MarkWebhookEventProcessed(database, eventID, "ignored: invalid JSON")
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}

		if event.EventType != "CHECKOUT.ORDER.APPROVED" && event.EventType != "PAYMENT.CAPTURE.COMPLETED" {
			db.MarkWebhookEventProcessed(database, eventID, "ignored: event_type="+event.EventType)
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}

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
			status, refID, err := getPayPalOrder(orderID)
			if err != nil {
				slog.Error("PayPal order query failed", "orderID", orderID, "error", err)
				outcome = "error: order query failed"
				return
			}
			if status != "COMPLETED" {
				return
			}
			if refID == "" {
				slog.Warn("PayPal order sin reference_id", "orderID", orderID)
				outcome = "error: no reference_id"
				return
			}

			txID, err := uuid.Parse(refID)
			if err != nil {
				slog.Error("PayPal invalid reference_id", "ref", refID)
				outcome = "error: invalid reference_id"
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
		status, refID, err := getPayPalOrder(paypalToken)
		if err != nil {
			slog.Error("PayPal order query on return failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error verificando pago"})
			return
		}
		if status != "COMPLETED" {
			c.JSON(http.StatusOK, gin.H{"success": false, "error": "pago aun no completado"})
			return
		}

		txID, err := uuid.Parse(refID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "orden sin referencia valida"})
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
		body, _ := io.ReadAll(c.Request.Body)
		slog.Info("NOWPayments webhook received", "body", string(body))
		eventID := db.LogWebhookEvent(database, "nowpayments", string(body))

		var notification struct {
			PaymentID int64 `json:"payment_id"`
		}
		if err := json.Unmarshal(body, &notification); err != nil || notification.PaymentID == 0 {
			db.MarkWebhookEventProcessed(database, eventID, "ignored: invalid JSON or no payment_id")
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}

		go safe.Run("HandlerNOWPaymentsWebhook", func() {
			outcome := "ignored: not confirmed"
			defer func() { db.MarkWebhookEventProcessed(database, eventID, outcome) }()

			// El IPN no viene firmado — no se le puede creer su "payment_status" ni
			// su "order_id" a ciegas, cualquiera podría forjar este POST. Se vuelve
			// a consultar el estado real directamente en la API de NOWPayments con
			// nuestra propia API key, igual que ya se hace con MercadoPago.
			status, orderID, err := nowPaymentsStatus(notification.PaymentID)
			if err != nil {
				slog.Error("NOWPayments status query failed", "paymentID", notification.PaymentID, "error", err)
				outcome = "error: status query failed"
				return
			}

			txID, parseErr := uuid.Parse(orderID)
			if parseErr != nil {
				slog.Error("NOWPayments invalid order_id", "id", orderID)
				outcome = "error: invalid order_id"
				return
			}

			// Guardar el ID real del pago ANTES de decidir qué hacer con su
			// estado — así, aunque el proceso se caiga justo después de esta
			// línea, la reconciliación automática (cada 2 min, ver
			// reconcileOnePayment) puede seguir consultando el pago real sin
			// depender de que este webhook se repita (algo que este gateway no
			// podía hacer antes: era el único excluido de esa reconciliación).
			if serr := db.SetProviderPaymentID(database, txID, fmt.Sprintf("%d", notification.PaymentID)); serr != nil {
				slog.Error("NOWPayments: no se pudo guardar el payment_id real", "txID", txID, "error", serr)
			}

			if status == "partially_paid" {
				// El cliente pagó menos cripto de lo esperado — antes esto se
				// trataba igual que cualquier pago pendiente y simplemente
				// expiraba en 30 min sin que nadie se enterara. Ahora se
				// avisa por Discord para que soporte decida manualmente
				// (acreditar proporcional o contactar al cliente) en vez de
				// perderlo en silencio.
				discordbot.AlertUnderpaidCryptoPayment(notification.PaymentID, orderID)
				outcome = "ignored: partially paid, admin alerted"
				return
			}
			if status != "confirmed" && status != "finished" {
				return
			}

			if err := processApprovedPayment(database, txID); err != nil {
				slog.Error("NOWPayments processing failed", "txID", txID, "error", err)
				outcome = "error: " + err.Error()
			} else {
				outcome = "processed"
			}
		})

		c.JSON(http.StatusOK, gin.H{"received": true})
	}
}

// ==================== DLOCAL GO WEBHOOK ====================

func HandlerDLocalGoWebhook(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		slog.Info("dLocal Go webhook received", "body", string(body))

		if !verifyDLocalGoSignature(body, c.GetHeader("Authorization")) {
			slog.Warn("dLocal Go webhook: firma inválida, ignorando")
			db.LogWebhookEvent(database, "dlocalgo", string(body)) // queda constancia igual, aunque se ignore
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}
		eventID := db.LogWebhookEvent(database, "dlocalgo", string(body))

		var notification struct {
			PaymentID string `json:"payment_id"`
		}
		if err := json.Unmarshal(body, &notification); err != nil || notification.PaymentID == "" {
			db.MarkWebhookEventProcessed(database, eventID, "ignored: invalid JSON or no payment_id")
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}

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
