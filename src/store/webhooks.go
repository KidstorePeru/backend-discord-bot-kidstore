package store

import (
	"KidStoreStore/src/autobuyer"
	"KidStoreStore/src/db"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)


var notifyProductPurchase func(productName, epicUsername, gateway string, amountPEN float64)

func SetProductPurchaseNotifier(fn func(productName, epicUsername, gateway string, amountPEN float64)) {
	notifyProductPurchase = fn
}

// ==================== COMMON ====================

func processApprovedPayment(database *sql.DB, txID uuid.UUID) error {
	tx, err := db.GetPaymentTransaction(database, txID)
	if err != nil {
		return fmt.Errorf("transaction not found: %w", err)
	}
	if tx.Status == "approved" {
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

	// For product purchases, generate activation code and notify
	if tx.PaymentType == "product_purchase" {
		// Generate 8-char activation code
		codeBytes := make([]byte, 4)
		rand.Read(codeBytes)
		activationCode := strings.ToUpper(hex.EncodeToString(codeBytes))
		if err := db.SetActivationCode(database, txID, activationCode); err != nil {
			slog.Error("Failed to set activation code", "txID", txID, "error", err)
		}
		tx.ActivationCode = activationCode

		// Register the code in the autobuyer so !activar works in the chatbot
		epicUsername := ""
		if cust, err := db.GetCustomerByID(database, tx.CustomerID); err == nil {
			epicUsername = cust.EpicUsername
		}
		if autobuyer.IsConfigured() {
			go func() {
				if err := autobuyer.RegisterCode(activationCode, tx.ProductName, "razer", epicUsername, tx.ID.String()); err != nil {
					slog.Error("Failed to register code in autobuyer", "code", activationCode, "error", err)
				} else {
					slog.Info("Code registered in autobuyer", "code", activationCode, "product", tx.ProductName)
				}
			}()
		}

		slog.Info("Product payment approved — pending fulfillment", "customer", tx.CustomerID, "product", tx.ProductID, "gateway", tx.Gateway)
		db.AddAuditLog(database, &tx.CustomerID, "PRODUCT_PAID",
			fmt.Sprintf("pago aprobado: %s via %s (S/%.2f)", tx.ProductName, tx.Gateway, tx.AmountPEN), "webhook")

		// Notify customer via Discord DM
		customer, err := db.GetCustomerByID(database, tx.CustomerID)
		if err == nil && customer.DiscordID != nil && *customer.DiscordID != "" {
			lang, _ := db.GetDiscordLang(database, *customer.DiscordID)
			if lang == "" { lang = "es" }
			if notifyDiscord != nil {
				go notifyDiscord(*customer.DiscordID, "product_paid", tx.ProductName, 0, lang)
			}
		}

		// Notify admin via Discord (product needs manual fulfillment)
		if notifyProductPurchase != nil {
			epicUsername := ""
			if err == nil { epicUsername = customer.EpicUsername }
			go notifyProductPurchase(tx.ProductName, epicUsername, tx.Gateway, tx.AmountPEN)
		}
	}

	// Send payment approved email notification
	if customer, err := db.GetCustomerByID(database, tx.CustomerID); err == nil && customer.Email != nil && *customer.Email != "" {
		emailLang := "es"
		if customer.DiscordID != nil {
			if dl, err := db.GetDiscordLang(database, *customer.DiscordID); err == nil && dl != "" { emailLang = dl }
		}
		go SendPaymentApprovedEmail(smtpConfig, *customer.Email, tx.ProductName, tx.AmountPEN, tx.KCAmount, tx.Gateway, emailLang, tx.ActivationCode)
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
				ID            string `json:"id"`
				PurchaseUnits []struct {
					ReferenceID string `json:"reference_id"`
				} `json:"purchase_units"`
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
			// Capture the order if it was just approved
			if event.EventType == "CHECKOUT.ORDER.APPROVED" {
				if err := capturePayPalOrder(event.Resource.ID); err != nil {
					slog.Error("PayPal capture failed", "orderID", event.Resource.ID, "error", err)
					return
				}
			}

			// Find our transaction ID from reference
			var refID string
			if len(event.Resource.PurchaseUnits) > 0 {
				refID = event.Resource.PurchaseUnits[0].ReferenceID
			}
			if refID == "" {
				slog.Warn("PayPal webhook missing reference_id")
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
		paymentID := c.Query("id")
		paypalToken := c.Query("token") // PayPal order ID

		txID, err := uuid.Parse(paymentID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id invalido"})
			return
		}

		// Capture the PayPal order
		if paypalToken != "" {
			if err := capturePayPalOrder(paypalToken); err != nil {
				slog.Error("PayPal capture on return failed", "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error capturando pago"})
				return
			}
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
			PaymentStatus string `json:"payment_status"`
			OrderID       string `json:"order_id"`
			PaymentID     int64  `json:"payment_id"`
		}
		if err := json.Unmarshal(body, &notification); err != nil {
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}

		// Only process confirmed/finished payments
		if notification.PaymentStatus != "confirmed" && notification.PaymentStatus != "finished" {
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}

		go func() {
			txID, err := uuid.Parse(notification.OrderID)
			if err != nil {
				slog.Error("NOWPayments invalid order_id", "id", notification.OrderID)
				return
			}

			if err := processApprovedPayment(database, txID); err != nil {
				slog.Error("NOWPayments processing failed", "txID", txID, "error", err)
			}
		}()

		c.JSON(http.StatusOK, gin.H{"received": true})
	}
}
