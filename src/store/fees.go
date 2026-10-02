package store

// Comisiones a cargo del cliente: la tienda siempre recibe el precio exacto
// del paquete. Mercado Pago y PayPal cobran su comisión sobre el TOTAL (más
// IGV en Mercado Pago), así que no basta con sumar el porcentaje: el total se
// calcula "hacia atrás" para que, después de la comisión, quede justo el
// precio. En cripto, NOWPayments cobra su comisión y la de la red al cliente
// ("Fee Paid by User"). Bizum se cobra en euros y luego se envía por remesa
// (comisión + tipo de cambio peor que el de mercado): el recargo cubre ambas.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"
	"KidStoreStore/src/types"

	"github.com/gin-gonic/gin"
)

// GatewayFee — tarifa de una pasarela automática.
type GatewayFee struct {
	Percent float64 `json:"percent"` // % de la pasarela sobre el total cobrado
	Fixed   float64 `json:"fixed"`   // cargo fijo por operación (S/ en Mercado Pago, US$ en PayPal y cripto)
	Tax     float64 `json:"tax"`     // % de impuesto sobre la comisión (IGV)
	Margin  float64 `json:"margin"`  // % extra de seguridad (opcional)
}

// RemittanceFee — costo de traer a Perú lo cobrado por Bizum.
type RemittanceFee struct {
	Percent  float64 `json:"percent"`   // % que cobra la remesa
	Fixed    float64 `json:"fixed"`     // cargo fijo por envío (€)
	FXMargin float64 `json:"fx_margin"` // cuánto peor es el cambio de la remesa que el de mercado (%)
}

type PaymentFees struct {
	MercadoPago GatewayFee    `json:"mercadopago"`
	PayPal      GatewayFee    `json:"paypal"`
	NOWPayments GatewayFee    `json:"nowpayments"` // extra opcional: su comisión ya la paga el cliente
	Bizum       RemittanceFee `json:"bizum"`
}

const paymentFeesKey = "payment_fees"

// Tarifas públicas: Mercado Pago Perú con dinero disponible al instante
// (3.49% + S/1 + IGV) y PayPal Perú para cobros del extranjero (5.4% + 1.5%
// internacional + US$0.30). El admin las ajusta en el panel con las de su
// cuenta. NOWPayments: su comisión ya la paga el cliente (0 = nada extra).
var defaultPaymentFees = PaymentFees{
	MercadoPago: GatewayFee{Percent: 3.49, Fixed: 1.00, Tax: 18},
	PayPal:      GatewayFee{Percent: 6.9, Fixed: 0.30},
	Bizum:       RemittanceFee{Percent: 1.5},
}

var (
	paymentFeesMu      sync.RWMutex
	paymentFees        = defaultPaymentFees
	paymentFeesBy      string
	paymentFeesUpdated time.Time
)

// CurrentPaymentFees — las comisiones vigentes.
func CurrentPaymentFees() PaymentFees {
	paymentFeesMu.RLock()
	defer paymentFeesMu.RUnlock()
	return paymentFees
}

// LoadPaymentFees lee las comisiones guardadas (al arrancar). Si no hay
// ninguna guardada todavía, quedan las de por defecto.
func LoadPaymentFees(database *sql.DB) {
	f := defaultPaymentFees // lo que falte en lo guardado queda con el valor por defecto
	ok, by, at, err := db.GetSetting(database, paymentFeesKey, &f)
	if err != nil {
		slog.Error("Comisiones: no se pudieron leer, se usan las de por defecto", "error", err)
		return
	}
	if !ok || f.validate() != nil {
		return
	}
	paymentFeesMu.Lock()
	paymentFees, paymentFeesBy, paymentFeesUpdated = f, by, at
	paymentFeesMu.Unlock()
}

func between(v, lo, hi float64) bool { return !math.IsNaN(v) && v >= lo && v <= hi }

func (g GatewayFee) valid() bool {
	return between(g.Percent, 0, 20) && between(g.Fixed, 0, 20) && between(g.Tax, 0, 30) && between(g.Margin, 0, 10)
}

