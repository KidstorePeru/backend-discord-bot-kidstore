package store

// Regresiones de la persistencia de entregas (auditorías de septiembre 2026):
//   - si Epic CONFIRMA la entrega pero no se puede guardar en la base, el pedido
//     nunca se marca 'sent' sin evidencia, el regalo no se reenvía, los cupos y
//     V-Bucks del bot se descuentan una sola vez y el KC del cliente no se toca;
//   - la entrega pendiente sobrevive a un reinicio (diario en disco) y su
//     recuperación es segura entre varios procesos;
//   - una respuesta de Epic sin confirmación verificable (cuerpo vacío, 204,
//     HTML) no se registra como entrega: revisión manual, sin reenvío ni reembolso;
//   - ninguna escritura atrasada revierte un pedido entregado ni permite
//     reembolsarlo si tiene evidencia de entrega.
// El fallo de escritura se provoca con un trigger de Postgres limitado al
// pedido de la prueba (no afecta a otras pruebas que compartan la base).

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"KidStoreStore/src/db"
	"KidStoreStore/src/types"

	"github.com/google/uuid"
)

// failDeliveryWrites hace que cualquier UPDATE que guarde evidencia de entrega
// en ESTE pedido falle, hasta que se llame a la función devuelta.
func failDeliveryWrites(t *testing.T, conn *sql.DB, orderID uuid.UUID) (restore func()) {
	t.Helper()
	name := "fail_delivery_" + strings.ReplaceAll(orderID.String(), "-", "")
	stmts := []string{
		fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $$
			BEGIN RAISE EXCEPTION 'escritura de entrega caída (prueba)'; END; $$ LANGUAGE plpgsql`, name),
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON orders FOR EACH ROW
			WHEN (NEW.id = '%s' AND NEW.delivery_evidence IS NOT NULL) EXECUTE FUNCTION %s()`, name, orderID, name),
	}
	for _, s := range stmts {
		if _, err := conn.Exec(s); err != nil {
			t.Fatalf("preparar trigger: %v", err)
		}
	}
	done := false
	restore = func() {
		if done {
			return
		}
		done = true
		conn.Exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON orders`, name))
		conn.Exec(fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
	}
	t.Cleanup(restore)
	return restore
}

type orderSnapshot struct {
	status        string
	evidence      sql.NullString
	sendAttempted bool
}

func readOrder(t *testing.T, conn *sql.DB, id uuid.UUID) orderSnapshot {
	t.Helper()
	var s orderSnapshot
	if err := conn.QueryRow(`SELECT status, delivery_evidence, send_attempted FROM orders WHERE id=$1`, id).
		Scan(&s.status, &s.evidence, &s.sendAttempted); err != nil {
		t.Fatalf("leer pedido: %v", err)
	}
	return s
}

// setupDeliveryTest: sin esperas entre reintentos y con un diario de entregas
// propio de la prueba (nunca el del sistema).
func setupDeliveryTest(t *testing.T) {
	prevDelays, prevDir := deliveryPersistRetryDelays, deliveryJournalDir
	deliveryPersistRetryDelays = []time.Duration{0, 0}
	deliveryJournalDir = t.TempDir()
	t.Cleanup(func() { deliveryPersistRetryDelays, deliveryJournalDir = prevDelays, prevDir })
}

// simulateRestart borra la memoria del proceso (pero no el diario en disco).
func simulateRestart() {
	unpersistedDeliveriesMu.Lock()
	unpersistedDeliveries = map[uuid.UUID]unpersistedDelivery{}
	unpersistedDeliveriesMu.Unlock()
}

// Prepara cliente, pedido reclamado, bot y un Epic simulado que cuenta los
// envíos. giftOutcome decide la respuesta a partir del número de envío (1, 2…).
func deliveryScenario(t *testing.T, conn *sql.DB, giftOutcome func(n int32, w http.ResponseWriter)) (types.Order, types.GameAccount, uuid.UUID, *int32) {
	t.Helper()
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	t.Cleanup(cleanup)
	creditApprovedPayment(t, conn, custID, 800)
	order := buyItemWithKC(t, conn, custID, 500)
	claimed := claimOrder(t, conn, order.ID)
	t.Cleanup(func() { forgetUnpersistedDelivery(order.ID) })

	bot := types.GameAccount{
		ID: uuid.New(), DisplayName: "bot_delivery_persist",
		RemainingGifts: 5, VBucks: 10000,
		AccessToken: "tok", AccessTokenExpDate: time.Now().Add(24 * time.Hour),
	}
	insertBotAccount(t, conn, bot)
	var gifts int32
	server := epicMockServer(t, "receiver-delivery-persist-000001", strings.ReplaceAll(bot.ID.String(), "-", ""),
		func(w http.ResponseWriter, r *http.Request) { giftOutcome(atomic.AddInt32(&gifts, 1), w) })
	withMockedEpicURLs(t, server.URL)
	return claimed, bot, custID, &gifts
}

func deliveredOK(_ int32, w http.ResponseWriter) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"profileRevision":1,"profileId":"common_core"}`)
}

