package store

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/middleware"
	"KidStoreStore/src/safe"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
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
	// NOWPaymentsIPNSecret firma los callbacks IPN (header x-nowpayments-sig)
	// — se genera aparte de NOWPaymentsAPIKey, en el dashboard de
	// NOWPayments (Payment Settings → Instant Payment Notifications). Ver
	// verifyNOWPaymentsSignature.
	NOWPaymentsIPNSecret string
	DLocalGoAPIKey      string
	DLocalGoSecretKey   string
	DLocalGoSandbox     bool
	FrontendURL         string
	BackendURL          string
}

var paymentCfg PaymentConfig

// nowPaymentsBaseURL apunta a la API real de NOWPayments — variable (en
// vez de un literal embebido) para que las pruebas puedan redirigirlo a un
// httptest.Server que simule respuestas sin llamar a NOWPayments de verdad.
var nowPaymentsBaseURL = "https://api.nowpayments.io/v1"

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

		// CreditPaymentOnce (db.go) solo acredita KC cuando payment_type es
		// exactamente "kc_recharge" — "product_purchase" existe en el schema
		// (activation_code, autobuyer_task_id) pero nunca se implementó en
		// ningún lado. Sin este chequeo, un payment_type distinto de
		// "kc_recharge" (typo, cliente desactualizado, o alguien pegándole
		// directo a la API) crea un cobro real que la pasarela aprueba y el
		// webhook confirma correctamente, pero que nunca se acredita — el
		// cliente paga y no recibe nada, sin ninguna alerta ni forma
		// automática de recuperarlo. El frontend actual solo manda
		// "kc_recharge", así que este chequeo no bloquea ningún flujo real.
		if req.PaymentType != "kc_recharge" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "payment_type invalido"})
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

		// Antes esto siempre usaba defaultUSDRate (un valor fijo, pensado
		// como respaldo para cuando la API de tasas no responde) para TODOS
		// los pagos en USD, mientras que dLocal Go sí usaba la tasa real y
		// actualizada vía convertPENToCurrency/currentConversionRates — dos
		// clientes pagando el mismo paquete de KC el mismo día podían pagar
		// montos distintos en USD según qué pasarela usaran, y esa
		// diferencia solo crecería con el tiempo si el tipo de cambio
		// real se aleja de 0.27. Ahora todas las pasarelas en USD (PayPal,
		// NOWPayments) usan la misma tasa en vivo que dLocal Go; si la API
		// de tasas falla, convertPENToCurrency ya cae sola al mismo
		// defaultUSDRate como respaldo (vía fallbackRates en shop.go), así
		// que el comportamiento de resguardo no cambia.
		amountUSD, err := convertPENToCurrency(pricePEN, "USD")
		if err != nil {
			amountUSD = pricePEN * defaultUSDRate
		}

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
				approved, err := mercadoPagoStatusByReference(txID.String())
				if err != nil {
					slog.Warn("MP status poll falló", "txID", txID, "error", err)
					return
				}
				if approved {
					processApprovedPayment(database, txID)
				}
			})
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "transaction": tx})
	}
}

// ==================== COMPROBANTE DE PAGO ====================

