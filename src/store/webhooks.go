package store

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	tx, err := db.GetPaymentTransaction(database, txID)
	if err != nil {
		return fmt.Errorf("transaction not found: %w", err)
	}
	if tx.Status == "approved" || tx.Status == "fulfilled" {
		return nil // already processed (idempotent)
	}

	if err := db.UpdatePaymentStatus(database, txID, "approved", tx.ExternalID); err != nil {
		return fmt.Errorf("updating status: %w", err)
	}

	// If KC recharge, credit KC
	if tx.PaymentType == "kc_recharge" && tx.KCAmount > 0 {
		soles := tx.AmountPEN
		note := fmt.Sprintf("Pago automatico via %s (ID: %s)", tx.Gateway, tx.ExternalID)
		if err := db.RechargeKC(database, tx.CustomerID, tx.KCAmount, &soles, &note, tx.Gateway, tx.Gateway); err != nil {
			slog.Error("KC recharge failed after payment", "txID", txID, "error", err)
			return fmt.Errorf("recharge failed: %w", err)
		}
		slog.Info("KC credited via payment", "customer", tx.CustomerID, "kc", tx.KCAmount, "gateway", tx.Gateway)
	}

	// Send payment approved email notification
	if customer, err := db.GetCustomerByID(database, tx.CustomerID); err == nil {
		if customer.Email != nil && *customer.Email != "" {
			go SendPaymentApprovedEmail(smtpConfig, *customer.Email, tx.ProductName, tx.AmountPEN, tx.KCAmount, tx.Gateway, "es")
		}
		if tx.PaymentType == "kc_recharge" && tx.KCAmount > 0 {
			discordbot.NotifyRecharge(customer, tx.KCAmount, customer.KCBalance, tx.Gateway)
		}
	}

	return nil
}

// ==================== MERCADOPAGO WEBHOOK ====================

func HandlerMercadoPagoWebhook(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		slog.Info("MercadoPago webhook received", "body", string(body))

		var notification struct {
			Type string `json:"type"`
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &notification); err != nil {
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}

		// Only process payment notifications
		if notification.Type != "payment" {
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}

		// Query MercadoPago API for payment details
		go func() {
			paymentID := notification.Data.ID
			req, _ := http.NewRequest("GET",
				fmt.Sprintf("https://api.mercadopago.com/v1/payments/%s", paymentID), nil)
			req.Header.Set("Authorization", "Bearer "+paymentCfg.MercadoPagoToken)

			resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
			if err != nil {
				slog.Error("MP payment query failed", "error", err)
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
				return
			}

			if err := processApprovedPayment(database, txID); err != nil {
				slog.Error("MP payment processing failed", "txID", txID, "error", err)
			}
		}()

		c.JSON(http.StatusOK, gin.H{"received": true})
	}
}

// ==================== PAYPAL WEBHOOK ====================

func HandlerPayPalWebhook(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		slog.Info("PayPal webhook received", "body", string(body))

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
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}

		if event.EventType != "CHECKOUT.ORDER.APPROVED" && event.EventType != "PAYMENT.CAPTURE.COMPLETED" {
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}

		go func() {
			orderID := event.Resource.ID
			if event.EventType == "CHECKOUT.ORDER.APPROVED" {
				// Capturar el pago — esto ya de por sí solo funciona si la orden
				// es real y aprobada, PayPal la rechaza si no.
				if err := capturePayPalOrder(orderID); err != nil {
					slog.Error("PayPal capture failed", "orderID", orderID, "error", err)
					return
				}
			} else {
				// PAYMENT.CAPTURE.COMPLETED: resource.id es el ID del capture, no
				// el de la orden — el order_id real viene en supplementary_data.
				orderID = event.Resource.SupplementaryData.RelatedIDs.OrderID
			}
			if orderID == "" {
				slog.Warn("PayPal webhook sin order_id resoluble")
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
				return
			}
			if status != "COMPLETED" {
				return
			}
			if refID == "" {
				slog.Warn("PayPal order sin reference_id", "orderID", orderID)
				return
			}

			txID, err := uuid.Parse(refID)
			if err != nil {
				slog.Error("PayPal invalid reference_id", "ref", refID)
				return
			}

			if err := processApprovedPayment(database, txID); err != nil {
				slog.Error("PayPal payment processing failed", "txID", txID, "error", err)
			}
		}()

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
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
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

		var notification struct {
			PaymentID int64 `json:"payment_id"`
		}
		if err := json.Unmarshal(body, &notification); err != nil || notification.PaymentID == 0 {
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}

		go func() {
			// El IPN no viene firmado — no se le puede creer su "payment_status" ni
			// su "order_id" a ciegas, cualquiera podría forjar este POST. Se vuelve
			// a consultar el estado real directamente en la API de NOWPayments con
			// nuestra propia API key, igual que ya se hace con MercadoPago.
			status, orderID, err := nowPaymentsStatus(notification.PaymentID)
			if err != nil {
				slog.Error("NOWPayments status query failed", "paymentID", notification.PaymentID, "error", err)
				return
			}
			if status != "confirmed" && status != "finished" {
				return
			}

			txID, err := uuid.Parse(orderID)
			if err != nil {
				slog.Error("NOWPayments invalid order_id", "id", orderID)
				return
			}

			if err := processApprovedPayment(database, txID); err != nil {
				slog.Error("NOWPayments processing failed", "txID", txID, "error", err)
			}
		}()

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
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}

		var notification struct {
			PaymentID string `json:"payment_id"`
		}
		if err := json.Unmarshal(body, &notification); err != nil || notification.PaymentID == "" {
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}

		go func() {
			status, orderID, err := dlocalGoPaymentStatus(notification.PaymentID)
			if err != nil {
				slog.Error("dLocal Go status query failed", "paymentID", notification.PaymentID, "error", err)
				return
			}
			if status != "PAID" {
				return
			}

			txID, err := uuid.Parse(orderID)
			if err != nil {
				slog.Error("dLocal Go invalid order_id", "id", orderID)
				return
			}
			if err := processApprovedPayment(database, txID); err != nil {
				slog.Error("dLocal Go processing failed", "txID", txID, "error", err)
			}
		}()

		c.JSON(http.StatusOK, gin.H{"received": true})
	}
}
