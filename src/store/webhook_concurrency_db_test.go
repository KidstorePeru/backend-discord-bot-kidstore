package store

// Concurrencia de webhook_events: un fallo atrasado no pisa un éxito, un evento
// resuelto sale de revisión con sus campos de reintento coherentes, y varios
// trabajadores/réplicas se coordinan con un lease (ClaimWebhookEvent) sin
// acreditar dos veces. La pasarela es un httptest.Server.

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"KidStoreStore/src/db"

	"github.com/google/uuid"
)

type eventRow struct {
	outcome                                   string
	attempts                                  int
	review, next, lastErr, claimed, processed bool
}

func readEvent(t *testing.T, conn *sql.DB, id uuid.UUID) eventRow {
	t.Helper()
	var r eventRow
	var out, lastErr sql.NullString
	var review, next, claimed, processed sql.NullTime
	if err := conn.QueryRow(`SELECT outcome, attempts, review_at, next_attempt_at, last_error, claimed_until, processed_at FROM webhook_events WHERE id=$1`, id).
		Scan(&out, &r.attempts, &review, &next, &lastErr, &claimed, &processed); err != nil {
		t.Fatalf("leer evento: %v", err)
	}
	r.outcome, r.review, r.next, r.lastErr, r.claimed, r.processed = out.String, review.Valid, next.Valid, lastErr.Valid, claimed.Valid, processed.Valid
	return r
}

func TestWebhookEvent_FalloAtrasadoNoSobrescribeUnExito(t *testing.T) {
	conn := setupShopTestDB(t)
	id := nowPaymentsEvent(t, conn, 890001)

	db.MarkWebhookEventProcessed(conn, id, "processed") // el trabajador rápido termina con éxito
	moved, _, err := db.RecordWebhookRetryFailure(conn, id, "error: status query failed", false, nowPaymentsMaxRetryAttempts, nowPaymentsRetryBackoff)
	if err != db.ErrWebhookEventAlreadySettled || moved {
		t.Fatalf("un fallo atrasado debe rechazarse con ErrWebhookEventAlreadySettled, obtuve moved=%v err=%v", moved, err)
	}
	// Ni por la vía "error:" genérica ni por el asentamiento de NOWPayments.
	db.MarkWebhookEventProcessed(conn, id, "error: otro fallo atrasado")
	settleNOWPaymentsEvent(conn, id, 890001, "error: status query failed")
	settleNOWPaymentsEvent(conn, id, 890001, "error: invalid order_id") // permanente: tampoco pasa a revisión

	r := readEvent(t, conn, id)
	if r.outcome != "processed" || r.review || r.next || r.lastErr || r.attempts != 0 {
		t.Fatalf("el éxito debe conservarse intacto: %+v", r)
	}
	// Un segundo resultado final tampoco reescribe el primero.
	db.MarkWebhookEventProcessed(conn, id, "ignored: algo distinto")
	if got := readEvent(t, conn, id).outcome; got != "processed" {
		t.Errorf("se conserva el primer resultado final, obtuve %q", got)
	}
}

func TestWebhookEvent_ExitoSacaDeRevisionYLimpiaCamposDeReintento(t *testing.T) {
	conn := setupShopTestDB(t)
	id := nowPaymentsEvent(t, conn, 890002)

	// Lo llevamos a revisión (error permanente) y luego llega un éxito de un trabajador lento.
	if moved, _, err := db.RecordWebhookRetryFailure(conn, id, "error: invalid order_id", true, nowPaymentsMaxRetryAttempts, nowPaymentsRetryBackoff); err != nil || !moved {
		t.Fatalf("preparar revisión: moved=%v err=%v", moved, err)
	}
	conn.Exec(`UPDATE webhook_events SET next_attempt_at=NOW()+INTERVAL '1 hour', claimed_until=NOW()+INTERVAL '1 hour' WHERE id=$1`, id)
	if r := readEvent(t, conn, id); !r.review || !r.lastErr {
		t.Fatalf("preparación incorrecta: %+v", r)
	}

	db.MarkWebhookEventProcessed(conn, id, "processed")
	r := readEvent(t, conn, id)
	if r.outcome != "processed" || r.review || r.next || r.lastErr || r.claimed || !r.processed {
		t.Fatalf("un evento resuelto debe salir de revisión y limpiar reintento/lease: %+v", r)
	}
	items, err := db.ListWebhookEventsInReview(conn, 200)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.ID == id {
			t.Error("un evento resuelto no debe seguir en la bandeja de revisión")
		}
	}
	// Tampoco es reintentable ni reclamable.
	conn.Exec(`UPDATE webhook_events SET received_at=NOW()-INTERVAL '1 day' WHERE id=$1`, id)
	pending, _ := db.GetUnresolvedWebhookEvents(conn, "nowpayments", 0)
	for _, ev := range pending {
		if ev.ID == id {
			t.Error("un evento resuelto no debe volver a la cola de reintentos")
		}
	}
	if ok, _ := db.ClaimWebhookEvent(conn, id, time.Minute); ok {
		t.Error("un evento resuelto no se puede reclamar")
	}
}