func reclaim(t *testing.T, conn *sql.DB, id uuid.UUID) types.Order {
	t.Helper()
	conn.Exec(`UPDATE orders SET updated_at=NOW()-INTERVAL '20 minutes' WHERE id=$1`, id)
	return claimOrder(t, conn, id)
}

func TestProcessOrder_FalloAlGuardarEvidencia_SeRecuperaSinReenviar(t *testing.T) {
	conn := setupShopTestDB(t)
	setupDeliveryTest(t)
	order, bot, custID, gifts := deliveryScenario(t, conn, deliveredOK)
	restore := failDeliveryWrites(t, conn, order.ID)

	processOrder(conn, order, []types.GameAccount{bot})

	got := readOrder(t, conn, order.ID)
	if got.status == "sent" {
		t.Fatal("sin poder guardar la evidencia, el pedido NO debe quedar 'sent' (quedaba sin trazabilidad)")
	}
	if !got.sendAttempted {
		t.Error("la marca de intento debe seguir en true: protege contra un reembolso a ciegas si el proceso se reinicia")
	}
	if n := atomic.LoadInt32(gifts); n != 1 {
		t.Fatalf("se esperaba exactamente 1 envío a Epic, hubo %d", n)
	}
	if g, v := botAccountState(t, conn, bot.ID); g != 4 || v != 10000-500 {
		t.Errorf("el envío real descuenta 1 cupo y 500 V-Bucks una sola vez: cupos=%d vbucks=%d", g, v)
	}

	// La base se recupera; el pedido vuelve a reclamarse (lease de 15 min vencido).
	restore()
	reclaimed := reclaim(t, conn, order.ID)
	if rest := withoutUnpersistedDeliveries(conn, []types.Order{reclaimed}); len(rest) != 0 {
		t.Fatal("un pedido con la entrega pendiente de guardar no debe volver al envío ni al reembolso")
	}

	got = readOrder(t, conn, order.ID)
	if got.status != "sent" || !got.evidence.Valid || !db.ValidDeliveryEvidence(got.evidence.String) || got.sendAttempted {
		t.Fatalf("la recuperación debe guardar 'sent' + la evidencia real de Epic y limpiar la marca: %+v", got)
	}
	if n := atomic.LoadInt32(gifts); n != 1 {
		t.Errorf("la recuperación no debe reenviar el regalo: hubo %d envíos", n)
	}
	if g, v := botAccountState(t, conn, bot.ID); g != 4 || v != 10000-500 {
		t.Errorf("la recuperación no debe volver a descontar al bot: cupos=%d vbucks=%d", g, v)
	}
	if bal := kcBalance(t, conn, custID); bal != 300 {
		t.Errorf("el KC del cliente no cambia (800-500=300), obtuve %d", bal)
	}
	if _, err := os.Stat(deliveryJournalPath(order.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Error("tras guardarse la entrega, su entrada del diario debe borrarse")
	}
}

// Tras un reinicio (se pierde la memoria), la entrega pendiente se recupera
// desde el diario en disco: 'sent' con la evidencia real, sin reenviar.
func TestProcessOrder_FalloAlGuardarEvidenciaYReinicio_SeRecuperaDesdeElDiario(t *testing.T) {
	conn := setupShopTestDB(t)
	setupDeliveryTest(t)
	order, bot, custID, gifts := deliveryScenario(t, conn, deliveredOK)
	restore := failDeliveryWrites(t, conn, order.ID)

	processOrder(conn, order, []types.GameAccount{bot})
	if _, err := os.Stat(deliveryJournalPath(order.ID)); err != nil {
		t.Fatalf("la entrega sin persistir debe quedar en el diario en disco: %v", err)
	}
	restore()
	simulateRestart()

	recoverUnpersistedDeliveries(conn)
	got := readOrder(t, conn, order.ID)
	if got.status != "sent" || !db.ValidDeliveryEvidence(got.evidence.String) {
		t.Fatalf("tras el reinicio, el diario debe recuperar la entrega: %+v", got)
	}
	if n := atomic.LoadInt32(gifts); n != 1 {
		t.Errorf("no debe reenviarse el regalo: hubo %d envíos", n)
	}
	if bal := kcBalance(t, conn, custID); bal != 300 {
		t.Errorf("sin reembolso: saldo esperado 300, obtuve %d", bal)
	}
}

// Si se pierden la memoria Y el diario (redespliegue), el pedido reclamado va a
// revisión SIN volver a llamar a Epic y sin reembolso.
func TestProcessOrder_FalloAlGuardarEvidenciaSinDiario_QuedaEnRevisionSinReenviarNiReembolsar(t *testing.T) {
	conn := setupShopTestDB(t)
	setupDeliveryTest(t)
	order, bot, custID, gifts := deliveryScenario(t, conn, deliveredOK)
	restore := failDeliveryWrites(t, conn, order.ID)

	processOrder(conn, order, []types.GameAccount{bot})
	restore()
	forgetUnpersistedDelivery(order.ID) // memoria y diario perdidos

	reclaimed := reclaim(t, conn, order.ID)
	botNow := bot
	botNow.RemainingGifts, botNow.VBucks = botAccountState(t, conn, bot.ID)
	processOrder(conn, reclaimed, []types.GameAccount{botNow})

	if got := readOrder(t, conn, order.ID); got.status != "review" {
		t.Fatalf("la entrega incierta debe quedar en revisión, obtuve %q", got.status)
	}
	if n := atomic.LoadInt32(gifts); n != 1 {
		t.Errorf("un intento interrumpido no se reenvía: hubo %d envíos", n)
	}
	if bal := kcBalance(t, conn, custID); bal != 300 {
		t.Errorf("no debe reembolsarse a ciegas (saldo esperado 300), obtuve %d", bal)
	}
	if g, v := botAccountState(t, conn, bot.ID); g != 4 || v != 10000-500 {
		t.Errorf("el bot solo se descuenta por el envío real: cupos=%d vbucks=%d", g, v)
	}
}

// Varios procesos recuperan la misma entrega a la vez: queda 'sent' una vez,
// con la evidencia, sin errores ni reembolsos.
func TestRetryUnpersistedDelivery_VariosProcesosALaVez(t *testing.T) {
	conn := setupShopTestDB(t)
	setupDeliveryTest(t)
	order, bot, custID, gifts := deliveryScenario(t, conn, deliveredOK)
	restore := failDeliveryWrites(t, conn, order.ID)
	processOrder(conn, order, []types.GameAccount{bot})
	restore()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recoverUnpersistedDeliveries(conn) // cada "proceso" lee el diario compartido
		}()
	}
	wg.Wait()
	if got := readOrder(t, conn, order.ID); got.status != "sent" || !db.ValidDeliveryEvidence(got.evidence.String) {
		t.Fatalf("la entrega debe quedar registrada una vez: %+v", got)
	}
	if n := atomic.LoadInt32(gifts); n != 1 {
		t.Errorf("hubo %d envíos", n)
	}
	if bal := kcBalance(t, conn, custID); bal != 300 {
		t.Errorf("saldo esperado 300, obtuve %d", bal)
	}
}

