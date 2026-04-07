package store

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/middleware"
	"bytes"
	"database/sql"
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

// Default exchange rate PEN → USD (fallback when API is unavailable)
const defaultUSDRate = 0.27

// ==================== CONFIG ====================

type PaymentConfig struct {
	MercadoPagoToken    string
	PayPalClientID      string
	PayPalClientSecret  string
	PayPalMode          string // sandbox or live
	NOWPaymentsAPIKey   string
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
	// KC packages (online prices with MP commission)
	"starter": {Name: "Starter 800 KC", PricePEN: 14.70, KCAmount: 800},
	"gamer":   {Name: "Gamer 2,400 KC", PricePEN: 41.60, KCAmount: 2400},
	"pro":     {Name: "Pro 4,500 KC", PricePEN: 76.80, KCAmount: 4500},
	"legend":  {Name: "Legend 12,500 KC", PricePEN: 211.30, KCAmount: 12500},
	// V-Bucks (names MUST match autobuyer products.json)
	"vb-800":   {Name: "800 V-Bucks", PricePEN: 23.20},
	"vb-2400":  {Name: "2400 V-Bucks", PricePEN: 53.60},
	"vb-4500":  {Name: "4500 V-Bucks", PricePEN: 79.80},
	"vb-12500": {Name: "12500 V-Bucks", PricePEN: 190.00},
	// Packs
	"pack-koi":   {Name: "Pack de Reino Koi", PricePEN: 23.20},
	"pack-drift": {Name: "Pack de Deriva Infinita", PricePEN: 23.20},
	"pack-brite": {Name: "Operation Brite", PricePEN: 13.70},
	// Club (name MUST match autobuyer card_products.json)
	"club-monthly": {Name: "Fortnite Crew", PricePEN: 22.10},
	// Rocket League (names MUST match autobuyer products.json with em dash)
	"rl-500":  {Name: "500 \u2014 RL Credits", PricePEN: 13.70},
	"rl-1100": {Name: "1100 \u2014 RL Credits", PricePEN: 26.30},
	"rl-3000": {Name: "3000 \u2014 RL Credits", PricePEN: 59.90},
	"rl-6500": {Name: "6500 \u2014 RL Credits", PricePEN: 116.50},
}

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
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		// Validate gateway
		if req.Gateway != "mercadopago" && req.Gateway != "paypal" && req.Gateway != "nowpayments" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "gateway invalido"})
			return
		}

		// Lookup product price or use custom
		var productName string
		var pricePEN float64
		var kcAmount int

		if req.CustomPrice > 0 && req.CustomName != "" {
			// Custom product (bulk V-Bucks, custom KC, bulk RL credits)
			productName = req.CustomName
			pricePEN = req.CustomPrice
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

		// If payment is pending, check with MercadoPago directly (for localhost without webhooks)
		if tx.Status == "pending" && tx.Gateway == "mercadopago" && tx.ExternalID != "" && paymentCfg.MercadoPagoToken != "" {
			go func() {
				req, _ := http.NewRequest("GET",
					"https://api.mercadopago.com/checkout/preferences/"+tx.ExternalID, nil)
				req.Header.Set("Authorization", "Bearer "+paymentCfg.MercadoPagoToken)
				// Also check payments by external_reference
				reqPay, _ := http.NewRequest("GET",
					"https://api.mercadopago.com/v1/payments/search?external_reference="+tx.ID.String(), nil)
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
				_ = req // suppress unused
				if len(result.Results) > 0 && result.Results[0].Status == "approved" {
					processApprovedPayment(database, tx.ID)
				}
			}()
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "transaction": tx})
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

func capturePayPalOrder(orderID string) error {
	token, err := getPayPalAccessToken()
	if err != nil {
		return err
	}

	baseURL := "https://api-m.sandbox.paypal.com"
	if paymentCfg.PayPalMode == "live" {
		baseURL = "https://api-m.paypal.com"
	}

	req, _ := http.NewRequest("POST", baseURL+"/v2/checkout/orders/"+orderID+"/capture", nil)
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
