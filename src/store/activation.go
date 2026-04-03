package store

import (
	"KidStoreStore/src/autobuyer"
	"KidStoreStore/src/db"
	"KidStoreStore/src/middleware"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// backendURL for autobuyer webhooks
var activationBackendURL string

func SetActivationBackendURL(url string) {
	activationBackendURL = url
}

// ==================== ACTIVATE ====================

func HandlerActivate(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, _ := uuid.Parse(customerIDStr)

		var req struct {
			Code         string `json:"code" binding:"required"`
			EpicUsername string `json:"epic_username"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		code := strings.ToUpper(strings.TrimSpace(req.Code))

		if !autobuyer.IsConfigured() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "error": "sistema de activacion no disponible"})
			return
		}

		// Find payment by activation code
		payment, err := db.GetPaymentByActivationCode(database, code)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "codigo de activacion no encontrado"})
			return
		}

		// Validate ownership
		if payment.CustomerID != customerID {
			c.JSON(http.StatusForbidden, gin.H{"success": false, "error": "este codigo no te pertenece"})
			return
		}

		// Validate status
		if payment.Status == "fulfilled" {
			c.JSON(http.StatusConflict, gin.H{"success": false, "error": "este codigo ya fue activado"})
			return
		}
		if payment.Status != "approved" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": fmt.Sprintf("estado de pago no valido: %s", payment.Status)})
			return
		}

		// Already has a task running?
		if payment.AutobuyerTaskID != "" {
			task, err := autobuyer.GetTask(payment.AutobuyerTaskID)
			if err == nil && (task.Status == "pending" || task.Status == "running" || task.Status == "waiting_auth" || task.Status == "waiting_input") {
				c.JSON(http.StatusOK, gin.H{
					"success":  true,
					"message":  "activacion en progreso",
					"task_id":  payment.AutobuyerTaskID,
					"status":   task.Status,
					"auth_url": task.AuthURL,
				})
				return
			}
		}

		// Determine epic username
		epicUsername := req.EpicUsername
		if epicUsername == "" {
			customer, err := db.GetCustomerByID(database, customerID)
			if err == nil {
				epicUsername = customer.EpicUsername
			}
		}

		// Create autobuyer task
		webhookURL := ""
		if activationBackendURL != "" {
			webhookURL = activationBackendURL + "/store/webhook/autobuyer"
		}

		task, err := autobuyer.CreateSale(autobuyer.CreateSaleRequest{
			ProductName:       payment.ProductName,
			PaymentType:       "razer",
			Buyer:             epicUsername,
			OrderID:           payment.ID.String(),
			StatusWebhookURL:  webhookURL,
			MessageWebhookURL: webhookURL,
		})
		if err != nil {
			slog.Error("Autobuyer create sale failed", "code", code, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error conectando con el autobuyer: " + err.Error()})
			return
		}

		// Save task ID
		db.UpdatePaymentActivation(database, payment.ID, task.ID, "activating")
		db.AddAuditLog(database, &customerID, "ACTIVATION_STARTED",
			fmt.Sprintf("producto %s activado con codigo %s, task %s", payment.ProductName, code, task.ID), "")

		slog.Info("Activation started", "code", code, "product", payment.ProductName, "task", task.ID)

		c.JSON(http.StatusOK, gin.H{
			"success":  true,
			"message":  "activacion iniciada",
			"task_id":  task.ID,
			"status":   task.Status,
			"auth_url": task.AuthURL,
		})
	}
}

// ==================== ACTIVATION STATUS ====================

func HandlerActivationStatus(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		code := strings.ToUpper(c.Param("code"))

		payment, err := db.GetPaymentByActivationCode(database, code)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "codigo no encontrado"})
			return
		}

		result := gin.H{
			"success":      true,
			"payment_status": payment.Status,
			"product_name": payment.ProductName,
			"activation_code": payment.ActivationCode,
		}

		if payment.AutobuyerTaskID != "" {
			task, err := autobuyer.GetTask(payment.AutobuyerTaskID)
			if err == nil {
				result["task_status"] = task.Status
				result["auth_url"] = task.AuthURL
				result["input_needed"] = task.InputNeeded
				result["error"] = task.Error
				if task.Result != nil {
					if s, ok := task.Result["success"].(bool); ok { result["completed"] = s }
				}
				if len(task.Messages) > 0 {
					last := task.Messages[len(task.Messages)-1]
					result["last_message"] = last.Text
				}
			}
		}

		c.JSON(http.StatusOK, result)
	}
}

// ==================== ACTIVATION INPUT ====================

func HandlerActivationInput(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		code := strings.ToUpper(c.Param("code"))

		var req struct {
			Value string `json:"value" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		payment, err := db.GetPaymentByActivationCode(database, code)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "codigo no encontrado"})
			return
		}

		if payment.AutobuyerTaskID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "no hay tarea activa para este codigo"})
			return
		}

		if err := autobuyer.SubmitInput(payment.AutobuyerTaskID, req.Value); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error enviando input: " + err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "message": "input enviado"})
	}
}

