package store

// Pruebas de regresión del endurecimiento de webhooks:
//   - cuerpos acotados (413) y errores de lectura comprobados;
//   - el evento se valida y se NORMALIZA antes de persistirlo (solo los
//     campos necesarios; nada de datos personales del pagador);
//   - retención: solo se purgan eventos ya procesados, nunca los que aún
//     necesitan reintento.
// Los handlers se ejercitan con el transporte HTTP por defecto reemplazado
// por uno simulado: NINGUNA prueba llega a una pasarela real.

import (
	"crypto/hmac"
	"crypto/sha512"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"KidStoreStore/src/db"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ── Pruebas puras: normalización ──

func TestNormalizeMercadoPagoEvent(t *testing.T) {
	t.Run("payment válido: conserva solo tipo e ID, descarta el resto", func(t *testing.T) {
		body := `{"type":"payment","action":"payment.created","data":{"id":"123456"},"user_id":"9","payer":{"email":"cliente@example.com"}}`
		id, canonical, v := normalizeMercadoPagoEvent([]byte(body))
		if v != webhookAccept || id != "123456" {
			t.Fatalf("esperaba accept/123456, obtuve %v/%q", v, id)
		}
		if strings.Contains(string(canonical), "cliente@example.com") || strings.Contains(string(canonical), "payer") {
			t.Errorf("el evento normalizado no debe conservar datos del pagador: %s", canonical)
		}
		if string(canonical) != `{"type":"payment","data":{"id":"123456"}}` {
			t.Errorf("forma normalizada inesperada: %s", canonical)
		}
	})
	t.Run("JSON inválido", func(t *testing.T) {
		if _, _, v := normalizeMercadoPagoEvent([]byte(`{no es json`)); v != webhookInvalid {
			t.Errorf("esperaba invalid, obtuve %v", v)
		}
	})
	t.Run("tipo distinto de payment: se ignora sin persistir", func(t *testing.T) {
		if _, _, v := normalizeMercadoPagoEvent([]byte(`{"type":"merchant_order","data":{"id":"1"}}`)); v != webhookIgnore {
			t.Errorf("esperaba ignore, obtuve %v", v)
		}
	})
	t.Run("ID vacío, con espacios o larguísimo: inválido", func(t *testing.T) {
		for _, id := range []string{"", "12 34", "a/b", strings.Repeat("9", 65)} {
			body := fmt.Sprintf(`{"type":"payment","data":{"id":%q}}`, id)
			if _, _, v := normalizeMercadoPagoEvent([]byte(body)); v != webhookInvalid {
				t.Errorf("id %q: esperaba invalid, obtuve %v", id, v)
			}
		}
	})
}

func TestNormalizePayPalEvent(t *testing.T) {
	t.Run("ORDER.APPROVED: conserva tipo e ID, descarta datos del pagador", func(t *testing.T) {
		body := `{"event_type":"CHECKOUT.ORDER.APPROVED","resource":{"id":"5O190127TN364715T","payer":{"email_address":"cliente@example.com","name":{"given_name":"Ana"}},"purchase_units":[{"shipping":{"address":{"address_line_1":"Calle 1"}}}]}}`
		ev, canonical, v := normalizePayPalEvent([]byte(body))
		if v != webhookAccept || ev.Resource.ID != "5O190127TN364715T" {
			t.Fatalf("esperaba accept, obtuve %v %+v", v, ev)
		}
		for _, leak := range []string{"cliente@example.com", "Ana", "Calle 1", "payer", "purchase_units"} {
			if strings.Contains(string(canonical), leak) {
				t.Errorf("el evento normalizado filtra %q: %s", leak, canonical)
			}
		}
	})
	t.Run("CAPTURE.COMPLETED sin order_id: inválido", func(t *testing.T) {
		body := `{"event_type":"PAYMENT.CAPTURE.COMPLETED","resource":{"id":"CAP123"}}`
		if _, _, v := normalizePayPalEvent([]byte(body)); v != webhookInvalid {
			t.Errorf("esperaba invalid, obtuve %v", v)
		}
	})
	t.Run("CAPTURE.COMPLETED con order_id: accept y conserva el order_id", func(t *testing.T) {
		body := `{"event_type":"PAYMENT.CAPTURE.COMPLETED","resource":{"id":"CAP123","supplementary_data":{"related_ids":{"order_id":"ORD456"}}}}`
		ev, _, v := normalizePayPalEvent([]byte(body))
		if v != webhookAccept || ev.Resource.SupplementaryData.RelatedIDs.OrderID != "ORD456" {
			t.Errorf("esperaba accept con order_id, obtuve %v %+v", v, ev)
		}
	})
	t.Run("otros tipos se ignoran; JSON roto es inválido", func(t *testing.T) {
		if _, _, v := normalizePayPalEvent([]byte(`{"event_type":"BILLING.SUBSCRIPTION.CREATED","resource":{"id":"X"}}`)); v != webhookIgnore {
			t.Errorf("esperaba ignore, obtuve %v", v)
		}
		if _, _, v := normalizePayPalEvent([]byte(`[[[`)); v != webhookInvalid {
			t.Errorf("esperaba invalid, obtuve %v", v)
		}
	})
}

// ── Pruebas de handlers (base de datos de pruebas + pasarelas simuladas) ──

type fakeGatewayTransport struct{ calls int }

func (f *fakeGatewayTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.calls++
	return &http.Response{
		StatusCode: 200, Status: "200 OK", Request: r,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(`{"status":"pending","payment_status":"waiting","order_id":"no-es-uuid"}`)),
	}, nil
}

