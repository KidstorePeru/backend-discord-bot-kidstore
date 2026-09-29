package store

// Pruebas del orden de creación de pagos: registro local DURABLE primero,
// sesión externa después, ID externo al final — y los estados/recuperación
// cuando cualquiera de los dos últimos pasos falla. El proveedor
// (Mercado Pago) es un http.RoundTripper simulado: nada llega a una pasarela
// real.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"KidStoreStore/src/db"
	"KidStoreStore/src/middleware"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Status: fmt.Sprintf("%d", status), Request: r,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
}

func fakeMercadoPago(t *testing.T, handler func(r *http.Request, txID string) *http.Response) {
	t.Helper()
	prev := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var payload struct {
			ExternalReference string `json:"external_reference"`
		}
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &payload)
		}
		return handler(r, payload.ExternalReference), nil
	})
	t.Cleanup(func() { http.DefaultTransport = prev })
	withPaymentCfg(t, func(c *PaymentConfig) {
		c.MercadoPagoToken = "test-token"
		c.FrontendURL = "http://localhost:5173"
	})
	prevBackoff := persistExternalIDBackoff
	persistExternalIDBackoff = []time.Duration{0, 0}
	t.Cleanup(func() { persistExternalIDBackoff = prevBackoff })
}

func createPaymentRouter(authDB, handlerDB *sql.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/store")
	g.Use(middleware.CustomerAuthMiddleware(authDB, sessionTestSecret))
	g.POST("/payment", HandlerCreatePayment(handlerDB))
	return r
}