// ==================== AUTOBUYER WEBHOOK ====================

func HandlerAutobuyerWebhook(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, _ := io.ReadAll(c.Request.Body)
		slog.Info("Autobuyer webhook received", "body", string(body))

		var event struct {
			TaskID      string `json:"task_id"`
			Event       string `json:"event"`
			Status      string `json:"status"`
			AuthURL     string `json:"auth_url"`
			InputNeeded string `json:"input_needed"`
			Text        string `json:"text"`
			Error  string                 `json:"error"`
			Result map[string]interface{} `json:"result"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			c.JSON(http.StatusOK, gin.H{"received": true})
			return
		}

		// Save progress from every webhook event to DB
		if event.TaskID != "" && event.Text != "" {
			currentProgress := db.GetPaymentProgress(database, event.TaskID)
			newProgress := currentProgress
			if newProgress != "" { newProgress += "|" }
			newProgress += event.Text
			if err := db.UpdatePaymentProgress(database, event.TaskID, newProgress); err != nil {
				slog.Error("Failed to save webhook progress", "task", event.TaskID, "error", err)
			} else {
				slog.Info("Webhook progress saved", "task", event.TaskID, "text", event.Text, "total_parts", len(strings.Split(newProgress, "|")))
			}
		} else {
			slog.Info("Webhook event without text", "task", event.TaskID, "event", event.Event, "status", event.Status)
		}

		// Find payment by autobuyer_task_id
		if event.Status == "done" && event.Result != nil {
			// Task completed — mark payment as fulfilled
			slog.Info("Autobuyer task completed", "task", event.TaskID)
			// Find the payment transaction with this task ID
			var paymentID uuid.UUID
			err := database.QueryRow(`SELECT id FROM payment_transactions WHERE autobuyer_task_id=$1`, event.TaskID).Scan(&paymentID)
			if err == nil {
				db.UpdatePaymentActivation(database, paymentID, event.TaskID, "fulfilled")
				slog.Info("Payment fulfilled", "payment", paymentID, "task", event.TaskID)

				// Send notifications
				var customerID uuid.UUID
				var productName string
				database.QueryRow(`SELECT customer_id, product_name FROM payment_transactions WHERE id=$1`, paymentID).Scan(&customerID, &productName)
				customer, err := db.GetCustomerByID(database, customerID)
				if err == nil {
					lang := "es"
					if customer.DiscordID != nil {
						if dl, _ := db.GetDiscordLang(database, *customer.DiscordID); dl != "" { lang = dl }
					}
					// Email notification
					if customer.Email != nil && *customer.Email != "" {
						deliveredSubject := "Producto entregado"
						if lang == "en" { deliveredSubject = "Product delivered" }
						go SendPaymentApprovedEmail(smtpConfig, *customer.Email, deliveredSubject, 0, 0, "autobuyer", lang, "")
					}
					// Discord DM notification to customer
					if customer.DiscordID != nil && *customer.DiscordID != "" && notifyDiscord != nil {
						go notifyDiscord(*customer.DiscordID, "sent", productName, 0, lang)
					}
					// Discord DM notification to admins
					if notifyProductPurchase != nil {
						go notifyProductPurchase(productName, customer.EpicUsername, "autobuyer - completado", 0)
					}
				}
			}
		} else if event.Status == "error" {
			slog.Error("Autobuyer task failed", "task", event.TaskID, "error", event.Error)
			var paymentID uuid.UUID
			err := database.QueryRow(`SELECT id FROM payment_transactions WHERE autobuyer_task_id=$1`, event.TaskID).Scan(&paymentID)
			if err == nil {
				db.UpdatePaymentActivation(database, paymentID, event.TaskID, "approved") // back to approved so they can retry
			}
		}

		c.JSON(http.StatusOK, gin.H{"received": true})
	}
}
