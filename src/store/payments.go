package store

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/middleware"
	"KidStoreStore/src/safe"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Default exchange rate PEN → USD (fallback when API is unavailable)
const defaultUSDRate = 0.27

// ==================== CONFIG ====================

type PaymentConfig struct {
	MercadoPagoToken    string
	PayPalClientID      string
	PayPalClientSecret  string
	PayPalMode          string // sandbox or live
	NOWPaymentsAPIKey   string
	DLocalGoAPIKey      string
	DLocalGoSecretKey   string
	DLocalGoSandbox     bool
	FrontendURL         string
	BackendURL          string
}

var paymentCfg PaymentConfig

func SetPaymentConfig(cfg PaymentConfig) {
	paymentCfg = cfg
}

// ==================== PRODUCT PRICE MAP ====================
// Prices in PEN — must match frontend constants.ts

var productPrices = map[string]struct {
	Name     string
	PricePEN float64
	KCAmount int // only for kc_recharge
}{
	// KC packages — mismo precio para pago manual y automático (S/1.30 cada 100 KC)
	"starter": {Name: "Starter 800 KC", PricePEN: 10.40, KCAmount: 800},
	"gamer":   {Name: "Gamer 2,400 KC", PricePEN: 31.20, KCAmount: 2400},
	"pro":     {Name: "Pro 4,500 KC", PricePEN: 58.50, KCAmount: 4500},
	"legend":  {Name: "Legend 12,500 KC", PricePEN: 162.50, KCAmount: 12500},
}

// kcRatePEN — precio oficial de 1 KC en soles (S/1.30 cada 100 KC). Debe
// coincidir siempre con KC_RATE en Recharge.tsx del frontend. Para una
// recarga personalizada, el precio a cobrar SIEMPRE se calcula acá con esta
// tasa — nunca se usa el "custom_price" que mande el cliente para decidir
// cuánto cobrar, porque eso permitiría pagar un monto ridículamente bajo
// (p. ej. S/0.01) y pedir cualquier cantidad de KC (p. ej. 999,999,999).
const kcRatePEN = 0.013
const minCustomKC = 100
const maxCustomKC = 10_000_000

// ==================== CREATE PAYMENT ====================