// ChargedAmountAndCurrency devuelve el monto y la divisa que la pasarela
// REALMENTE cobró — antes el comprobante siempre mostraba "S/ {amount_pen}"
// sin importar la pasarela, pero amount_pen es solo un precio de
// REFERENCIA que se calcula al crear el pago; PayPal y NOWPayments cobran
// en USD, y dLocal Go cobra en la divisa real del cliente (currency_code/
// amount_local, ya guardados desde HandlerCreatePayment). Mostrar "S/" para
// un cobro que en realidad fue en USD o en otra divisa es directamente
// incorrecto, no solo impreciso.
func ChargedAmountAndCurrency(gateway string, amountPEN, amountUSD, amountLocal float64, currencyCode string) (amount float64, currency string) {
	switch gateway {
	case "paypal", "nowpayments":
		return amountUSD, "USD"
	case "dlocalgo":
		if currencyCode != "" {
			return amountLocal, currencyCode
		}
		return amountUSD, "USD"
	default: // mercadopago, manual (yape/plin/transferencia) — siempre en soles
		return amountPEN, "PEN"
	}
}

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
		chargedAmount, chargedCurrency := ChargedAmountAndCurrency(tx.Gateway, tx.AmountPEN, tx.AmountUSD, tx.AmountLocal, tx.CurrencyCode)
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"voucher": gin.H{
				"type":             "payment",
				"reference":        strings.ToUpper(id.String()[:8]),
				"customer_name":    customer.EpicUsername,
				"product_name":     tx.ProductName,
				"amount_pen":       tx.AmountPEN,
				"charged_amount":   chargedAmount,
				"charged_currency": chargedCurrency,
				"kc_amount":        tx.KCAmount,
				"gateway":          tx.Gateway,
				"external_id":      tx.ExternalID,
				"status":           tx.Status,
				"created_at":       tx.CreatedAt,
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

		// r.PaymentTransactionID no es nil cuando esta fila de kc_recharges
		// es la acreditación automática de un pago por pasarela (ver
		// CreditPaymentOnce) — no una recarga manual. El enlace
		// /dashboard/comprobantes/recarga/:id sigue funcionando igual (nunca
		// se rompe un enlace viejo, sea de un correo o guardado en algún
		// lado), pero acá se reconoce el caso y se usa el importe y la
		// divisa REALES que cobró esa pasarela — mostrar siempre "S/" sería
		// directamente incorrecto para PayPal/NOWPayments (cobran en USD) o
		// dLocal Go (cobra en la divisa real del cliente).
		if r.PaymentTransactionID != nil {
			tx, txErr := db.GetPaymentTransaction(database, *r.PaymentTransactionID)
			if txErr == nil && tx.CustomerID == customerID {
				chargedAmount, chargedCurrency := ChargedAmountAndCurrency(tx.Gateway, tx.AmountPEN, tx.AmountUSD, tx.AmountLocal, tx.CurrencyCode)
				c.JSON(http.StatusOK, gin.H{
					"success": true,
					"voucher": gin.H{
						"type":             "recharge",
						"reference":        strings.ToUpper(id.String()[:8]),
						"customer_name":    customer.EpicUsername,
						"product_name":     tx.ProductName,
						"amount_pen":       tx.AmountPEN,
						"charged_amount":   chargedAmount,
						"charged_currency": chargedCurrency,
						"kc_amount":        r.AmountKC,
						"gateway":          tx.Gateway,
						"status":           "approved",
						"created_at":       r.CreatedAt,
					},
				})
				return
			}
			// Si por lo que sea el pago vinculado ya no se puede leer (fila
			// borrada, error transitorio de DB), NUNCA se debe caer al bloque
			// de "recarga manual" de más abajo: ese usa r.AmountSoles, que acá
			// es solo el equivalente en PEN de referencia que CreditPaymentOnce
			// guardó al acreditar (db.go) — no necesariamente lo que la
			// pasarela cobró de verdad (PayPal/NOWPayments cobran en USD,
			// dLocal Go en la divisa real del cliente). Presentarlo como
			// "amount_pen"/"charged_currency: PEN" afirmaría una divisa que
			// puede ser incorrecta. Se responde igual con éxito (el
			// comprobante existe y el enlace sigue funcionando) pero sin
			// inventar importe ni divisa — el frontend ya sabe mostrar "monto
			// no disponible" cuando charged_amount/charged_currency vienen
			// vacíos (ver Voucher.tsx, hasChargedInfo).
			if txErr != nil {
				slog.Warn("HandlerRechargeVoucher: no se pudo leer el pago vinculado, se muestra el comprobante sin importe/divisa en vez de inventarlos", "rechargeID", id, "paymentID", *r.PaymentTransactionID, "error", txErr)
				productName := "Recarga vía pasarela"
				if r.Note != nil && *r.Note != "" { productName = *r.Note }
				c.JSON(http.StatusOK, gin.H{
					"success": true,
					"voucher": gin.H{
						"type":               "recharge",
						"reference":          strings.ToUpper(id.String()[:8]),
						"customer_name":      customer.EpicUsername,
						"product_name":       productName,
						"amount_pen":         nil,
						"charged_amount":     nil,
						"charged_currency":   nil,
						"kc_amount":          r.AmountKC,
						"gateway":            r.Method,
						"status":             "approved",
						"created_at":         r.CreatedAt,
						"amount_unavailable": true,
					},
				})
				return
			}
		}

		amountSoles := 0.0
		if r.AmountSoles != nil { amountSoles = *r.AmountSoles }
		productName := "Recarga manual de KC"
		if r.Note != nil && *r.Note != "" { productName = *r.Note }
		// Recarga manual genuina (Yape/Plin/transferencia, o /kc add de
		// Discord) — siempre se cobra en soles, no hay ninguna pasarela de
		// por medio. Cuando amountSoles es 0 (recarga hecha desde Discord sin
		// registrar un monto en soles), no se inventa un importe: se omite el
		// campo en vez de mostrar "S/ 0.00" como si esa hubiera sido la cifra
		// real cobrada.
		var chargedAmount interface{}
		var chargedCurrency interface{}
		if amountSoles > 0 {
			chargedAmount = amountSoles
			chargedCurrency = "PEN"
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"voucher": gin.H{
				"type":             "recharge",
				"reference":        strings.ToUpper(id.String()[:8]),
				"customer_name":    customer.EpicUsername,
				"product_name":     productName,
				"amount_pen":       amountSoles,
				"charged_amount":   chargedAmount,
				"charged_currency": chargedCurrency,
				"kc_amount":        r.AmountKC,
				"gateway":          r.Method,
				"status":           "approved",
				"created_at":       r.CreatedAt,
			},
		})
	}
}

