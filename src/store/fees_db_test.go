package store

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"KidStoreStore/src/db"

	"github.com/gin-gonic/gin"
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

// Las pasarelas retiradas ya no crean pagos.
func TestPago_SoloMercadoPago(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, token, cleanup := newPaymentCustomer(t, conn)
	defer cleanup()
	r := createPaymentRouter(conn, conn)
	for _, gw := range []string{"paypal", "nowpayments", "dlocalgo", "binance"} {
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
	r.GET("/store/payment-fees", HandlerGetPaymentFees)
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
	want := PaymentFees{MercadoPago: GatewayFee{Percent: 4.99, Fixed: 1, Tax: 18, Margin: 0.2}, Bizum: RemittanceFee{Percent: 2, Fixed: 1.99, FXMargin: 1.5}}
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