func postCreatePayment(r *gin.Engine, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/store/payment", strings.NewReader(`{"gateway":"mercadopago","payment_type":"kc_recharge","product_id":"starter"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

type paymentRow struct {
	Status     string
	ExternalID string
	Progress   string
}

func customerPayments(t *testing.T, conn *sql.DB, custID uuid.UUID) map[uuid.UUID]paymentRow {
	t.Helper()
	rows, err := conn.Query(`SELECT id, status, COALESCE(external_id,''), COALESCE(progress,'') FROM payment_transactions WHERE customer_id=$1`, custID)
	if err != nil {
		t.Fatalf("leer pagos: %v", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]paymentRow{}
	for rows.Next() {
		var id uuid.UUID
		var p paymentRow
		if err := rows.Scan(&id, &p.Status, &p.ExternalID, &p.Progress); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[id] = p
	}
	return out
}

func onlyPayment(t *testing.T, conn *sql.DB, custID uuid.UUID) (uuid.UUID, paymentRow) {
	t.Helper()
	all := customerPayments(t, conn, custID)
	if len(all) != 1 {
		t.Fatalf("esperaba exactamente 1 pago local, encontré %d", len(all))
	}
	for id, p := range all {
		return id, p
	}
	return uuid.Nil, paymentRow{}
}

func newPaymentCustomer(t *testing.T, conn *sql.DB) (uuid.UUID, string, func()) {
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	return custID, tokenFor(t, custID), func() {
		conn.Exec(`DELETE FROM payment_transactions WHERE customer_id=$1`, custID)
		cleanup()
	}
}

func TestCreatePayment_GuardaElRegistroLocalANTESDeCrearLaSesionExterna(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, token, cleanup := newPaymentCustomer(t, conn)
	defer cleanup()

	var seenAtProviderCall *paymentRow
	fakeMercadoPago(t, func(r *http.Request, txID string) *http.Response {
		id, _ := uuid.Parse(txID)
		var p paymentRow
		if err := conn.QueryRow(`SELECT status, COALESCE(external_id,''), COALESCE(progress,'') FROM payment_transactions WHERE id=$1`, id).Scan(&p.Status, &p.ExternalID, &p.Progress); err == nil {
			seenAtProviderCall = &p
		}
		return jsonResponse(r, 201, `{"id":"pref-A","init_point":"https://mp.test/checkout/A"}`)
	})

	w := postCreatePayment(createPaymentRouter(conn, conn), token)
	if w.Code != http.StatusOK {
		t.Fatalf("esperaba 200, obtuve %d: %s", w.Code, w.Body.String())
	}
	if seenAtProviderCall == nil || seenAtProviderCall.Status != "pending" || seenAtProviderCall.ExternalID != "" {
		t.Fatalf("cuando se llamó a la pasarela ya debía existir el pago local pending sin external_id, vi %+v", seenAtProviderCall)
	}
	var resp struct {
		CheckoutURL string `json:"checkout_url"`
		ExternalID  string `json:"external_id"`
		PaymentID   string `json:"payment_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	id, row := onlyPayment(t, conn, custID)
	if resp.CheckoutURL != "https://mp.test/checkout/A" || resp.ExternalID != "pref-A" || resp.PaymentID != id.String() {
		t.Errorf("respuesta inesperada: %+v", resp)
	}
	if row.Status != "pending" || row.ExternalID != "pref-A" {
		t.Errorf("tras el éxito el pago debe quedar pending con su ID externo persistido: %+v", row)
	}
}

func TestCreatePayment_FalloDeLaPasarela_CierraElPagoLocalYNoDevuelveCheckout(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, token, cleanup := newPaymentCustomer(t, conn)
	defer cleanup()

	for name, resp := range map[string]func(r *http.Request) *http.Response{
		"la pasarela responde 500":       func(r *http.Request) *http.Response { return jsonResponse(r, 500, `{"error":"boom"}`) },
		"respuesta 201 sin ID ni checkout": func(r *http.Request) *http.Response { return jsonResponse(r, 201, `{}`) },
	} {
		resp := resp
		fakeMercadoPago(t, func(r *http.Request, _ string) *http.Response { return resp(r) })
		conn.Exec(`DELETE FROM payment_transactions WHERE customer_id=$1`, custID)

		w := postCreatePayment(createPaymentRouter(conn, conn), token)
		if w.Code != http.StatusInternalServerError {
			t.Errorf("%s: esperaba 500, obtuve %d", name, w.Code)
		}
		if strings.Contains(w.Body.String(), "checkout_url") {
			t.Errorf("%s: NUNCA debe devolverse un checkout: %s", name, w.Body.String())
		}
		_, row := onlyPayment(t, conn, custID)
		if row.Status != "failed" || row.ExternalID != "" || !strings.HasPrefix(row.Progress, "creation_failed:") {
			t.Errorf("%s: el pago local debe cerrarse como failed/creation_failed: %+v", name, row)
		}
	}
}

func TestCreatePayment_SiNoSeGuardaElRegistroLocal_NoSeCreaNingunaSesionExterna(t *testing.T) {
	conn := setupShopTestDB(t)
	_, token, cleanup := newPaymentCustomer(t, conn)
	defer cleanup()

	providerCalls := 0
	fakeMercadoPago(t, func(r *http.Request, _ string) *http.Response {
		providerCalls++
		return jsonResponse(r, 201, `{"id":"pref-X","init_point":"https://mp.test/X"}`)
	})
	broken, err := sql.Open("postgres", "host=127.0.0.1 port=1 sslmode=disable connect_timeout=1")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer broken.Close()

	// El middleware usa la base real (para poder autenticar); el handler, la caída.
	w := postCreatePayment(createPaymentRouter(conn, broken), token)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("esperaba 500, obtuve %d", w.Code)
	}
	if providerCalls != 0 {
		t.Fatalf("sin registro local durable NO debe crearse ninguna sesión en la pasarela (llamadas=%d)", providerCalls)
	}
	if strings.Contains(w.Body.String(), "checkout_url") {
		t.Errorf("no debe devolverse un checkout: %s", w.Body.String())
	}
}

// TestCreatePayment_FalloAlGuardarElIDExterno_NoEntregaCheckoutYQuedaConciliable
// simula que la escritura del external_id falla (un trigger de prueba, acotado
// a este cliente, la rechaza mientras el pago sigue 'pending'): el cliente no
// recibe el checkout, y el pago queda en 'review' CON su ID externo para que
// la conciliación automática lo siga consultando.
func TestCreatePayment_FalloAlGuardarElIDExterno_NoEntregaCheckoutYQuedaConciliable(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, token, cleanup := newPaymentCustomer(t, conn)
	defer cleanup()

	fakeMercadoPago(t, func(r *http.Request, _ string) *http.Response {
		return jsonResponse(r, 201, `{"id":"pref-D","init_point":"https://mp.test/checkout/D"}`)
	})

	mustExec := func(q string) {
		if _, err := conn.Exec(q); err != nil {
			t.Fatalf("preparar trigger de prueba: %v\n%s", err, q)
		}
	}
	mustExec(fmt.Sprintf(`CREATE OR REPLACE FUNCTION test_block_ext_id() RETURNS trigger AS $$
		BEGIN
			IF NEW.status='pending' AND COALESCE(NEW.external_id,'')<>'' AND NEW.customer_id='%s' THEN
				RAISE EXCEPTION 'fallo simulado al guardar external_id';
			END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql`, custID))
	mustExec(`DROP TRIGGER IF EXISTS test_block_ext_id_trg ON payment_transactions`)
	mustExec(`CREATE TRIGGER test_block_ext_id_trg BEFORE UPDATE ON payment_transactions FOR EACH ROW EXECUTE PROCEDURE test_block_ext_id()`)
	defer func() {
		conn.Exec(`DROP TRIGGER IF EXISTS test_block_ext_id_trg ON payment_transactions`)
		conn.Exec(`DROP FUNCTION IF EXISTS test_block_ext_id()`)
	}()

	w := postCreatePayment(createPaymentRouter(conn, conn), token)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("esperaba 500, obtuve %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "checkout_url") || strings.Contains(w.Body.String(), "mp.test") {
		t.Fatalf("NUNCA debe entregarse un checkout no rastreable: %s", w.Body.String())
	}

	id, row := onlyPayment(t, conn, custID)
	if row.Status != "review" || row.ExternalID != "pref-D" || row.Progress != "external_id_unsaved" {
		t.Fatalf("el pago debe quedar en 'review' con su ID externo: %+v", row)
	}

	// Sigue en el circuito de conciliación (GetStalePendingPayments incluye
	// 'review' con external_id) y NO lo cierra el barrido de abandonados.
	conn.Exec(`UPDATE payment_transactions SET created_at=NOW()-INTERVAL '1 hour' WHERE id=$1`, id)
	if _, err := db.ExpireAbandonedPaymentCreations(conn, 15*time.Minute); err != nil {
		t.Fatalf("ExpireAbandonedPaymentCreations: %v", err)
	}
	stale, err := db.GetStalePendingPayments(conn)
	if err != nil {
		t.Fatalf("GetStalePendingPayments: %v", err)
	}
	found := false
	for _, p := range stale {
		if p.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("el pago en 'review' con ID externo debe seguir siendo conciliable")
	}
}

func TestExpireAbandonedPaymentCreations_SoloCierraLosPendingSinIDExternoYViejos(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, _, cleanup := newPaymentCustomer(t, conn)
	defer cleanup()

	insert := func(status, externalID string, age time.Duration) uuid.UUID {
		id := uuid.New()
		if _, err := conn.Exec(`INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, created_at, updated_at)
			VALUES ($1,$2,'mercadopago','kc_recharge','starter','Starter',10.40,2.80,800,$3,$4, NOW() - $5::interval, NOW() - $5::interval)`,
			id, custID, externalID, status, fmt.Sprintf("%d seconds", int64(age.Seconds()))); err != nil {
			t.Fatalf("insert: %v", err)
		}
		return id
	}
	abandoned := insert("pending", "", 30*time.Minute)   // se cierra
	inFlight := insert("pending", "", 1*time.Minute)     // creación en curso: se respeta
	tracked := insert("pending", "pref-1", 30*time.Minute) // normal (webhooks + conciliación)
	review := insert("review", "", 30*time.Minute)       // no es pending: no se toca

	n, err := db.ExpireAbandonedPaymentCreations(conn, 15*time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("debía cerrar exactamente 1 (cerró %d, err=%v)", n, err)
	}
	all := customerPayments(t, conn, custID)
	if all[abandoned].Status != "failed" || !strings.HasPrefix(all[abandoned].Progress, "creation_failed") {
		t.Errorf("el abandonado debe quedar failed/creation_failed: %+v", all[abandoned])
	}
	for name, id := range map[string]uuid.UUID{"en curso": inFlight, "con ID externo": tracked} {
		if all[id].Status != "pending" {
			t.Errorf("%s no debía tocarse: %+v", name, all[id])
		}
	}
	if all[review].Status != "review" {
		t.Errorf("un pago en review no debía tocarse: %+v", all[review])
	}
}