func TestClaimWebhookEvent_SoloUnTrabajadorLoObtieneYElLeaseVence(t *testing.T) {
	conn := setupShopTestDB(t)
	id := nowPaymentsEvent(t, conn, 890003)

	const n = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	var wins int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if ok, err := db.ClaimWebhookEvent(conn, id, time.Minute); err != nil {
				t.Errorf("claim: %v", err)
			} else if ok {
				atomic.AddInt32(&wins, 1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins != 1 {
		t.Fatalf("exactamente UN trabajador debe obtener el lease, lo obtuvieron %d", wins)
	}
	// Mientras el lease está vigente, el evento no aparece para el reintento.
	pending, _ := db.GetUnresolvedWebhookEvents(conn, "nowpayments", 0)
	for _, ev := range pending {
		if ev.ID == id {
			t.Error("un evento con lease vigente no debe listarse para otro trabajador")
		}
	}
	// Si el trabajador se cae, el lease vence y otro puede tomarlo.
	conn.Exec(`UPDATE webhook_events SET claimed_until=NOW()-INTERVAL '1 second' WHERE id=$1`, id)
	if ok, err := db.ClaimWebhookEvent(conn, id, time.Minute); err != nil || !ok {
		t.Errorf("con el lease vencido debe poder reclamarse: ok=%v err=%v", ok, err)
	}
	// Asentar el resultado libera el lease.
	db.MarkWebhookEventProcessed(conn, id, "processed")
	if readEvent(t, conn, id).claimed {
		t.Error("el lease debe liberarse al asentar el resultado")
	}
}

// Varias réplicas ejecutan RetryFailedWebhookEvents a la vez sobre el mismo
// evento: la pasarela se consulta y se acredita UNA sola vez.
func TestRetryFailedWebhookEvents_ReplicasConcurrentesAcreditanUnaSolaVez(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()

	txID := uuid.New()
	if _, err := conn.Exec(`
		INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, created_at, updated_at)
		VALUES ($1,$2,'nowpayments','kc_recharge','starter','Starter',10.40,2.80,800,'invoice-conc','pending',NOW(),NOW())`,
		txID, custID); err != nil {
		t.Fatalf("insert payment_transactions: %v", err)
	}
	defer conn.Exec(`DELETE FROM payment_transactions WHERE id=$1`, txID)

	calls := mockNOWPayments(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond) // ensancha la ventana de carrera
		fmt.Fprintf(w, `{"payment_status":"finished","order_id":%q}`, txID.String())
	})
	id := nowPaymentsEvent(t, conn, 890004)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			RetryFailedWebhookEvents(conn)
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("solo un trabajador debe procesar el evento (consultas a la pasarela: %d)", got)
	}
	var balance int
	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, custID).Scan(&balance)
	if balance != 800 {
		t.Errorf("debe acreditarse exactamente una vez (800), obtuve %d", balance)
	}
	if r := readEvent(t, conn, id); r.outcome != "processed" || r.review || r.next || r.claimed {
		t.Errorf("estado final incoherente: %+v", r)
	}
}

// Trabajador lento (éxito) vs trabajador rápido con fallo: sin importar el orden
// de llegada, el evento termina resuelto y fuera de revisión.
func TestWebhookEvent_ExitoYFalloConcurrentesTerminanSiempreResueltos(t *testing.T) {
	conn := setupShopTestDB(t)
	for i := 0; i < 20; i++ {
		id := nowPaymentsEvent(t, conn, int64(891000+i))
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			db.MarkWebhookEventProcessed(conn, id, "processed")
		}()
		go func() {
			defer wg.Done()
			<-start
			settleNOWPaymentsEvent(conn, id, 1, "error: invalid order_id")
		}()
		close(start)
		wg.Wait()
		r := readEvent(t, conn, id)
		// Si el fallo llegó primero, quedó en revisión y el éxito posterior lo saca de allí;
		// si el éxito llegó primero, el fallo se ignora. Ambos órdenes terminan igual.
		if r.outcome != "processed" || r.review || r.next || r.lastErr || !strings.HasPrefix(r.outcome, "processed") {
			t.Fatalf("iteración %d: estado final incoherente: %+v", i, r)
		}
	}
}