func (f PaymentFees) validate() error {
	for name, g := range map[string]GatewayFee{"Mercado Pago": f.MercadoPago, "PayPal": f.PayPal, "cripto": f.NOWPayments} {
		if !g.valid() {
			return fmt.Errorf("comisión de %s: porcentaje 0–20, cargo fijo 0–20, impuesto 0–30 y margen 0–10", name)
		}
	}
	b := f.Bizum
	if !between(b.Percent, 0, 20) || !between(b.Fixed, 0, 20) || !between(b.FXMargin, 0, 15) {
		return fmt.Errorf("recargo de Bizum: porcentaje 0–20, cargo fijo 0–20 y margen de cambio 0–15")
	}
	return nil
}

// ceilCents redondea HACIA ARRIBA al céntimo (nunca se cobra de menos). El
// pequeño margen evita que 32.7400000001 suba a 32.75 por error de coma flotante.
func ceilCents(x float64) float64 { return math.Ceil(x*100-1e-6) / 100 }

// gatewayTotal: lo que paga el cliente para que, descontada la comisión de la
// pasarela (porcentaje y cargo fijo, ambos con IGV), quede exactamente net.
func gatewayTotal(net float64, f GatewayFee) (total, fee float64) {
	tax := 1 + f.Tax/100
	pct := f.Percent/100*tax + f.Margin/100
	total = ceilCents((net + f.Fixed*tax) / (1 - pct))
	return total, roundCents(total - net)
}

// bizumTotal: euros que paga el cliente para que, tras la comisión de la
// remesa y su tipo de cambio, lleguen a Perú exactamente basePEN soles.
// eurPerPEN es el cambio de mercado (cuántos € vale 1 sol).
func bizumTotal(basePEN, eurPerPEN float64, f RemittanceFee) (total, fee float64) {
	baseEUR := basePEN * eurPerPEN
	needed := baseEUR / (1 - f.FXMargin/100)
	total = ceilCents((needed + f.Fixed) / (1 - f.Percent/100))
	return total, roundCents(total - roundCents(baseEUR))
}

// ==================== ENDPOINTS ====================

// HandlerGetPaymentFees (público): la web muestra el desglose antes de pagar.
func HandlerGetPaymentFees(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"success": true, "fees": CurrentPaymentFees()})
}

// HandlerAdminGetPaymentFees: las comisiones vigentes y quién las cambió.
func HandlerAdminGetPaymentFees(c *gin.Context) {
	paymentFeesMu.RLock()
	defer paymentFeesMu.RUnlock()
	resp := gin.H{"success": true, "fees": paymentFees, "defaults": defaultPaymentFees, "updated_by": paymentFeesBy}
	if !paymentFeesUpdated.IsZero() {
		resp["updated_at"] = paymentFeesUpdated
	}
	c.JSON(http.StatusOK, resp)
}

// HandlerAdminUpdatePaymentFees guarda las comisiones nuevas.
func HandlerAdminUpdatePaymentFees(database *sql.DB, reviewer func(*gin.Context) string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Se parte de las vigentes: un campo que no venga (p. ej. desde una
		// versión vieja del panel) conserva su valor, nunca queda en 0.
		f := CurrentPaymentFees()
		if err := c.ShouldBindJSON(&f); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "datos inválidos"})
			return
		}
		if err := f.validate(); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}
		by := reviewer(c)
		if err := db.SetSetting(database, paymentFeesKey, f, by); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudieron guardar las comisiones"})
			return
		}
		prev := CurrentPaymentFees()
		paymentFeesMu.Lock()
		paymentFees, paymentFeesBy, paymentFeesUpdated = f, by, time.Now()
		paymentFeesMu.Unlock()
		db.AddAuditLog(database, nil, "PAYMENT_FEES_UPDATED",
			fmt.Sprintf("%s cambió las comisiones: Mercado Pago %+v → %+v; PayPal %+v → %+v; cripto %+v → %+v; Bizum %+v → %+v",
				by, prev.MercadoPago, f.MercadoPago, prev.PayPal, f.PayPal, prev.NOWPayments, f.NOWPayments, prev.Bizum, f.Bizum), c.ClientIP())
		c.JSON(http.StatusOK, gin.H{"success": true, "fees": f})
	}
}