// Epic responde 2xx sin una confirmación verificable: NO se registra la
// entrega (antes quedaba 'sent' con evidencia vacía), no se reenvía, no se
// reembolsa y el bot no se descuenta. Revisión manual.
func TestProcessOrder_RespuestaSinConfirmacion_QuedaEnRevision(t *testing.T) {
	for name, outcome := range map[string]func(int32, http.ResponseWriter){
		"200 vacío": func(_ int32, w http.ResponseWriter) { w.WriteHeader(http.StatusOK) },
		"204":       func(_ int32, w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) },
		"200 HTML":  func(_ int32, w http.ResponseWriter) { w.WriteHeader(http.StatusOK); fmt.Fprint(w, "<html>ok</html>") },
		"200 {}":    func(_ int32, w http.ResponseWriter) { w.WriteHeader(http.StatusOK); fmt.Fprint(w, "{}") },
	} {
		t.Run(name, func(t *testing.T) {
			conn := setupShopTestDB(t)
			setupDeliveryTest(t)
			order, bot, custID, gifts := deliveryScenario(t, conn, outcome)

			processOrder(conn, order, []types.GameAccount{bot})

			got := readOrder(t, conn, order.ID)
			if got.status != "review" || got.evidence.Valid {
				t.Fatalf("una respuesta no concluyente debe ir a revisión sin evidencia: %+v", got)
			}
			if bal := kcBalance(t, conn, custID); bal != 300 {
				t.Errorf("sin reembolso a ciegas: saldo esperado 300, obtuve %d", bal)
			}
			if g, v := botAccountState(t, conn, bot.ID); g != 5 || v != 10000 {
				t.Errorf("sin confirmación no se descuenta al bot: cupos=%d vbucks=%d", g, v)
			}
			// Un nuevo ciclo no lo vuelve a intentar (está en revisión).
			if pending, _ := db.ClaimPendingOrders(conn); len(pending) > 0 {
				for _, p := range pending {
					if p.ID == order.ID {
						t.Error("un pedido en revisión no debe volver a reclamarse")
					}
				}
			}
			if n := atomic.LoadInt32(gifts); n != 1 {
				t.Errorf("no debe reenviarse: hubo %d envíos", n)
			}
		})
	}
}