func HandlerCreatePayment(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, _ := uuid.Parse(customerIDStr)

		var req struct {
			Gateway     string  `json:"gateway" binding:"required"`
			PaymentType string  `json:"payment_type" binding:"required"`
			ProductID   string  `json:"product_id" binding:"required"`
			CustomName  string  `json:"custom_name"`
			CustomPrice float64 `json:"custom_price"`
			CustomKC    int     `json:"custom_kc"`
			// Currency: divisa de referencia del cliente (ISO 4217). Solo la usa
			// dLocal Go, para cobrar en la moneda real del cliente en vez de
			// forzar PEN/USD. Las demas pasarelas la ignoran.
			Currency string `json:"currency"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		// Validate gateway
		if req.Gateway != "mercadopago" && req.Gateway != "paypal" && req.Gateway != "nowpayments" && req.Gateway != "dlocalgo" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "gateway invalido"})
			return
		}

		// Lookup product price or use custom
		var productName string
		var pricePEN float64
		var kcAmount int

		if req.CustomPrice > 0 && req.CustomName != "" {
			// Recarga de KC personalizada — el precio SIEMPRE se calcula acá con
			// la tasa oficial a partir de custom_kc. Nunca se usa custom_price
			// del cliente para el cobro real: si se usara tal cual, cualquiera
			// podría pagar S/0.01 y pedir la cantidad de KC que quisiera.
			if req.CustomKC < minCustomKC || req.CustomKC > maxCustomKC {
				c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": fmt.Sprintf("la recarga personalizada debe ser entre %d y %d KC", minCustomKC, maxCustomKC)})
				return
			}
			productName = req.CustomName
			pricePEN = roundCents(float64(req.CustomKC) * kcRatePEN)
			kcAmount = req.CustomKC
		} else {
			product, exists := productPrices[req.ProductID]
			if !exists {
				c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "producto no encontrado"})
				return
			}
			productName = product.Name
			pricePEN = product.PricePEN
			kcAmount = product.KCAmount
		}

		amountUSD := pricePEN * defaultUSDRate

		txID := uuid.New()
		tx := db.PaymentTransactionInput{
			ID:          txID,
			CustomerID:  customerID,
			Gateway:     req.Gateway,
			PaymentType: req.PaymentType,
			ProductID:   req.ProductID,
			ProductName: productName,
			AmountPEN:   pricePEN,
			AmountUSD:   amountUSD,
			KCAmount:    kcAmount,
		}

		// Create checkout URL based on gateway
		var checkoutURL string
		var externalID string
		var err error

		switch req.Gateway {
		case "mercadopago":
			checkoutURL, externalID, err = createMercadoPagoPreference(tx)
		case "paypal":
			checkoutURL, externalID, err = createPayPalOrder(tx)
		case "nowpayments":
			checkoutURL, externalID, err = createNOWPaymentsInvoice(tx)
		case "dlocalgo":
			currencyCode := req.Currency
			if currencyCode == "" {
				currencyCode = "USD"
			}
			var amountLocal float64
			amountLocal, err = convertPENToCurrency(pricePEN, currencyCode)
			if err == nil {
				tx.CurrencyCode = currencyCode
				tx.AmountLocal = amountLocal
				checkoutURL, externalID, err = createDLocalGoPayment(tx)
			}
		}

		if err != nil {
			slog.Error("Payment gateway error", "gateway", req.Gateway, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error creando pago: " + err.Error()})
			return
		}

		tx.ExternalID = externalID
		if err := db.CreatePaymentTransaction(database, tx); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error guardando transaccion"})
			return
		}

		db.AddAuditLog(database, &customerID, "PAYMENT_CREATED",
			fmt.Sprintf("pago %s via %s: %s (S/%.2f)", txID, req.Gateway, productName, pricePEN), c.ClientIP())

		c.JSON(http.StatusOK, gin.H{
			"success":      true,
			"payment_id":   txID,
			"checkout_url": checkoutURL,
			"external_id":  externalID,
		})
	}
}

// ==================== PAYMENT STATUS ====================

func HandlerPaymentStatus(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, err := uuid.Parse(customerIDStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id invalido"})
			return
		}

		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id invalido"})
			return
		}
		tx, err := db.GetPaymentTransaction(database, id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "transaccion no encontrada"})
			return
		}
		// Nunca dejar que un cliente vea el pago de otro cliente solo por
		// adivinar o conocer un UUID ajeno.
		if tx.CustomerID != customerID {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "transaccion no encontrada"})
			return
		}

		// If payment is pending, check with MercadoPago directly (for localhost without webhooks)
		if tx.Status == "pending" && tx.Gateway == "mercadopago" && tx.ExternalID != "" && paymentCfg.MercadoPagoToken != "" {
			txID := tx.ID
			go safe.Run("HandlerPaymentStatus.mercadopago-poll", func() {
				// Check payments by external_reference
				reqPay, err := http.NewRequest("GET",
					"https://api.mercadopago.com/v1/payments/search?external_reference="+url.QueryEscape(txID.String()), nil)
				if err != nil {
					slog.Error("MP status poll: error construyendo request", "error", err)
					return
				}
				reqPay.Header.Set("Authorization", "Bearer "+paymentCfg.MercadoPagoToken)
				client := &http.Client{Timeout: 10 * time.Second}
				resp, err := client.Do(reqPay)
				if err != nil { return }
				defer resp.Body.Close()
				var result struct {
					Results []struct {
						Status string `json:"status"`
					} `json:"results"`
				}
				json.NewDecoder(resp.Body).Decode(&result)
				if len(result.Results) > 0 && result.Results[0].Status == "approved" {
					processApprovedPayment(database, txID)
				}
			})
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "transaction": tx})
	}
}

// ==================== COMPROBANTE DE PAGO ====================

// HandlerPaymentVoucher devuelve los datos para la página de comprobante de
// una recarga — solo si ya está aprobada/cumplida y le pertenece al cliente
// autenticado (mismo chequeo de propiedad que HandlerPaymentStatus).
func HandlerPaymentVoucher(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, err := uuid.Parse(customerIDStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id invalido"})
			return
		}
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id invalido"})
			return
		}
		tx, err := db.GetPaymentTransaction(database, id)
		if err != nil || tx.CustomerID != customerID {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "comprobante no encontrado"})
			return
		}
		if tx.Status != "approved" && tx.Status != "fulfilled" {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "este pago todavía no tiene comprobante"})
			return
		}
		customer, err := db.GetCustomerByID(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo cliente"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"voucher": gin.H{
				"type":          "payment",
				"reference":     strings.ToUpper(id.String()[:8]),
				"customer_name": customer.EpicUsername,
				"product_name":  tx.ProductName,
				"amount_pen":    tx.AmountPEN,
				"kc_amount":     tx.KCAmount,
				"gateway":       tx.Gateway,
				"external_id":   tx.ExternalID,
				"status":        tx.Status,
				"created_at":    tx.CreatedAt,
			},
		})
	}
}

// HandlerRechargeVoucher devuelve los datos para la página de comprobante de
// una recarga manual (Yape/Plin aprobada por un admin, o /kc add de Discord)
// — solo si le pertenece al cliente autenticado.
func HandlerRechargeVoucher(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, err := uuid.Parse(customerIDStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id invalido"})
			return
		}
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id invalido"})
			return
		}
		r, err := db.GetKCRechargeByID(database, id)
		if err != nil || r.CustomerID != customerID {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "comprobante no encontrado"})
			return
		}
		customer, err := db.GetCustomerByID(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo cliente"})
			return
		}
		amountSoles := 0.0
		if r.AmountSoles != nil { amountSoles = *r.AmountSoles }
		productName := "Recarga manual de KC"
		if r.Note != nil && *r.Note != "" { productName = *r.Note }
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"voucher": gin.H{
				"type":          "recharge",
				"reference":     strings.ToUpper(id.String()[:8]),
				"customer_name": customer.EpicUsername,
				"product_name":  productName,
				"amount_pen":    amountSoles,
				"kc_amount":     r.AmountKC,
				"gateway":       r.Method,
				"status":        "approved",
				"created_at":    r.CreatedAt,
			},
		})
	}
}

// ==================== CANCELAR PAGO ====================

// HandlerCancelPayment permite al cliente marcar su propio pago pendiente
// como fallido en cuanto la pasarela le confirma en el navegador que lo
// canceló — sin esto, la transacción se queda "pendiente" en su historial
// hasta que el barrido automático la expira, hasta 30 minutos después.
func HandlerCancelPayment(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, err := uuid.Parse(customerIDStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id invalido"})
			return
		}
		txID, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id invalido"})
			return
		}

		cancelled, err := db.CancelPendingPayment(database, txID, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error cancelando pago"})
			return
		}
		// Si no se canceló nada (no era tuya, o ya no estaba pending — pudo
		// aprobarse justo en este instante) no es un error: el estado real de la
		// transacción es el que ya tiene, y el frontend lo vuelve a consultar.
		c.JSON(http.StatusOK, gin.H{"success": true, "cancelled": cancelled})
	}
}

// ==================== MERCADOPAGO ====================

func createMercadoPagoPreference(tx db.PaymentTransactionInput) (string, string, error) {
	if paymentCfg.MercadoPagoToken == "" {
		return "", "", fmt.Errorf("MercadoPago not configured")
	}

	payload := map[string]interface{}{
		"items": []map[string]interface{}{{
			"title":       tx.ProductName,
			"quantity":    1,
			"unit_price":  tx.AmountPEN,
			"currency_id": "PEN",
		}},
		"external_reference": tx.ID.String(),
	}
	isLocal := strings.Contains(paymentCfg.FrontendURL, "localhost")
	// MercadoPago rejects localhost URLs — only set back_urls and notification_url for production
	if !isLocal {
		payload["back_urls"] = map[string]string{
			"success": fmt.Sprintf("%s/payment/return?id=%s&status=success", paymentCfg.FrontendURL, tx.ID),
			"failure": fmt.Sprintf("%s/payment/return?id=%s&status=failure", paymentCfg.FrontendURL, tx.ID),
			"pending": fmt.Sprintf("%s/payment/return?id=%s&status=pending", paymentCfg.FrontendURL, tx.ID),
		}
		payload["auto_return"] = "approved"
		payload["notification_url"] = fmt.Sprintf("%s/store/webhook/mercadopago", paymentCfg.BackendURL)
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", "https://api.mercadopago.com/checkout/preferences", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+paymentCfg.MercadoPagoToken)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("MP request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		return "", "", fmt.Errorf("MP error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ID       string `json:"id"`
		InitPoint string `json:"init_point"`
	}
	json.Unmarshal(respBody, &result)
	return result.InitPoint, result.ID, nil
}

// ==================== PAYPAL ====================

func getPayPalAccessToken() (string, error) {
	if paymentCfg.PayPalClientID == "" {
		return "", fmt.Errorf("PayPal not configured")
	}

	baseURL := "https://api-m.sandbox.paypal.com"
	if paymentCfg.PayPalMode == "live" {
		baseURL = "https://api-m.paypal.com"
	}

	req, _ := http.NewRequest("POST", baseURL+"/v1/oauth2/token", bytes.NewBufferString("grant_type=client_credentials"))
	req.SetBasicAuth(paymentCfg.PayPalClientID, paymentCfg.PayPalClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken string `json:"access_token"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if result.AccessToken == "" {
		return "", fmt.Errorf("failed to get PayPal token")
	}
	return result.AccessToken, nil
}

func createPayPalOrder(tx db.PaymentTransactionInput) (string, string, error) {
	token, err := getPayPalAccessToken()
	if err != nil {
		return "", "", err
	}

	baseURL := "https://api-m.sandbox.paypal.com"
	if paymentCfg.PayPalMode == "live" {
		baseURL = "https://api-m.paypal.com"
	}

	payload := map[string]interface{}{
		"intent": "CAPTURE",
		"purchase_units": []map[string]interface{}{{
			"reference_id": tx.ID.String(),
			"description":  tx.ProductName,
			"amount": map[string]interface{}{
				"currency_code": "USD",
				"value":         fmt.Sprintf("%.2f", tx.AmountUSD),
			},
		}},
		"application_context": map[string]interface{}{
			"return_url": fmt.Sprintf("%s/payment/return?id=%s&status=success&gateway=paypal", paymentCfg.FrontendURL, tx.ID),
			"cancel_url": fmt.Sprintf("%s/payment/return?id=%s&status=failure", paymentCfg.FrontendURL, tx.ID),
		},
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", baseURL+"/v2/checkout/orders", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("PayPal request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 {
		return "", "", fmt.Errorf("PayPal error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ID    string `json:"id"`
		Links []struct {
			Href string `json:"href"`
			Rel  string `json:"rel"`
		} `json:"links"`
	}
	json.Unmarshal(respBody, &result)

	var approveURL string
	for _, link := range result.Links {
		if link.Rel == "approve" {
			approveURL = link.Href
			break
		}
	}
	return approveURL, result.ID, nil
}

// getPayPalOrder consulta el estado real de una orden directamente en la API
// de PayPal, usando nuestras propias credenciales. Nunca hay que confiar en el
// contenido de un webhook o de un parámetro de la URL para decidir si un pago
// se aprobó — cualquiera podría forjar esa llamada. Esta es la única fuente de
// verdad: si PayPal dice que la orden está COMPLETED y a qué reference_id
// (nuestro txID) pertenece, recién ahí se acredita el pago.
func getPayPalOrder(orderID string) (status, referenceID string, err error) {
	token, err := getPayPalAccessToken()
	if err != nil {
		return "", "", err
	}

	baseURL := "https://api-m.sandbox.paypal.com"
	if paymentCfg.PayPalMode == "live" {
		baseURL = "https://api-m.paypal.com"
	}

	// orderID puede venir de un webhook público sin firmar, o del query
	// param ?token= en /store/paypal-capture (nadie lo autentica antes de
	// esta función) — nunca se interpola crudo en la URL.
	req, err := http.NewRequest("GET", baseURL+"/v2/checkout/orders/"+url.PathEscape(orderID), nil)
	if err != nil {
		return "", "", fmt.Errorf("PayPal: orderID inválido: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("PayPal order query failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("PayPal order query error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Status        string `json:"status"`
		PurchaseUnits []struct {
			ReferenceID string `json:"reference_id"`
		} `json:"purchase_units"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", "", fmt.Errorf("PayPal: respuesta de orden inesperada: %s", string(respBody))
	}
	if len(result.PurchaseUnits) == 0 {
		return result.Status, "", nil
	}
	return result.Status, result.PurchaseUnits[0].ReferenceID, nil
}

func capturePayPalOrder(orderID string) error {
	token, err := getPayPalAccessToken()
	if err != nil {
		return err
	}

	baseURL := "https://api-m.sandbox.paypal.com"
	if paymentCfg.PayPalMode == "live" {
		baseURL = "https://api-m.paypal.com"
	}

	req, err := http.NewRequest("POST", baseURL+"/v2/checkout/orders/"+url.PathEscape(orderID)+"/capture", nil)
	if err != nil {
		return fmt.Errorf("PayPal: orderID inválido: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("capture failed %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// nowPaymentsStatus consulta el estado real de un pago directamente en la API
// de NOWPayments, usando nuestra propia API key — el IPN que llega al webhook
// no viene firmado, así que no se le puede creer a ciegas su contenido.
func nowPaymentsStatus(paymentID int64) (status string, orderID string, err error) {
	if paymentCfg.NOWPaymentsAPIKey == "" {
		return "", "", fmt.Errorf("NOWPayments not configured")
	}

	req, err := http.NewRequest("GET", fmt.Sprintf("https://api.nowpayments.io/v1/payment/%d", paymentID), nil)
	if err != nil {
		return "", "", fmt.Errorf("NOWPayments: error construyendo request: %w", err)
	}
	req.Header.Set("x-api-key", paymentCfg.NOWPaymentsAPIKey)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("NOWPayments status request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("NOWPayments status error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		PaymentStatus string `json:"payment_status"`
		OrderID       string `json:"order_id"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", "", fmt.Errorf("NOWPayments: respuesta de estado inesperada: %s", string(respBody))
	}
	return result.PaymentStatus, result.OrderID, nil
}

// ==================== NOWPAYMENTS ====================

func createNOWPaymentsInvoice(tx db.PaymentTransactionInput) (string, string, error) {
	if paymentCfg.NOWPaymentsAPIKey == "" {
		return "", "", fmt.Errorf("NOWPayments not configured")
	}

	payload := map[string]interface{}{
		"price_amount":   tx.AmountUSD,
		"price_currency": "usd",
		"order_id":       tx.ID.String(),
		"order_description": tx.ProductName,
		"ipn_callback_url":  fmt.Sprintf("%s/store/webhook/nowpayments", paymentCfg.BackendURL),
		"success_url":       fmt.Sprintf("%s/payment/return?id=%s&status=success", paymentCfg.FrontendURL, tx.ID),
		"cancel_url":        fmt.Sprintf("%s/payment/return?id=%s&status=failure", paymentCfg.FrontendURL, tx.ID),
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", "https://api.nowpayments.io/v1/invoice", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", paymentCfg.NOWPaymentsAPIKey)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("NOWPayments request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return "", "", fmt.Errorf("NOWPayments error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ID         string `json:"id"`
		InvoiceURL string `json:"invoice_url"`
	}
	json.Unmarshal(respBody, &result)

	if result.InvoiceURL == "" {
		return "", "", fmt.Errorf("NOWPayments no invoice_url: %s", string(respBody))
	}

	return result.InvoiceURL, fmt.Sprintf("%v", result.ID), nil
}

// ==================== DLOCAL GO ====================
// Pasarela para clientes fuera de Peru — cobra tarjetas internacionales y
// metodos locales en la divisa real del cliente (no solo PEN/USD).
// Docs: https://docs.dlocalgo.com/integration-api/welcome-to-dlocal-go-api

func dlocalGoBaseURL() string {
	if paymentCfg.DLocalGoSandbox {
		return "https://api-sbx.dlocalgo.com"
	}
	return "https://api.dlocalgo.com"
}

// convertPENToCurrency convierte un precio en PEN a la divisa indicada,
// usando el mismo tipo de cambio que ya usa el sitio para mostrar precios
// de referencia — así el monto que se cobra coincide con lo que el cliente
// vio en pantalla.
func convertPENToCurrency(pricePEN float64, currencyCode string) (float64, error) {
	if currencyCode == "PEN" {
		return pricePEN, nil
	}
	rates := currentConversionRates()
	rate, ok := rates[currencyCode]
	if !ok || rate <= 0 {
		return 0, fmt.Errorf("no se pudo obtener el tipo de cambio para %s", currencyCode)
	}
	return roundCents(pricePEN * rate), nil
}

func roundCents(n float64) float64 {
	return float64(int64(n*100+0.5)) / 100
}

func createDLocalGoPayment(tx db.PaymentTransactionInput) (string, string, error) {
	if paymentCfg.DLocalGoAPIKey == "" || paymentCfg.DLocalGoSecretKey == "" {
		return "", "", fmt.Errorf("dLocal Go aún no está configurado")
	}

	payload := map[string]interface{}{
		"amount":            tx.AmountLocal,
		"currency":          tx.CurrencyCode,
		"order_id":          tx.ID.String(),
		"description":       tx.ProductName,
		"notification_url":  fmt.Sprintf("%s/store/webhook/dlocalgo", paymentCfg.BackendURL),
		"success_url":       fmt.Sprintf("%s/payment/return?id=%s&status=success", paymentCfg.FrontendURL, tx.ID),
		"back_url":          fmt.Sprintf("%s/payment/return?id=%s&status=failure", paymentCfg.FrontendURL, tx.ID),
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", dlocalGoBaseURL()+"/v1/payments", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s:%s", paymentCfg.DLocalGoAPIKey, paymentCfg.DLocalGoSecretKey))

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("dLocal Go request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return "", "", fmt.Errorf("dLocal Go error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		ID          string `json:"id"`
		RedirectURL string `json:"redirect_url"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", "", fmt.Errorf("dLocal Go: respuesta inesperada: %s", string(respBody))
	}
	if result.RedirectURL == "" {
		return "", "", fmt.Errorf("dLocal Go no devolvió redirect_url: %s", string(respBody))
	}

	return result.RedirectURL, result.ID, nil
}

// dlocalGoPaymentStatus consulta el estado real de un pago — se usa tras
// recibir la notificación (que solo trae el ID de dLocal, sin más datos).
// order_id es el nuestro (el que mandamos al crear el pago), y nos permite
// ubicar la transacción interna sin necesitar una tabla de mapeo aparte —
// igual que hace MercadoPago con su external_reference.
func dlocalGoPaymentStatus(paymentID string) (status string, orderID string, err error) {
	req, err := http.NewRequest("GET", dlocalGoBaseURL()+"/v1/payments/"+url.PathEscape(paymentID), nil)
	if err != nil {
		return "", "", fmt.Errorf("dLocal Go: paymentID inválido: %w", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s:%s", paymentCfg.DLocalGoAPIKey, paymentCfg.DLocalGoSecretKey))

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("dLocal Go status request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("dLocal Go status error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Status  string `json:"status"` // PENDING, PAID, REJECTED, CANCELLED, EXPIRED
		OrderID string `json:"order_id"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", "", fmt.Errorf("dLocal Go: respuesta de estado inesperada: %s", string(respBody))
	}
	return result.Status, result.OrderID, nil
}

// verifyDLocalGoSignature valida la firma HMAC-SHA256 de una notificación:
// HMAC-SHA256(secretKey, apiKey + payload) debe coincidir con la firma recibida.
func verifyDLocalGoSignature(rawBody []byte, signatureHeader string) bool {
	const prefix = "V2-HMAC-SHA256, Signature: "
	if !strings.HasPrefix(signatureHeader, prefix) {
		return false
	}
	receivedSig := strings.TrimPrefix(signatureHeader, prefix)

	mac := hmac.New(sha256.New, []byte(paymentCfg.DLocalGoSecretKey))
	mac.Write([]byte(paymentCfg.DLocalGoAPIKey))
	mac.Write(rawBody)
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(receivedSig), []byte(expectedSig))
}
