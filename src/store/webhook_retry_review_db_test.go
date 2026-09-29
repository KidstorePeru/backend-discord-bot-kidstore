package store

// Pruebas de los reintentos de NOWPayments con backoff + bandeja de revisión
// manual, y de la retención de webhook_events (medida desde que el evento se
// PROCESÓ, no desde que se recibió). La pasarela es un httptest.Server.

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"KidStoreStore/src/db"

	"github.com/google/uuid"
)

func TestNOWPaymentsRetryBackoff_CreceYTieneTope(t *testing.T) {
	want := []time.Duration{3 * time.Minute, 6 * time.Minute, 12 * time.Minute, 24 * time.Minute, 48 * time.Minute, 96 * time.Minute, 192 * time.Minute, 6 * time.Hour}
	for i, w := range want {
		if got := nowPaymentsRetryBackoff(i + 1); got != w {
			t.Errorf("intento %d: esperaba %v, obtuve %v", i+1, w, got)
		}
	}
	if got := nowPaymentsRetryBackoff(50); got != nowPaymentsBackoffMax {
		t.Errorf("el backoff debe tener tope %v, obtuve %v", nowPaymentsBackoffMax, got)
	}
}

func TestIsPermanentNOWPaymentsOutcome(t *testing.T) {
	if !isPermanentNOWPaymentsOutcome("error: invalid order_id") {
		t.Error("order_id inválido no se arregla reintentando")
	}
	for _, transient := range []string{"error: status query failed", "error: no se pudo guardar el payment_id, pendiente de reintento", "error: crediting payment: boom"} {
		if isPermanentNOWPaymentsOutcome(transient) {
			t.Errorf("%q es recuperable, no debe ser permanente", transient)
		}
	}
}

// nowPaymentsEvent registra un IPN ya normalizado y "viejo" (para superar el
// margen nowPaymentsRetryMinAge) y devuelve su ID.
func nowPaymentsEvent(t *testing.T, conn *sql.DB, paymentID int64) uuid.UUID {
	t.Helper()
	id, err := db.LogWebhookEvent(conn, "nowpayments", fmt.Sprintf(`{"payment_id":%d}`, paymentID))
	if err != nil {
		t.Fatalf("LogWebhookEvent: %v", err)
	}
	t.Cleanup(func() { conn.Exec(`DELETE FROM webhook_events WHERE id=$1`, id) })
	if _, err := conn.Exec(`UPDATE webhook_events SET received_at=NOW()-INTERVAL '1 hour' WHERE id=$1`, id); err != nil {
		t.Fatalf("envejecer evento: %v", err)
	}
	return id
}

func eventState(t *testing.T, conn *sql.DB, id uuid.UUID) (attempts int, outcome string, inReview bool, hasNext bool) {
	t.Helper()
	var next, review sql.NullTime
	var out sql.NullString
	if err := conn.QueryRow(`SELECT attempts, outcome, next_attempt_at, review_at FROM webhook_events WHERE id=$1`, id).Scan(&attempts, &out, &next, &review); err != nil {
		t.Fatalf("leer estado del evento: %v", err)
	}
	return attempts, out.String, review.Valid, next.Valid
}

func mockNOWPayments(t *testing.T, handler http.HandlerFunc) (calls *int32) {
	t.Helper()
	var n int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		handler(w, r)
	}))
	prevURL := nowPaymentsBaseURL
	nowPaymentsBaseURL = server.URL
	withPaymentCfg(t, func(c *PaymentConfig) { c.NOWPaymentsAPIKey = "test-key" })
	t.Cleanup(func() { nowPaymentsBaseURL = prevURL; server.Close() })
	return &n
}