// Ninguna escritura atrasada revierte un pedido entregado, ni se puede
// reembolsar un pedido con evidencia de entrega.
func TestPedidoEntregado_NoLoRevierteNiReembolsaUnaRespuestaAtrasada(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	order := newShopTestOrder(t, conn, custID, 100)
	evidence := `{"epic_response":{"profileRevision":1,"profileId":"common_core"},"offer_id":"offer-test","receiver_account_id":"r1"}`
	if err := db.MarkOrderDelivered(conn, order.ID, uuid.New(), evidence); err != nil {
		t.Fatalf("MarkOrderDelivered: %v", err)
	}

	if err := db.UpdateOrderStatus(conn, order.ID, "pending", nil, nil); !errors.Is(err, db.ErrOrderNotActive) {
		t.Errorf("volver a 'pending' un pedido entregado debe rechazarse, obtuve %v", err)
	}
	if _, err := db.MarkOrderSendAttempted(conn, order.ID, true); !errors.Is(err, db.ErrOrderNotActive) {
		t.Errorf("un trabajador atrasado no puede empezar otro envío, obtuve %v", err)
	}
	db.SetOrderReviewOrClear(conn, order.ID, true, "respuesta atrasada")
	failOrderAndRefund(conn, order, "fallo atrasado", "fallo atrasado")
	if got := readOrder(t, conn, order.ID); got.status != "sent" {
		t.Fatalf("un pedido entregado no debe cambiar de estado, quedó %q", got.status)
	}
	if bal := kcBalance(t, conn, custID); bal != 0 {
		t.Errorf("no debe reembolsarse un pedido entregado, saldo %d", bal)
	}

	// Aunque un admin lo haya puesto en revisión, con evidencia no se reembolsa.
	conn.Exec(`UPDATE orders SET status='review' WHERE id=$1`, order.ID)
	if err := db.RefundOrder(conn, order.ID); !errors.Is(err, db.ErrOrderHasDeliveryEvidence) {
		t.Errorf("RefundOrder debe rechazar un pedido con evidencia de entrega, obtuve %v", err)
	}
	if err := db.ResolveReviewOrder(conn, order.ID, "refund", "admin-test"); err == nil {
		t.Error("tampoco desde la resolución manual de revisión")
	}
	if bal := kcBalance(t, conn, custID); bal != 0 {
		t.Errorf("saldo esperado 0, obtuve %d", bal)
	}
}