// ==================== CANCELAR PAGO ====================

// HandlerCancelPayment lo llama el frontend cuando el cliente cierra la
// ventana de pago o se agota el timeout de espera (~6 min) sin un status
// definitivo. NINGUNA de esas dos cosas demuestra que el pago haya
// fallado — una transferencia, una confirmación cripto o incluso una
// tarjeta pueden seguir procesándose del lado de la pasarela después de
// eso. Antes esto marcaba el pago "failed" sin más, lo que además lo sacaba
// para siempre de ReconcilePendingPayments (que solo revisa status='pending')
// — si luego SÍ llegaba una confirmación tardía, el webhook igual la
// acreditaría (CreditPaymentOnce no depende del status), pero si el webhook
// nunca llegaba, el pago quedaba "failed" para siempre sin que nada volviera
// a consultar a la pasarela.
//
// Ahora, antes de tocar nada, se consulta directamente a la pasarela (misma
// fuente de verdad que usa la reconciliación automática):
//   - Si la pasarela confirma que se aprobó → se acredita (nunca se marca
//     fallido un pago que en realidad sí se cobró).
//   - Si la pasarela confirma un rechazo/cancelación DEFINITIVO → recién ahí
//     se marca 'failed'.
//   - Si sigue sin resolverse (pendiente en la pasarela, sin sesión creada
//     aún, o no se pudo consultar) → se deja 'pending', tal cual, para que
//     ReconcilePendingPayments y/o el webhook lo sigan intentando.
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

		tx, err := db.GetPaymentTransaction(database, txID)
		if err != nil || tx.CustomerID != customerID {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "transacción no encontrada"})
			return
		}
		if tx.Status != "pending" {
			// Ya se resolvió por otra vía (webhook, reconciliación, admin) — el
			// frontend vuelve a consultar el estado real, no hay nada que hacer acá.
			c.JSON(http.StatusOK, gin.H{"success": true, "status": tx.Status})
			return
		}

		if tx.ExternalID == "" {
			// Nunca se llegó a crear una sesión real en la pasarela (la llamada
			// para generarla falló, o el cliente nunca fue redirigido) — no hay
			// nada que la pasarela pueda haber cobrado, es seguro marcarlo fallido.
			db.CancelPendingPayment(database, txID, customerID)
			c.JSON(http.StatusOK, gin.H{"success": true, "status": "failed"})
			return
		}

		outcome, gwErr := checkGatewayOutcome(tx)
		switch outcome {
		case gatewayApproved:
			if err := processApprovedPayment(database, txID); err != nil {
				slog.Error("HandlerCancelPayment: error acreditando pago aprobado", "txID", txID, "error", err)
			}
			c.JSON(http.StatusOK, gin.H{"success": true, "status": "approved"})
		case gatewayRejected:
			db.AdminUpdatePaymentStatus(database, txID, "failed")
			c.JSON(http.StatusOK, gin.H{"success": true, "status": "failed"})
		default: // gatewayStillPending, o la consulta a la pasarela falló
			if gwErr != nil {
				slog.Warn("HandlerCancelPayment: no se pudo verificar con la pasarela, se deja pendiente", "txID", txID, "gateway", tx.Gateway, "error", gwErr)
			}
			c.JSON(http.StatusOK, gin.H{"success": true, "status": "pending"})
		}
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