func TestRetryFailedWebhookEvents_ErrorRecuperable_BackoffYLuegoBandejaDeRevision(t *testing.T) {
	conn := setupShopTestDB(t)
	calls := mockNOWPayments(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	id := nowPaymentsEvent(t, conn, 880001)

	// Intento 1: falla → se cuenta, se agenda backoff, sigue reintentable.
	RetryFailedWebhookEvents(conn)
	attempts, outcome, inReview, hasNext := eventState(t, conn, id)
	if attempts != 1 || inReview || !hasNext || !strings.HasPrefix(outcome, "error:") {
		t.Fatalf("tras el 1er fallo: attempts=%d review=%v next=%v outcome=%q", attempts, inReview, hasNext, outcome)
	}
	if atomic.LoadInt32(calls) != 1 {
		t.Fatalf("esperaba 1 llamada a la pasarela, hubo %d", atomic.LoadInt32(calls))
	}

	// Inmediatamente después: el backoff impide repetir el mismo error.
	RetryFailedWebhookEvents(conn)
	if atomic.LoadInt32(calls) != 1 {
		t.Errorf("el backoff debe impedir un reintento inmediato (llamadas=%d)", atomic.LoadInt32(calls))
	}

	// Hasta agotar el umbral: cada intento llega solo cuando vence su backoff.
	for i := 2; i <= nowPaymentsMaxRetryAttempts; i++ {
		if _, err := conn.Exec(`UPDATE webhook_events SET next_attempt_at=NOW()-INTERVAL '1 second' WHERE id=$1`, id); err != nil {
			t.Fatalf("vencer backoff: %v", err)
		}
		RetryFailedWebhookEvents(conn)
		attempts, _, inReview, _ = eventState(t, conn, id)
		if attempts != i {
			t.Fatalf("intento %d: attempts=%d", i, attempts)
		}
		if i < nowPaymentsMaxRetryAttempts && inReview {
			t.Fatalf("no debe pasar a revisión antes del umbral (intento %d)", i)
		}
	}
	attempts, outcome, inReview, hasNext = eventState(t, conn, id)
	if !inReview || hasNext || !strings.HasPrefix(outcome, "review:") || attempts != nowPaymentsMaxRetryAttempts {
		t.Fatalf("tras agotar el umbral debe estar en revisión: attempts=%d review=%v next=%v outcome=%q", attempts, inReview, hasNext, outcome)
	}
	callsAtReview := atomic.LoadInt32(calls)

	// En revisión: NO se reintenta automáticamente, aunque pase el tiempo.
	conn.Exec(`UPDATE webhook_events SET next_attempt_at=NOW()-INTERVAL '1 day', processed_at=NOW()-INTERVAL '1 day' WHERE id=$1`, id)
	RetryFailedWebhookEvents(conn)
	if atomic.LoadInt32(calls) != callsAtReview {
		t.Error("un evento en revisión manual no debe reintentarse solo")
	}

	// Se CONSERVA con sus datos: aparece en la bandeja y la purga no lo toca.
	conn.Exec(`UPDATE webhook_events SET processed_at=NOW()-INTERVAL '400 days', received_at=NOW()-INTERVAL '400 days' WHERE id=$1`, id)
	if _, err := db.PurgeProcessedWebhookEvents(conn, webhookEventRetention); err != nil {
		t.Fatalf("purga: %v", err)
	}
	items, err := db.ListWebhookEventsInReview(conn, 100)
	if err != nil {
		t.Fatalf("ListWebhookEventsInReview: %v", err)
	}
	found := false
	for _, it := range items {
		if it.ID == id {
			found = true
			if !strings.Contains(it.RawBody, "880001") || it.Attempts != nowPaymentsMaxRetryAttempts {
				t.Errorf("la bandeja debe conservar el payment_id y los intentos: %+v", it)
			}
		}
	}
	if !found {
		t.Fatal("el evento en revisión debe seguir en la bandeja tras la purga (nunca se descarta en silencio)")
	}

	// Reencolar (manual retry) lo devuelve al ciclo automático.
	requeued, err := db.RequeueWebhookEvent(conn, id)
	if err != nil || !requeued {
		t.Fatalf("RequeueWebhookEvent: %v %v", requeued, err)
	}
	if again, _ := db.RequeueWebhookEvent(conn, id); again {
		t.Error("reencolar un evento que ya no está en revisión debe devolver false")
	}
	RetryFailedWebhookEvents(conn)
	if atomic.LoadInt32(calls) != callsAtReview+1 {
		t.Errorf("tras reencolar debe reintentarse (llamadas=%d, esperaba %d)", atomic.LoadInt32(calls), callsAtReview+1)
	}
	attempts, _, inReview, _ = eventState(t, conn, id)
	if attempts != 1 || inReview {
		t.Errorf("tras reencolar los intentos se reinician: attempts=%d review=%v", attempts, inReview)
	}
}

func TestRetryFailedWebhookEvents_ErrorNoRecuperable_VaDirectoAReviewSinRepetir(t *testing.T) {
	conn := setupShopTestDB(t)
	calls := mockNOWPayments(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"payment_status":"finished","order_id":"no-es-un-uuid-nuestro"}`)
	})
	id := nowPaymentsEvent(t, conn, 880002)

	RetryFailedWebhookEvents(conn)
	attempts, outcome, inReview, hasNext := eventState(t, conn, id)
	if !inReview || hasNext || attempts != 1 || !strings.HasPrefix(outcome, "review:") {
		t.Fatalf("un error no recuperable debe ir a revisión en el 1er intento: attempts=%d review=%v next=%v outcome=%q", attempts, inReview, hasNext, outcome)
	}
	// Los barridos siguientes NO vuelven a consultar a la pasarela por él.
	before := atomic.LoadInt32(calls)
	RetryFailedWebhookEvents(conn)
	if atomic.LoadInt32(calls) != before {
		t.Error("no debe repetirse un error permanente")
	}
}

func TestRetryFailedWebhookEvents_FirmaInvalidaNuncaEsReintentable(t *testing.T) {
	conn := setupShopTestDB(t)
	calls := mockNOWPayments(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	if err := logRejectedWebhook(conn, "nowpayments", "invalid signature", []byte(`{"payment_id":880003}`)); err != nil {
		t.Fatalf("logRejectedWebhook: %v", err)
	}
	defer conn.Exec(`DELETE FROM webhook_events WHERE gateway='nowpayments' AND outcome='rejected: invalid signature' AND body_size=$1`, len(`{"payment_id":880003}`))
	conn.Exec(`UPDATE webhook_events SET received_at=NOW()-INTERVAL '2 hours' WHERE outcome='rejected: invalid signature' AND body_size=$1`, len(`{"payment_id":880003}`))

	RetryFailedWebhookEvents(conn)
	if atomic.LoadInt32(calls) != 0 {
		t.Errorf("un evento rechazado por firma inválida jamás debe reintentarse (llamadas=%d)", atomic.LoadInt32(calls))
	}
}

// ── Retención ──

func TestPurgeProcessedWebhookEvents_LaRetencionSeMideDesdeElProcesamiento(t *testing.T) {
	conn := setupShopTestDB(t)
	insert := func(gateway, outcome string, receivedAgo, processedAgo time.Duration, review bool) uuid.UUID {
		id := uuid.New()
		var reviewAt interface{}
		if review {
			reviewAt = time.Now().Add(-processedAgo)
		}
		if _, err := conn.Exec(`INSERT INTO webhook_events (id, gateway, raw_body, outcome, received_at, processed_at, review_at)
			VALUES ($1,$2,'{"payment_id":1}',$3, NOW() - $4::interval, NOW() - $5::interval, $6)`,
			id, gateway, outcome, fmt.Sprintf("%d seconds", int64(receivedAgo.Seconds())), fmt.Sprintf("%d seconds", int64(processedAgo.Seconds())), reviewAt); err != nil {
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
	day := 24 * time.Hour

	// REGRESIÓN: recibido hace 40 días, pero se acaba de resolver (tras varios
	// reintentos) — debe conservarse todo el período de retención.
	receivedLongAgoJustResolved := insert("nowpayments", "processed", 40*day, 0, false)
	receivedLongAgoResolvedYesterday := insert("nowpayments", "processed", 40*day, 1*day, false)
	// Resuelto hace más que la retención: se purga.
	resolvedLongAgo := insert("mercadopago", "processed", 45*day, 31*day, false)
	// Reciente: se conserva.
	recent := insert("paypal", "processed", 2*day, 1*day, false)
	// En revisión manual, aunque sea viejo: se conserva.
	oldReview := insert("nowpayments", "review: status query failed", 90*day, 60*day, true)
	// Con error reintentable (NOWPayments): se conserva.
	oldNOWError := insert("nowpayments", "error: status query failed", 90*day, 60*day, false)
	// Sin resolver: se conserva.
	unresolvedID := uuid.New()
	conn.Exec(`INSERT INTO webhook_events (id, gateway, raw_body, received_at) VALUES ($1,'nowpayments','{"payment_id":2}', NOW()-INTERVAL '90 days')`, unresolvedID)
	t.Cleanup(func() { conn.Exec(`DELETE FROM webhook_events WHERE id=$1`, unresolvedID) })

	if _, err := db.PurgeProcessedWebhookEvents(conn, webhookEventRetention); err != nil {
		t.Fatalf("PurgeProcessedWebhookEvents: %v", err)
	}

	if !exists(receivedLongAgoJustResolved) {
		t.Error("REGRESIÓN: un evento recibido hace 40 días que acaba de resolverse NO debe eliminarse de inmediato")
	}
	if !exists(receivedLongAgoResolvedYesterday) {
		t.Error("un evento resuelto ayer (recibido hace 40 días) debe conservarse")
	}
	if exists(resolvedLongAgo) {
		t.Error("un evento resuelto hace más de la retención debe purgarse")
	}
	for name, id := range map[string]uuid.UUID{"reciente": recent, "en revisión": oldReview, "con error reintentable": oldNOWError, "sin resolver": unresolvedID} {
		if !exists(id) {
			t.Errorf("el evento %s NO debía purgarse", name)
		}
	}
}

func TestIsPermanentNOWPaymentsOutcome_TransaccionInexistente(t *testing.T) {
	if !isPermanentNOWPaymentsOutcome("error: transaction not found") {
		t.Error("una transacción local inexistente no se arregla reintentando: va a revisión")
	}
}

func TestSetProviderPaymentID_ExigeUnaFilaDeNOWPayments(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()

	insert := func(gateway string) uuid.UUID {
		id := uuid.New()
		if _, err := conn.Exec(`INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, created_at, updated_at)
			VALUES ($1,$2,$3,'kc_recharge','starter','Starter',10.40,2.80,800,'ext','pending',NOW(),NOW())`, id, custID, gateway); err != nil {
			t.Fatalf("insert: %v", err)
		}
		t.Cleanup(func() { conn.Exec(`DELETE FROM payment_transactions WHERE id=$1`, id) })
		return id
	}
	now := insert("nowpayments")
	other := insert("mercadopago")

	if err := db.SetProviderPaymentID(conn, now, "777"); err != nil {
		t.Fatalf("una transacción de NOWPayments debe guardarse: %v", err)
	}
	if err := db.SetProviderPaymentID(conn, uuid.New(), "777"); err != db.ErrPaymentTransactionNotFound {
		t.Errorf("transacción inexistente: esperaba ErrPaymentTransactionNotFound, obtuve %v", err)
	}
	if err := db.SetProviderPaymentID(conn, other, "777"); err != db.ErrPaymentTransactionNotFound {
		t.Errorf("transacción de otra pasarela: esperaba ErrPaymentTransactionNotFound, obtuve %v", err)
	}
	var stored sql.NullString
	conn.QueryRow(`SELECT provider_payment_id FROM payment_transactions WHERE id=$1`, other).Scan(&stored)
	if stored.Valid && stored.String != "" {
		t.Errorf("no debe escribirse provider_payment_id en un pago de otra pasarela: %q", stored.String)
	}
}

// Un IPN cuyo order_id no corresponde a ninguna transacción local NO se marca
// como procesado: se conserva en la bandeja de revisión con sus datos.
func TestRetryFailedWebhookEvents_TransaccionLocalInexistenteQuedaEnRevision(t *testing.T) {
	conn := setupShopTestDB(t)
	ghost := uuid.New() // UUID válido sin transacción local
	calls := mockNOWPayments(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"payment_status":"waiting","order_id":%q}`, ghost.String())
	})
	id := nowPaymentsEvent(t, conn, 880003)

	RetryFailedWebhookEvents(conn)
	attempts, outcome, inReview, hasNext := eventState(t, conn, id)
	if !inReview || hasNext || attempts != 1 || !strings.HasPrefix(outcome, "review:") {
		t.Fatalf("debe quedar en revisión, no procesado: attempts=%d review=%v next=%v outcome=%q", attempts, inReview, hasNext, outcome)
	}
	var processed sql.NullTime
	conn.QueryRow(`SELECT processed_at FROM webhook_events WHERE id=$1`, id).Scan(&processed)
	if processed.Valid && !strings.HasPrefix(outcome, "review:") {
		t.Error("el evento no debe marcarse como procesado con éxito")
	}
	before := atomic.LoadInt32(calls)
	RetryFailedWebhookEvents(conn)
	if atomic.LoadInt32(calls) != before {
		t.Error("un evento en revisión no se reintenta solo")
	}
}