// stubGateways evita cualquier llamada de red real desde los goroutines de
// procesamiento de los handlers (http.Client sin Transport usa DefaultTransport).
func stubGateways(t *testing.T) *fakeGatewayTransport {
	t.Helper()
	fake := &fakeGatewayTransport{}
	prev := http.DefaultTransport
	http.DefaultTransport = fake
	t.Cleanup(func() { http.DefaultTransport = prev })
	return fake
}

func webhookRouter(handlers map[string]gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	for path, h := range handlers {
		r.POST(path, h)
	}
	return r
}

func postRaw(r *gin.Engine, path string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func countEvents(t *testing.T, conn *sql.DB, gateway string, since time.Time) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM webhook_events WHERE gateway=$1 AND received_at >= $2`, gateway, since).Scan(&n); err != nil {
		t.Fatalf("no se pudo contar webhook_events: %v", err)
	}
	return n
}

func waitEventsProcessed(t *testing.T, conn *sql.DB, gateway string, since time.Time) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var pending int
		conn.QueryRow(`SELECT COUNT(*) FROM webhook_events WHERE gateway=$1 AND received_at >= $2 AND processed_at IS NULL`, gateway, since).Scan(&pending)
		if pending == 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("los eventos de %s no terminaron de procesarse a tiempo", gateway)
}

func cleanupEvents(conn *sql.DB, gateway string, since time.Time) {
	conn.Exec(`DELETE FROM webhook_events WHERE gateway=$1 AND received_at >= $2`, gateway, since)
}

// errReader simula una lectura que se corta a mitad de camino.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("conexión cortada") }

func TestHandlersMercadoPagoYPayPal_RechazanCuerpoGrandeInvalidoYErrorDeLectura(t *testing.T) {
	conn := setupShopTestDB(t)
	withPaymentCfg(t, func(c *PaymentConfig) { c.AllowUnsignedWebhooks, c.AppEnv = true, AppEnvDevelopment }) // estas pruebas no ejercitan la firma
	fake := stubGateways(t)
	since := time.Now().Add(-time.Second)
	defer cleanupEvents(conn, "mercadopago", since)
	defer cleanupEvents(conn, "paypal", since)

	router := webhookRouter(map[string]gin.HandlerFunc{
		"/mp": HandlerMercadoPagoWebhook(conn),
		"/pp": HandlerPayPalWebhook(conn),
	})

	huge := `{"type":"payment","data":{"id":"1"},"padding":"` + strings.Repeat("x", maxWebhookBodyBytes+1) + `"}`
	for path, gw := range map[string]string{"/mp": "mercadopago", "/pp": "paypal"} {
		if w := postRaw(router, path, strings.NewReader(huge), nil); w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: cuerpo sobre el tope debe dar 413, obtuve %d", gw, w.Code)
		}
		if w := postRaw(router, path, strings.NewReader(`{no es json`), nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s: JSON inválido debe dar 400, obtuve %d", gw, w.Code)
		}
		if w := postRaw(router, path, errReader{}, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s: un error de lectura debe dar 400 (antes se ignoraba), obtuve %d", gw, w.Code)
		}
		if n := countEvents(t, conn, gw, since); n != 0 {
			t.Errorf("%s: ningún cuerpo inválido/grande/truncado debe persistirse, encontré %d filas", gw, n)
		}
	}
	// Eventos válidos pero irrelevantes: 200 sin persistir.
	if w := postRaw(router, "/mp", strings.NewReader(`{"type":"merchant_order","data":{"id":"1"}}`), nil); w.Code != http.StatusOK {
		t.Errorf("mp: evento irrelevante debe dar 200, obtuve %d", w.Code)
	}
	if w := postRaw(router, "/pp", strings.NewReader(`{"event_type":"BILLING.PLAN.CREATED","resource":{"id":"X"}}`), nil); w.Code != http.StatusOK {
		t.Errorf("pp: evento irrelevante debe dar 200, obtuve %d", w.Code)
	}
	if countEvents(t, conn, "mercadopago", since)+countEvents(t, conn, "paypal", since) != 0 {
		t.Error("los eventos irrelevantes no deben persistirse")
	}
	if fake.calls != 0 {
		t.Errorf("ninguna de estas peticiones debía tocar una pasarela, hubo %d llamadas", fake.calls)
	}
}

func TestHandlersMercadoPagoYPayPal_PersistenSoloElEventoNormalizado(t *testing.T) {
	conn := setupShopTestDB(t)
	withPaymentCfg(t, func(c *PaymentConfig) { c.AllowUnsignedWebhooks, c.AppEnv = true, AppEnvDevelopment }) // estas pruebas no ejercitan la firma
	stubGateways(t)
	since := time.Now().Add(-time.Second)
	defer cleanupEvents(conn, "mercadopago", since)
	defer cleanupEvents(conn, "paypal", since)

	router := webhookRouter(map[string]gin.HandlerFunc{
		"/mp": HandlerMercadoPagoWebhook(conn),
		"/pp": HandlerPayPalWebhook(conn),
	})

	mpBody := `{"type":"payment","data":{"id":"777001"},"payer":{"email":"secreto-mp@example.com"}}`
	if w := postRaw(router, "/mp", strings.NewReader(mpBody), nil); w.Code != http.StatusOK {
		t.Fatalf("mp válido debe dar 200, obtuve %d: %s", w.Code, w.Body.String())
	}
	ppBody := `{"event_type":"CHECKOUT.ORDER.APPROVED","resource":{"id":"ORDPP1","payer":{"email_address":"secreto-pp@example.com"}}}`
	if w := postRaw(router, "/pp", strings.NewReader(ppBody), nil); w.Code != http.StatusOK {
		t.Fatalf("pp válido debe dar 200, obtuve %d: %s", w.Code, w.Body.String())
	}
	waitEventsProcessed(t, conn, "mercadopago", since)
	waitEventsProcessed(t, conn, "paypal", since)

	for gw, secret := range map[string]string{"mercadopago": "secreto-mp@example.com", "paypal": "secreto-pp@example.com"} {
		var raw string
		if err := conn.QueryRow(`SELECT raw_body FROM webhook_events WHERE gateway=$1 AND received_at >= $2`, gw, since).Scan(&raw); err != nil {
			t.Fatalf("%s: el evento válido debe quedar registrado: %v", gw, err)
		}
		if strings.Contains(raw, secret) || strings.Contains(raw, "payer") {
			t.Errorf("%s: raw_body conserva datos personales: %s", gw, raw)
		}
	}
}

func signNOWPayments(secret, sortedBody string) string {
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write([]byte(sortedBody))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestHandlersNOWPaymentsYDLocalGo_LimiteYNormalizacionSinDebilitarFirma(t *testing.T) {
	conn := setupShopTestDB(t)
	stubGateways(t)
	since := time.Now().Add(-time.Second)
	defer cleanupEvents(conn, "nowpayments", since)
	defer cleanupEvents(conn, "dlocalgo", since)

	prevCfg := paymentCfg
	paymentCfg.NOWPaymentsIPNSecret = "test-ipn-secret"
	defer func() { paymentCfg = prevCfg }()

	router := webhookRouter(map[string]gin.HandlerFunc{
		"/now": HandlerNOWPaymentsWebhook(conn),
		"/dl":  HandlerDLocalGoWebhook(conn),
	})

	huge := `{"payment_id":1,"padding":"` + strings.Repeat("x", maxWebhookBodyBytes+1) + `"}`
	if w := postRaw(router, "/now", strings.NewReader(huge), nil); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("nowpayments: cuerpo sobre el tope debe dar 413, obtuve %d", w.Code)
	}
	if w := postRaw(router, "/dl", strings.NewReader(huge), nil); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("dlocalgo: cuerpo sobre el tope debe dar 413, obtuve %d", w.Code)
	}
	if w := postRaw(router, "/now", errReader{}, nil); w.Code != http.StatusBadRequest {
		t.Errorf("nowpayments: error de lectura debe dar 400, obtuve %d", w.Code)
	}

	// Firma inválida: sigue rechazándose (401), y lo que queda guardado está
	// acotado y ya resuelto (nunca reintentable).
	bigBad := `{"payment_id":42,"padding":"` + strings.Repeat("y", 10000) + `"}`
	if w := postRaw(router, "/now", strings.NewReader(bigBad), map[string]string{"x-nowpayments-sig": "00"}); w.Code != http.StatusUnauthorized {
		t.Errorf("nowpayments: firma inválida debe seguir dando 401, obtuve %d", w.Code)
	}
	if w := postRaw(router, "/dl", strings.NewReader(bigBad), map[string]string{"Authorization": "invalida"}); w.Code != http.StatusUnauthorized {
		t.Errorf("dlocalgo: firma inválida debe seguir dando 401, obtuve %d", w.Code)
	}
	for _, gw := range []string{"nowpayments", "dlocalgo"} {
		var raw sql.NullString
		var size sql.NullInt64
		var hash sql.NullString
		var processed sql.NullTime
		var outcome string
		if err := conn.QueryRow(`SELECT raw_body, body_size, body_sha256, processed_at, outcome FROM webhook_events WHERE gateway=$1 AND received_at >= $2 ORDER BY received_at DESC LIMIT 1`, gw, since).Scan(&raw, &size, &hash, &processed, &outcome); err != nil {
			t.Fatalf("%s: el rechazo debe quedar auditado: %v", gw, err)
		}
		if raw.Valid {
			t.Errorf("%s: un rechazo NO debe guardar ningún fragmento del cuerpo sin autenticar, obtuve %d bytes", gw, len(raw.String))
		}
		if !size.Valid || int(size.Int64) != len(bigBad) || !hash.Valid || len(hash.String) != 64 {
			t.Errorf("%s: deben quedar el tamaño (%d) y el SHA-256 del cuerpo (size=%v hash=%v)", gw, len(bigBad), size, hash)
		}
		if !processed.Valid || !strings.HasPrefix(outcome, "rejected") {
			t.Errorf("%s: el rechazo debe quedar resuelto y sin prefijo error: (processed=%v outcome=%q)", gw, processed.Valid, outcome)
		}
	}

	// NOWPayments con firma VÁLIDA: se guarda solo {"payment_id":N}.
	prevURL := nowPaymentsBaseURL
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"payment_status":"waiting","order_id":"no-es-uuid"}`)
	}))
	defer server.Close()
	nowPaymentsBaseURL = server.URL
	defer func() { nowPaymentsBaseURL = prevURL }()

	const sorted = `{"customer_email":"secreto-now@example.com","payment_id":555123,"payment_status":"finished"}`
	if w := postRaw(router, "/now", strings.NewReader(sorted), map[string]string{"x-nowpayments-sig": signNOWPayments("test-ipn-secret", sorted)}); w.Code != http.StatusOK {
		t.Fatalf("nowpayments con firma válida debe dar 200, obtuve %d: %s", w.Code, w.Body.String())
	}
	waitEventsProcessed(t, conn, "nowpayments", since)
	var stored string
	if err := conn.QueryRow(`SELECT raw_body FROM webhook_events WHERE gateway='nowpayments' AND received_at >= $1 AND outcome NOT LIKE 'rejected%'`, since).Scan(&stored); err != nil {
		t.Fatalf("el evento válido debe quedar registrado: %v", err)
	}
	if stored != `{"payment_id":555123}` {
		t.Errorf("solo debe guardarse el payment_id, obtuve %s", stored)
	}
	// Y sigue siendo reprocesable por RetryFailedWebhookEvents.
	if id, ok := parseNOWPaymentsPaymentID([]byte(stored)); !ok || id != 555123 {
		t.Errorf("el evento normalizado debe seguir siendo reintentable, parse=%d,%v", id, ok)
	}
}