// MarkOrderDelivered rechaza evidencia vacía o inválida.
func TestMarkOrderDelivered_RechazaEvidenciaVaciaOInvalida(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	order := newShopTestOrder(t, conn, custID, 100)
	for _, ev := range []string{"", "   ", "null", "{}", "not json", `{"epic_response":{}}`, `{"epic_response":{"a":1},"offer_id":""}`} {
		if err := db.MarkOrderDelivered(conn, order.ID, uuid.New(), ev); !errors.Is(err, db.ErrInvalidDeliveryEvidence) {
			t.Errorf("evidencia %q: esperaba ErrInvalidDeliveryEvidence, obtuve %v", ev, err)
		}
	}
	if got := readOrder(t, conn, order.ID); got.status == "sent" || got.evidence.Valid {
		t.Fatalf("no debe registrarse la entrega: %+v", got)
	}
}

// MarkOrderDelivered nunca pisa un pedido ya reembolsado.
func TestRetryUnpersistedDelivery_NoPisaUnPedidoReembolsado(t *testing.T) {
	conn := setupShopTestDB(t)
	setupDeliveryTest(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	order := newShopTestOrder(t, conn, custID, 100)
	conn.Exec(`UPDATE orders SET status='refunded' WHERE id=$1`, order.ID)

	unpersistedDeliveriesMu.Lock()
	unpersistedDeliveries[order.ID] = unpersistedDelivery{OrderID: order.ID, CustomerID: custID, BotID: uuid.New(),
		Evidence: `{"epic_response":{"profileRevision":1,"profileId":"common_core"},"offer_id":"offer-test","receiver_account_id":"r1"}`}
	unpersistedDeliveriesMu.Unlock()
	t.Cleanup(func() { forgetUnpersistedDelivery(order.ID) })

	recoverUnpersistedDeliveries(conn)
	if got := readOrder(t, conn, order.ID); got.status != "refunded" || got.evidence.Valid {
		t.Fatalf("un pedido reembolsado no debe pasar a 'sent': %+v", got)
	}
	unpersistedDeliveriesMu.Lock()
	_, still := unpersistedDeliveries[order.ID]
	unpersistedDeliveriesMu.Unlock()
	if still {
		t.Error("tras avisar del conflicto, la entrega pendiente se descarta (no se reintenta para siempre)")
	}
}

// Una entrada dañada del diario se aparta (.bad) y no rompe la recuperación.
func TestLoadDeliveryJournal_EntradaDanadaSeAparta(t *testing.T) {
	setupDeliveryTest(t)
	bad := filepath.Join(deliveryJournalDir, uuid.NewString()+".json")
	if err := os.WriteFile(bad, []byte(`{"order_id":`), 0o600); err != nil {
		t.Fatal(err)
	}
	loadDeliveryJournal()
	if _, err := os.Stat(bad + ".bad"); err != nil {
		t.Errorf("la entrada dañada debe apartarse como .bad: %v", err)
	}
}