// mercadoPagoPaymentStatus busca el pago en MercadoPago para nuestro
// external_reference (nuestro propio UUID de pago). Es la única forma
// confiable de reconciliar MercadoPago sin esperar al webhook: el
// external_id que guardamos al crear el pago es el ID de la PREFERENCIA (la
// sesión de checkout), no el del pago real — ese solo existe y se conoce
// después de que el cliente paga. La búsqueda por external_reference es lo
// que ya usaba el poll de HandlerPaymentStatus; se extrajo acá para que
// ReconcilePendingPayments (webhooks.go) también pueda reusarla.
//
// Devuelve el status crudo que MercadoPago tiene
// registrado para este txID ("approved", "rejected", "cancelled", "pending",
// "in_process", etc.), o "" si todavía no encuentra ningún pago asociado.
// Se expone el status real (no solo un booleano) porque distinguir "MP dice
// que se rechazó/canceló" de "MP todavía no tiene nada, o sigue pendiente"
// importa para decidir si es seguro marcar un pago como fallido (ver
// checkGatewayOutcome en webhooks.go) — cerrar la ventana de pago o un
// timeout del lado del cliente no son, por sí mismos, ninguna de las dos cosas.
func mercadoPagoPaymentStatus(txID string) (status string, err error) {
	if paymentCfg.MercadoPagoToken == "" {
		return "", fmt.Errorf("MercadoPago not configured")
	}
	req, err := http.NewRequest("GET",
		"https://api.mercadopago.com/v1/payments/search?external_reference="+url.QueryEscape(txID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+paymentCfg.MercadoPagoToken)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		Results []struct {
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	for _, r := range result.Results {
		if r.Status == "approved" {
			return "approved", nil
		}
	}
	if len(result.Results) > 0 {
		return result.Results[0].Status, nil
	}
	return "", nil
}

// mercadoPagoStatusByReference — conserva la firma booleana original para
// los llamadores que solo necesitan saber si ya se aprobó.
func mercadoPagoStatusByReference(txID string) (approved bool, err error) {
	status, err := mercadoPagoPaymentStatus(txID)
	if err != nil {
		return false, err
	}
	return status == "approved", nil
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

// payPalOrderDetails junta todo lo que hace falta para decidir con
// confianza si una orden de PayPal realmente pagó lo que esperábamos — el
// status de la orden por sí solo no alcanza: dice si el flujo de checkout
// terminó, pero no confirma que la CAPTURA del dinero (el cobro real) haya
// quedado completa, ni que el importe/divisa cobrados sean los que fijamos
// al crear la orden.
type payPalOrderDetails struct {
	Status        string // status de la ORDEN: CREATED, APPROVED, COMPLETED, VOIDED, etc.
	ReferenceID   string // nuestro propio txID, tal como lo fijamos al crear la orden
	AmountValue   string // importe realmente capturado (o, a falta de captura, el autorizado)
	CurrencyCode  string // divisa del importe de arriba
	CaptureStatus string // status de la captura más reciente: COMPLETED, DECLINED, PENDING… ("" si aún no hay ninguna)
}

// getPayPalOrder consulta el estado real de una orden directamente en la API
// de PayPal, usando nuestras propias credenciales. Nunca hay que confiar en el
// contenido de un webhook o de un parámetro de la URL para decidir si un pago
// se aprobó — cualquiera podría forjar esa llamada. Esta es la única fuente de
// verdad: si PayPal dice que la orden está COMPLETED, con una captura
// COMPLETED y por el importe/divisa correctos, y a qué reference_id (nuestro
// txID) pertenece, recién ahí se acredita el pago (ver payPalOrderMatchesTx).
func getPayPalOrder(orderID string) (payPalOrderDetails, error) {
	token, err := getPayPalAccessToken()
	if err != nil {
		return payPalOrderDetails{}, err
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
		return payPalOrderDetails{}, fmt.Errorf("PayPal: orderID inválido: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return payPalOrderDetails{}, fmt.Errorf("PayPal order query failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return payPalOrderDetails{}, fmt.Errorf("PayPal order query error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Status        string `json:"status"`
		PurchaseUnits []struct {
			ReferenceID string `json:"reference_id"`
			Amount      struct {
				Value        string `json:"value"`
				CurrencyCode string `json:"currency_code"`
			} `json:"amount"`
			Payments struct {
				Captures []struct {
					Status string `json:"status"`
					Amount struct {
						Value        string `json:"value"`
						CurrencyCode string `json:"currency_code"`
					} `json:"amount"`
				} `json:"captures"`
			} `json:"payments"`
		} `json:"purchase_units"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return payPalOrderDetails{}, fmt.Errorf("PayPal: respuesta de orden inesperada: %s", string(respBody))
	}

	details := payPalOrderDetails{Status: result.Status}
	if len(result.PurchaseUnits) == 0 {
		return details, nil
	}
	pu := result.PurchaseUnits[0]
	details.ReferenceID = pu.ReferenceID
	details.AmountValue = pu.Amount.Value
	details.CurrencyCode = pu.Amount.CurrencyCode
	if n := len(pu.Payments.Captures); n > 0 {
		// La más reciente es la que manda si hubo más de un intento de
		// captura (no debería pasar en nuestro flujo de intent=CAPTURE, pero
		// no cuesta nada ser explícitos sobre cuál se usa).
		cap := pu.Payments.Captures[n-1]
		details.CaptureStatus = cap.Status
		// El importe de la CAPTURA es lo que PayPal realmente cobró — más
		// confiable que el importe autorizado en la orden, que es lo único
		// que queda si todavía no hay ninguna captura.
		if cap.Amount.Value != "" {
			details.AmountValue = cap.Amount.Value
			details.CurrencyCode = cap.Amount.CurrencyCode
		}
	}
	return details, nil
}

// payPalOrderMatchesTx es el chequeo final antes de acreditar KC por un pago
// de PayPal: no alcanza con que la ORDEN diga COMPLETED (ver
// payPalOrderDetails) — hace falta además que exista una captura realmente
// COMPLETED (no DECLINED, PENDING, REFUNDED, etc.) y que el importe/divisa
// que PayPal dice haber cobrado coincidan con lo que esta transacción
// esperaba cobrar. Sin esto, un status "COMPLETED" a nivel de orden podía
// tomarse como aprobación aunque la captura real hubiera fallado o el
// importe cobrado no fuera el esperado.
func payPalOrderMatchesTx(order payPalOrderDetails, expectedUSD float64) bool {
	if order.Status != "COMPLETED" {
		return false
	}
	if order.CaptureStatus != "COMPLETED" {
		return false
	}
	if order.CurrencyCode != "USD" {
		return false
	}
	amount, err := strconv.ParseFloat(order.AmountValue, 64)
	if err != nil {
		return false
	}
	// Margen de un centavo para tolerar redondeo de punto flotante entre lo
	// que calculamos al crear la orden (fmt.Sprintf("%.2f", …)) y lo que
	// PayPal devuelve como string — nunca para tolerar un importe distinto.
	return math.Abs(amount-expectedUSD) < 0.01
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

	req, err := http.NewRequest("GET", fmt.Sprintf("%s/payment/%d", nowPaymentsBaseURL, paymentID), nil)
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
	// Si el secreto no está configurado, HMAC(clave vacía, ...) es un valor
	// fijo y calculable por cualquiera — la firma dejaría de verificar nada
	// en la práctica. Mejor rechazar todo explícitamente que validar contra
	// un secreto que no es tal.
	if paymentCfg.DLocalGoSecretKey == "" {
		return false
	}
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

// sortJSONForNOWPaymentsSignature reordena alfabéticamente (de forma
// recursiva, en todos los niveles) las claves del JSON crudo de un IPN de
// NOWPayments, y lo vuelve a serializar de forma compacta — el mismo paso
// de "sort object keys recursively + JSON.stringify" que describen los
// ejemplos oficiales en Node.js/PHP/Python. Separada de
// verifyNOWPaymentsSignature para poder probarla por sí sola con casos
// conocidos (incluyendo el ejemplo de la propia documentación).
func sortJSONForNOWPaymentsSignature(rawBody []byte) ([]byte, error) {
	// UseNumber() evita decodificar los números como float64 — eso
	// redondearía enteros grandes y podría reformatear decimales al volver
	// a serializar, produciendo bytes distintos de los que NOWPayments usó
	// para firmar. json.Number preserva el literal exacto tal como llegó.
	var parsed interface{}
	dec := json.NewDecoder(bytes.NewReader(rawBody))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("NOWPayments: cuerpo del IPN no es JSON válido: %w", err)
	}

	// encoding/json ordena alfabéticamente las claves de cualquier
	// map[string]any al serializarlo, en TODOS los niveles de anidamiento
	// (recursivo por construcción, porque cada objeto anidado se serializa
	// con la misma regla) — exactamente el "sort object keys recursively"
	// que piden los ejemplos oficiales, sin necesidad de implementarlo a mano.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Por default, Marshal/Encoder escapan '<', '>' y '&' como <, etc.
	// — JSON.stringify/json_encode/json.dumps (los lenguajes de los
	// ejemplos oficiales de NOWPayments) no lo hacen. Si algún campo del
	// pedido (p. ej. order_description) tuviera alguno de esos caracteres,
	// dejar el escape por defecto activado produciría una firma distinta a
	// la que NOWPayments calculó.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(parsed); err != nil {
		return nil, fmt.Errorf("NOWPayments: error re-serializando el cuerpo ordenado: %w", err)
	}
	// Encoder.Encode agrega un '\n' final que JSON.stringify no tiene.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// verifyNOWPaymentsSignature valida la firma HMAC-SHA512 de un IPN de
// NOWPayments siguiendo su documentación oficial
// (https://nowpayments.zendesk.com/hc/en-us/articles/21395546303389):
// las claves del cuerpo recibido se ordenan alfabéticamente en todos los
// niveles, el resultado se firma con HMAC-SHA512 usando el IPN secret key
// (una clave DISTINTA de la API key, generada aparte en el dashboard de
// NOWPayments), y esa firma debe coincidir con el header x-nowpayments-sig.
// Sin esto, cualquiera podía mandar un POST fabricado a
// /store/webhook/nowpayments haciéndose pasar por una notificación real —
// el handler solo consultaba el estado real del pago DESPUÉS de aceptar
// cualquier payment_id que viniera en el cuerpo, así que un atacante que
// conociera o adivinara el payment_id de un pago pendiente ajeno podía
// forzar que se reconsultara (sin poder alterar el resultado real, gracias
// a processNOWPaymentsPaymentID), pero sí podía generar ruido/reintentos
// arbitrarios y saturar webhook_events con eventos falsos.
func verifyNOWPaymentsSignature(rawBody []byte, sigHeader string) bool {
	// Sin secreto configurado no hay nada contra qué comparar — mejor
	// rechazar todo explícitamente que aceptar cualquier cosa sin firma.
	if paymentCfg.NOWPaymentsIPNSecret == "" {
		return false
	}
	if sigHeader == "" {
		return false
	}

	sortedJSON, err := sortJSONForNOWPaymentsSignature(rawBody)
	if err != nil {
		return false
	}

	mac := hmac.New(sha512.New, []byte(paymentCfg.NOWPaymentsIPNSecret))
	mac.Write(sortedJSON)
	expectedSig := hex.EncodeToString(mac.Sum(nil))

	// NOWPayments manda el hex en minúsculas; se normaliza por si acaso para
	// no rechazar por una diferencia de mayúsculas/minúsculas que no afecta
	// la validez criptográfica de la firma.
	return hmac.Equal([]byte(strings.ToLower(sigHeader)), []byte(expectedSig))
}
