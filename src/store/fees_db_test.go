package store

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"KidStoreStore/src/db"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func setTestPaymentFees(t *testing.T, f PaymentFees) {
	t.Helper()
	paymentFeesMu.Lock()
	prev := paymentFees
	paymentFees = f
	paymentFeesMu.Unlock()
	t.Cleanup(func() {
		paymentFeesMu.Lock()
		paymentFees = prev
		paymentFeesMu.Unlock()
	})
}

// El pago con Mercado Pago cobra precio + comisión (dos líneas en el checkout)
// y guarda el precio y la comisión por separado.
func TestPagoMercadoPago_ElClientePagaLaComision(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, token, cleanup := newPaymentCustomer(t, conn)
	defer cleanup()
	setTestPaymentFees(t, defaultPaymentFees)

	fakeMercadoPago(t, func(r *http.Request, _ string) *http.Response {
		return jsonResponse(r, 201, `{"id":"pref-F","init_point":"https://mp.test/checkout/F"}`)
	})
	// Se captura lo que se le manda a Mercado Pago.
	var items []struct {
		Title     string  `json:"title"`
		UnitPrice float64 `json:"unit_price"`
		Currency  string  `json:"currency_id"`
	}
	inner := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Body != nil && strings.Contains(r.URL.Path, "preferences") {
			b, _ := io.ReadAll(r.Body)
			var p struct {
				Items json.RawMessage `json:"items"`
			}
			json.Unmarshal(b, &p)
			json.Unmarshal(p.Items, &items)
			r.Body = io.NopCloser(strings.NewReader(string(b)))
		}
		return inner.RoundTrip(r)
	})

	w := postCreatePayment(createPaymentRouter(conn, conn), token) // Starter S/10.40
	if w.Code != http.StatusOK {
		t.Fatalf("crear pago: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Price, Fee, Total float64
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	wantTotal, wantFee := gatewayTotal(10.40, defaultPaymentFees.MercadoPago)
	if resp.Price != 10.40 || resp.Fee != wantFee || resp.Total != wantTotal {
		t.Errorf("respuesta = %+v, se esperaba precio 10.40, comisión %.2f, total %.2f", resp, wantFee, wantTotal)
	}
	if len(items) != 2 || items[0].UnitPrice != 10.40 || items[1].UnitPrice != wantFee || items[1].Title != "Comisión de Mercado Pago" || items[1].Currency != "PEN" {
		t.Errorf("líneas enviadas a Mercado Pago = %+v", items)
	}

	id, _ := onlyPayment(t, conn, custID)
	tx, err := db.GetPaymentTransaction(conn, id)
	if err != nil || tx.AmountPEN != 10.40 || tx.FeeAmount != wantFee {
		t.Fatalf("pago guardado = %+v %v", tx, err)
	}
	if amount, cur := ChargedAmountAndCurrency(tx.Gateway, tx.AmountPEN, tx.AmountUSD, tx.AmountLocal, tx.CurrencyCode, tx.FeeAmount); amount != wantTotal || cur != "PEN" {
		t.Errorf("lo cobrado debe ser precio + comisión: %.2f %s", amount, cur)
	}

	// Lo que Mercado Pago deposita queda registrado.
	prevNet := mercadoPagoNetReceived
	mercadoPagoNetReceived = func(string) (float64, float64, bool, error) { return wantTotal, 10.41, true, nil }
	t.Cleanup(func() { mercadoPagoNetReceived = prevNet })
	verifyNetReceived(conn, tx)
	if got, _ := db.GetPaymentTransaction(conn, id); got.NetReceived == nil || *got.NetReceived != 10.41 {
		t.Errorf("neto recibido = %v", got.NetReceived)
	}
}

// dLocal Go ya no crea pagos (Mercado Pago cubre las tarjetas internacionales).
func TestPago_DLocalGoYaNoCreaPagos(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, token, cleanup := newPaymentCustomer(t, conn)
	defer cleanup()
	r := createPaymentRouter(conn, conn)
	for _, gw := range []string{"dlocalgo", "binance"} {
		req := httptest.NewRequest("POST", "/store/payment", strings.NewReader(`{"gateway":"`+gw+`","payment_type":"kc_recharge","product_id":"starter"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", gw, w.Code, w.Body.String())
		}
	}
	if n := len(customerPayments(t, conn, custID)); n != 0 {
		t.Errorf("no debía crearse ningún pago, hay %d", n)
	}
}

// El admin cambia las comisiones; se guardan, se aplican y sobreviven a un reinicio.
func TestComisiones_PanelAdmin(t *testing.T) {
	conn := setupShopTestDB(t)
	setTestPaymentFees(t, defaultPaymentFees)
	t.Cleanup(func() { conn.Exec(`DELETE FROM app_settings WHERE key = $1`, paymentFeesKey) })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/store/payment-fees", HandlerGetPaymentFees(conn))
	r.PUT("/admin/payment-fees", HandlerAdminUpdatePaymentFees(conn, func(*gin.Context) string { return "Panel: dueño" }))
	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/admin/payment-fees", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	if w := put(`{"mercadopago":{"percent":45,"fixed":1,"tax":18},"bizum":{"percent":1.5}}`); w.Code != http.StatusBadRequest {
		t.Errorf("un porcentaje imposible debe rechazarse: %d", w.Code)
	}
	if w := put(`{"mercadopago":{"percent":4.99,"fixed":1,"tax":18,"margin":0.2},"bizum":{"percent":2,"fixed":1.99,"fx_margin":1.5}}`); w.Code != http.StatusOK {
		t.Fatalf("guardar: %d %s", w.Code, w.Body.String())
	}
	// PayPal y cripto no venían en el cuerpo: conservan su valor (nunca quedan en 0).
	want := PaymentFees{MercadoPago: GatewayFee{Percent: 4.99, Fixed: 1, Tax: 18, Margin: 0.2,
		VolumeThreshold: defaultPaymentFees.MercadoPago.VolumeThreshold, VolumePercent: defaultPaymentFees.MercadoPago.VolumePercent},
		PayPal: defaultPaymentFees.PayPal,
		Bizum: RemittanceFee{Percent: 2, Fixed: 1.99, FXMargin: 1.5}}
	if CurrentPaymentFees() != want {
		t.Errorf("vigentes = %+v", CurrentPaymentFees())
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/store/payment-fees", nil))
	if !strings.Contains(w.Body.String(), `"percent":4.99`) || !strings.Contains(w.Body.String(), `"fx_margin":1.5`) {
		t.Errorf("la web debe recibir las comisiones nuevas: %s", w.Body.String())
	}

	// "Reinicio": se vuelven a leer de la base.
	setTestPaymentFees(t, defaultPaymentFees)
	LoadPaymentFees(conn)
	if CurrentPaymentFees() != want {
		t.Errorf("tras reiniciar = %+v", CurrentPaymentFees())
	}

	// Bizum usa la remesa configurada.
	q, err := quoteManual("gamer", 0, "bizum", 0.25)
	wantEUR, _ := bizumTotal(31.20, 0.25, want.Bizum)
	if err != nil || q.Amount != wantEUR {
		t.Errorf("bizum = %+v %v, se esperaba €%.2f", q, err, wantEUR)
	}
}

// postPayment crea un pago con la pasarela indicada.
func postPayment(r *gin.Engine, token, gateway string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/store/payment", strings.NewReader(`{"gateway":"`+gateway+`","payment_type":"kc_recharge","product_id":"gamer"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// PayPal cobra en dólares precio + comisión; NOWPayments cobra el precio y le
// pide a NOWPayments que la comisión (suya y de la red) la pague el cliente.
func TestPagoPayPalYCripto_ElClientePagaLaComision(t *testing.T) {
	conn := setupShopTestDB(t)
	_, token, cleanup := newPaymentCustomer(t, conn)
	defer cleanup()
	setTestPaymentFees(t, defaultPaymentFees)
	withPaymentCfg(t, func(c *PaymentConfig) {
		c.PayPalClientID, c.PayPalClientSecret, c.PayPalMode = "id", "secret", "sandbox"
		c.NOWPaymentsAPIKey = "np-key"
		c.FrontendURL, c.BackendURL = "https://www.example.com", "https://api.example.com"
	})

	sent := map[string]map[string]any{}
	prev := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &body)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/oauth2/token"):
			return jsonResponse(r, 200, `{"access_token":"tok"}`), nil
		case strings.HasSuffix(r.URL.Path, "/v2/checkout/orders"):
			sent["paypal"] = body
			return jsonResponse(r, 201, `{"id":"ORDER-1","links":[{"rel":"approve","href":"https://paypal.test/approve"}]}`), nil
		case strings.HasSuffix(r.URL.Path, "/v1/invoice"):
			sent["nowpayments"] = body
			return jsonResponse(r, 200, `{"id":"inv-1","invoice_url":"https://nowpayments.test/inv-1"}`), nil
		}
		return jsonResponse(r, 404, `{}`), nil
	})
	t.Cleanup(func() { http.DefaultTransport = prev })

	r := createPaymentRouter(conn, conn)
	type created struct {
		Price, Fee, Total float64
		Currency          string
	}

	w := postPayment(r, token, "paypal")
	if w.Code != http.StatusOK {
		t.Fatalf("paypal: %d %s", w.Code, w.Body.String())
	}
	var pp created
	json.Unmarshal(w.Body.Bytes(), &pp)
	wantTotal, wantFee := gatewayTotal(pp.Price, defaultPaymentFees.PayPal)
	if pp.Currency != "USD" || pp.Fee != wantFee || pp.Total != wantTotal || pp.Fee <= 0 {
		t.Errorf("paypal = %+v, se esperaba comisión %.2f y total %.2f", pp, wantFee, wantTotal)
	}
	units, _ := sent["paypal"]["purchase_units"].([]any)
	if len(units) != 1 {
		t.Fatalf("orden de PayPal = %+v", sent["paypal"])
	}
	amount := units[0].(map[string]any)["amount"].(map[string]any)
	if amount["value"] != fmt.Sprintf("%.2f", wantTotal) || amount["currency_code"] != "USD" {
		t.Errorf("PayPal debe cobrar precio + comisión: %+v", amount)
	}

	w = postPayment(r, token, "nowpayments")
	if w.Code != http.StatusOK {
		t.Fatalf("nowpayments: %d %s", w.Code, w.Body.String())
	}
	var np created
	json.Unmarshal(w.Body.Bytes(), &np)
	if np.Fee != 0 || np.Total != np.Price {
		t.Errorf("cripto: la comisión la cobra NOWPayments al cliente, aquí no se suma nada: %+v", np)
	}
	if sent["nowpayments"]["is_fee_paid_by_user"] != true || sent["nowpayments"]["price_amount"] != np.Price {
		t.Errorf("factura de NOWPayments = %+v", sent["nowpayments"])
	}
}

// PayPal: se guarda lo que depositó y se detecta si fue menos que el precio.
func TestPayPal_NetoRecibido(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, _, cleanup := newPaymentCustomer(t, conn)
	defer cleanup()
	id := uuid.New()
	if err := db.CreatePaymentTransaction(conn, db.PaymentTransactionInput{
		ID: id, CustomerID: custID, Gateway: "paypal", PaymentType: "kc_recharge", ProductID: "gamer",
		ProductName: "Gamer 2,400 KC", AmountPEN: 31.20, AmountUSD: 8.42, FeeAmount: 0.95, KCAmount: 2400, ExternalID: "ORDER-9",
	}); err != nil {
		t.Fatal(err)
	}
	tx, _ := db.GetPaymentTransaction(conn, id)
	if amount, cur := ChargedAmountAndCurrency(tx.Gateway, tx.AmountPEN, tx.AmountUSD, tx.AmountLocal, tx.CurrencyCode, tx.FeeAmount); amount != 9.37 || cur != "USD" {
		t.Errorf("cobrado = %.2f %s, se esperaba 9.37 USD", amount, cur)
	}
	prevNet := payPalNetReceived
	var asked string
	payPalNetReceived = func(orderID string) (float64, float64, bool, error) { asked = orderID; return 9.37, 8.42, true, nil }
	t.Cleanup(func() { payPalNetReceived = prevNet })
	verifyNetReceived(conn, tx)
	got, _ := db.GetPaymentTransaction(conn, id)
	if asked != "ORDER-9" || got.NetReceived == nil || *got.NetReceived != 8.42 {
		t.Errorf("neto de PayPal = %v (orden consultada %q)", got.NetReceived, asked)
	}
}

// Si en el mes se cobró más de S/25,000 con Mercado Pago, se usa su tarifa
// más alta (3.99% + S/1): la web y el cobro real pasan solos al tramo nuevo.
func TestMercadoPago_TramoPorVolumenDelMes(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, _, cleanup := newPaymentCustomer(t, conn)
	defer cleanup()
	setTestPaymentFees(t, defaultPaymentFees)
	resetVolume := func() { monthVolumeMu.Lock(); monthVolumeKey = ""; monthVolumeMu.Unlock() }
	resetVolume()
	t.Cleanup(resetVolume)

	add := func(amount float64, creditedAt string) {
		t.Helper()
		if _, err := conn.Exec(`INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name,
			amount_pen, amount_usd, fee_amount, kc_amount, external_id, status, kc_credited_at, created_at, updated_at)
			VALUES ($1,$2,'mercadopago','kc_recharge','legend','Legend',$3,0,0,12500,'pref','approved',`+creditedAt+`,NOW(),NOW())`,
			uuid.New(), custID, amount); err != nil {
			t.Fatal(err)
		}
	}
	percent := func() float64 {
		t.Helper()
		resetVolume()
		return effectivePaymentFees(conn).MercadoPago.Percent
	}

	add(24_000, "NOW()")
	add(9_000, "NOW() - INTERVAL '40 days'") // del mes pasado: no cuenta
	if p := percent(); p != 3.49 {
		t.Errorf("con S/24,000 en el mes la comisión debe ser 3.49%%, es %.2f%%", p)
	}
	add(1_500, "NOW()")
	if p := percent(); p != 3.99 {
		t.Errorf("con S/25,500 en el mes la comisión debe ser 3.99%%, es %.2f%%", p)
	}
	// Lo cobrado en un pago nuevo usa el tramo alto.
	total, _ := gatewayTotal(31.20, effectivePaymentFees(conn).MercadoPago)
	if want, _ := gatewayTotal(31.20, GatewayFee{Percent: 3.99, Fixed: 1, Tax: 18}); total != want {
		t.Errorf("total = %.2f, se esperaba %.2f", total, want)
	}
}