// ==================== VERIFICACIÓN: ¿LLEGÓ EL PRECIO COMPLETO? ====================

// mercadoPagoNetReceived busca el pago aprobado de este txID y devuelve lo que
// cobró y lo que Mercado Pago depositó de verdad (ya sin su comisión e IGV).
// Variable para poder simularla en las pruebas.
var mercadoPagoNetReceived = func(txID string) (charged, net float64, found bool, err error) {
	if paymentCfg.MercadoPagoToken == "" {
		return 0, 0, false, fmt.Errorf("MercadoPago not configured")
	}
	req, err := http.NewRequest("GET",
		"https://api.mercadopago.com/v1/payments/search?external_reference="+url.QueryEscape(txID), nil)
	if err != nil {
		return 0, 0, false, err
	}
	req.Header.Set("Authorization", "Bearer "+paymentCfg.MercadoPagoToken)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, 0, false, err
	}
	defer resp.Body.Close()
	var result struct {
		Results []struct {
			Status             string  `json:"status"`
			TransactionAmount  float64 `json:"transaction_amount"`
			TransactionDetails struct {
				NetReceivedAmount float64 `json:"net_received_amount"`
			} `json:"transaction_details"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, 0, false, err
	}
	for _, r := range result.Results {
		if r.Status == "approved" && r.TransactionDetails.NetReceivedAmount > 0 {
			return r.TransactionAmount, r.TransactionDetails.NetReceivedAmount, true, nil
		}
	}
	return 0, 0, false, nil
}

// payPalNetReceived: lo cobrado y lo que PayPal depositó (tras su comisión).
// Variable para poder simularla en las pruebas.
var payPalNetReceived = func(orderID string) (charged, net float64, found bool, err error) {
	order, err := getPayPalOrder(orderID)
	if err != nil {
		return 0, 0, false, err
	}
	charged, _ = strconv.ParseFloat(order.AmountValue, 64)
	net, perr := strconv.ParseFloat(order.NetAmount, 64)
	if perr != nil || net <= 0 || order.CurrencyCode != "USD" {
		return 0, 0, false, nil
	}
	return charged, net, true, nil
}

// verifyNetReceived guarda lo que la pasarela (Mercado Pago o PayPal) depositó
// por el pago y avisa al equipo si fue MENOS que el precio (la comisión real
// es mayor que la configurada en el panel). Así nunca se pierde dinero sin
// enterarse.
func verifyNetReceived(database *sql.DB, tx types.PaymentTransaction) {
	var charged, net, price float64
	var found bool
	var err error
	gateway, symbol := "Mercado Pago", "S/ "
	switch tx.Gateway {
	case "mercadopago":
		price = tx.AmountPEN
		charged, net, found, err = mercadoPagoNetReceived(tx.ID.String())
	case "paypal":
		gateway, symbol, price = "PayPal", "US$", tx.AmountUSD
		charged, net, found, err = payPalNetReceived(tx.ExternalID)
	default:
		return
	}
	if err != nil || !found {
		if err != nil {
			slog.Warn("Comisiones: no se pudo consultar el neto recibido", "gateway", tx.Gateway, "txID", tx.ID, "error", err)
		}
		return
	}
	if err := db.SetPaymentNetReceived(database, tx.ID, net); err != nil {
		slog.Warn("Comisiones: no se pudo guardar el neto recibido", "txID", tx.ID, "error", err)
	}
	if net+0.005 < price {
		slog.Warn("Comisiones: la pasarela depositó menos que el precio", "gateway", tx.Gateway, "txID", tx.ID, "price", price, "charged", charged, "net", net)
		discordbot.AlertFeeShortfall(gateway, symbol, tx.ProductName, price, charged, net)
		return
	}
	discordbot.ClearFeeShortfall(gateway)
}