// ── Retención ──

func TestPurgeProcessedWebhookEvents_SoloBorraProcesadosYNuncaLosReintentables(t *testing.T) {
	conn := setupShopTestDB(t)

	insert := func(gateway, outcome string, processed bool, age time.Duration) uuid.UUID {
		id := uuid.New()
		if _, err := conn.Exec(`INSERT INTO webhook_events (id, gateway, raw_body, outcome, received_at, processed_at)
			VALUES ($1,$2,'{}',$3, NOW() - $4::interval, CASE WHEN $5 THEN NOW() - $4::interval ELSE NULL END)`,
			id, gateway, sql.NullString{String: outcome, Valid: outcome != ""}, fmt.Sprintf("%d seconds", int64(age.Seconds())), processed); err != nil {
			t.Fatalf("insert: %v", err)
		}
		t.Cleanup(func() { conn.Exec(`DELETE FROM webhook_events WHERE id=$1`, id) })
		return id
	}
	exists := func(id uuid.UUID) bool {
		var n int
		conn.QueryRow(`SELECT COUNT(*) FROM webhook_events WHERE id=$1`, id).Scan(&n)
		return n == 1
	}

	old := webhookEventRetention + 24*time.Hour
	recent := 24 * time.Hour

	oldProcessed := insert("mercadopago", "processed", true, old)
	oldIgnoredPP := insert("paypal", "ignored: not completed", true, old)
	oldMPError := insert("mercadopago", "error: MP query failed", true, old) // sin reintento para MP: purgable
	recentProcessed := insert("mercadopago", "processed", true, recent)
	oldUnresolved := insert("nowpayments", "", false, old)
	oldNOWError := insert("nowpayments", "error: status query failed", true, old)
	oldNOWProcessed := insert("nowpayments", "processed", true, old)

	if _, err := db.PurgeProcessedWebhookEvents(conn, webhookEventRetention); err != nil {
		t.Fatalf("PurgeProcessedWebhookEvents: %v", err)
	}

	for name, id := range map[string]uuid.UUID{"procesado viejo mp": oldProcessed, "ignorado viejo pp": oldIgnoredPP, "error viejo mp (sin reintento)": oldMPError, "nowpayments procesado viejo": oldNOWProcessed} {
		if exists(id) {
			t.Errorf("%s debía purgarse", name)
		}
	}
	for name, id := range map[string]uuid.UUID{"procesado reciente": recentProcessed, "sin resolver (processed_at NULL)": oldUnresolved, "nowpayments con error (pendiente de reintento)": oldNOWError} {
		if !exists(id) {
			t.Errorf("%s NO debía purgarse", name)
		}
	}
	// El evento con error de NOWPayments sigue siendo candidato a reintento.
	unresolved, err := db.GetUnresolvedWebhookEvents(conn, "nowpayments", 0)
	if err != nil {
		t.Fatalf("GetUnresolvedWebhookEvents: %v", err)
	}
	found := false
	for _, ev := range unresolved {
		if ev.ID == oldNOWError {
			found = true
		}
	}
	if !found {
		t.Error("el evento con error de NOWPayments debe seguir apareciendo como reintentable tras la purga")
	}
}
