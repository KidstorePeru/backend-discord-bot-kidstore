package store

// Pruebas de integración para failOrderAndRefund — a diferencia de
// shop_test.go (funciones puras, sin DB), estas ejercitan la coordinación
// real entre persistencia (send_attempted, status del pedido, balance del
// cliente) y el resultado (reembolso vs. revisión manual). Cubren las
// secuencias completas del punto 1 y 6 del pedido de correcciones:
//   - Entrega realizada con respuesta perdida / reinicio antes de
//     persistir la entrega: un intento de envío queda marcado sin
//     resolver (send_attempted=true) y NO debe reembolsarse a ciegas.
//   - Ausencia de reembolsos duplicados cuando el reembolso falla, y que
//     el resultado (refunded=true/false) que ve el correo refleje lo que
//     de verdad pasó en la base de datos.
//
// Usa exactamente el mismo contrato TEST_DB_* que src/db/db_test.go (ver
// ese archivo para la explicación completa de por qué nunca se conecta a
// producción) — se duplica un setupTestDB mínimo acá porque Go no permite
// importar los helpers _test.go de otro paquete.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"KidStoreStore/src/db"
	"KidStoreStore/src/types"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

var shopTestDB *sql.DB

func setupShopTestDB(t *testing.T) *sql.DB {
	t.Helper()
	if shopTestDB != nil {
		return shopTestDB
	}
	godotenv.Load(".env.test")
	godotenv.Load("../../.env.test")
	host := os.Getenv("TEST_DB_HOST")
	if host == "" {
		t.Skip("TEST_DB_HOST no configurado — se salta esta prueba de integración (necesita una base de datos de PRUEBA separada, nunca la de producción; ver .env.test.example)")
	}
	dbName := os.Getenv("TEST_DB_NAME")
	if host == os.Getenv("DB_HOST") && dbName != "" && dbName == os.Getenv("DB_NAME") {
		t.Fatal("TEST_DB_HOST/TEST_DB_NAME apuntan a la MISMA base que DB_HOST/DB_NAME (producción) — configura una base de datos de pruebas separada")
	}
	port := os.Getenv("TEST_DB_PORT")
	if port == "" {
		port = "5432"
	}
	sslMode := os.Getenv("TEST_DB_SSLMODE")
	if sslMode == "" {
		sslMode = "disable"
	}
	psqlInfo := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		host, port, os.Getenv("TEST_DB_USER"), os.Getenv("TEST_DB_PASSWORD"), dbName, sslMode)
	conn, err := sql.Open("postgres", psqlInfo)
	if err != nil {
		t.Fatalf("no se pudo abrir la conexión de prueba: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Skipf("no se pudo conectar a la base de datos de prueba: %v", err)
	}
	if err := db.CreateTables(conn); err != nil {
		t.Fatalf("no se pudo preparar el esquema en la base de pruebas: %v", err)
	}
	shopTestDB = conn
	return shopTestDB
}

func newShopTestCustomer(t *testing.T, conn *sql.DB, kcBalance int) (uuid.UUID, func()) {
	t.Helper()
	id := uuid.New()
	suffix := id.String()[:8]
	_, err := conn.Exec(`
		INSERT INTO customers (id, epic_username, email, password_hash, kc_balance, is_verified, is_active, is_admin, has_password, created_at, updated_at)
		VALUES ($1,$2,$3,'x',$4,true,true,false,true,NOW(),NOW())`,
		id, "shoptest_"+suffix, "shoptest-"+suffix+"@example.com", kcBalance)
	if err != nil {
		t.Fatalf("no se pudo crear el cliente de prueba: %v", err)
	}
	return id, func() {
		conn.Exec(`DELETE FROM audit_logs WHERE customer_id=$1`, id)
		conn.Exec(`DELETE FROM orders WHERE customer_id=$1`, id)
		conn.Exec(`DELETE FROM customers WHERE id=$1`, id)
	}
}

func newShopTestOrder(t *testing.T, conn *sql.DB, customerID uuid.UUID, priceKC int) types.Order {
	t.Helper()
	o := types.Order{
		ID: uuid.New(), CustomerID: customerID, EpicUsername: "receiver_test",
		ItemOfferID: "offer-test", ItemName: "Item de prueba", PriceKC: priceKC, PriceVBucks: priceKC,
	}
	_, err := conn.Exec(`
		INSERT INTO orders (id, customer_id, epic_username, item_offer_id, item_name, price_kc, price_vbucks, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$6,'processing',NOW(),NOW())`,
		o.ID, o.CustomerID, o.EpicUsername, o.ItemOfferID, o.ItemName, priceKC)
	if err != nil {
		t.Fatalf("no se pudo crear el pedido de prueba: %v", err)
	}
	return o
}

// TestFailOrderAndRefund_EntregaInciertaQuedaEnRevision cubre "entrega
// realizada con respuesta perdida" y "reinicio antes de persistir la
// entrega": si queda un intento de envío sin resolver (send_attempted=true,
// ver MarkOrderSendAttempted), failOrderAndRefund NO debe reembolsar a
// ciegas — el ítem podría haberse entregado de verdad y el proceso solo
// perdió la confirmación. Debe quedar en revisión manual, sin tocar el
// balance del cliente.
func TestFailOrderAndRefund_EntregaInciertaQuedaEnRevision(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 500)
	defer cleanup()
	order := newShopTestOrder(t, conn, custID, 100)

	// Simula que un intento de SendGift quedó interrumpido (timeout,
	// reinicio) sin que se supiera el resultado — exactamente lo que deja
	// send_attempted=true sin limpiar.
	if _, err := db.MarkOrderSendAttempted(conn, order.ID, true); err != nil {
		t.Fatalf("MarkOrderSendAttempted: %v", err)
	}

	failOrderAndRefund(conn, order, "motivo interno de prueba", "motivo para el cliente")

	var status string
	if err := conn.QueryRow(`SELECT status FROM orders WHERE id=$1`, order.ID).Scan(&status); err != nil {
		t.Fatalf("no se pudo leer el status del pedido: %v", err)
	}
	if status != "review" {
		t.Errorf("con un intento de envío sin resolver, el pedido debería quedar en 'review', quedó en %q", status)
	}

	var balance int
	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, custID).Scan(&balance)
	if balance != 500 {
		t.Errorf("el balance NO debería tocarse mientras la entrega sigue incierta (no se puede confirmar que no se entregó) — esperado 500, obtuve %d", balance)
	}
}

// TestFailOrderAndRefund_SinAmbiguedadReembolsaNormal confirma que el
// camino normal (sin ningún intento de envío ambiguo) sigue funcionando —
// el pedido falla y el KC se reembolsa de verdad.
func TestFailOrderAndRefund_SinAmbiguedadReembolsaNormal(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 500)
	defer cleanup()
	order := newShopTestOrder(t, conn, custID, 100)
	// Descontar el KC como haría DeductKCAndCreateOrder al crear el pedido
	// de verdad — failOrderAndRefund asume que el precio ya se descontó.
	if _, err := conn.Exec(`UPDATE customers SET kc_balance=kc_balance-100 WHERE id=$1`, custID); err != nil {
		t.Fatalf("no se pudo descontar el balance inicial: %v", err)
	}

	failOrderAndRefund(conn, order, "motivo interno de prueba", "motivo para el cliente")

	var status string
	conn.QueryRow(`SELECT status FROM orders WHERE id=$1`, order.ID).Scan(&status)
	if status != "refunded" {
		t.Errorf("sin ninguna ambigüedad pendiente, el pedido debería quedar 'refunded', quedó en %q", status)
	}
	var balance int
	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, custID).Scan(&balance)
	if balance != 500 {
		t.Errorf("el balance debería restaurarse a 500 tras el reembolso, obtuve %d", balance)
	}
}

// TestFailOrderAndRefund_PedidoIlocalizableNoAfirmaExitoNiTocaBalance cubre
// el espíritu del punto 6 desde el otro extremo: cuando failOrderAndRefund
// ni siquiera puede localizar el pedido al momento de actuar (fila borrada
// por una condición de carrera, réplica atrasada, etc.), debe fallar de
// forma segura — sin escribir ningún registro que afirme un reembolso
// exitoso, y sin tocar el balance del cliente. Esto es justo lo que
// requiere failOrderAndRefund por diseño: si MarkOrderSendAttempted no
// puede leer el pedido, se corta ahí y se reintenta en el próximo ciclo, en
// vez de seguir adelante con datos que ya no existen.
func TestFailOrderAndRefund_PedidoIlocalizableNoAfirmaExitoNiTocaBalance(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 500)
	defer cleanup()
	order := newShopTestOrder(t, conn, custID, 100)

	// Simula una condición de carrera: el pedido desaparece justo antes de
	// que failOrderAndRefund intente actuar sobre él.
	if _, err := conn.Exec(`DELETE FROM orders WHERE id=$1`, order.ID); err != nil {
		t.Fatalf("no se pudo borrar el pedido para forzar el escenario: %v", err)
	}

	failOrderAndRefund(conn, order, "motivo interno de prueba", "motivo para el cliente")

	var count int
	conn.QueryRow(`
		SELECT count(*) FROM audit_logs
		WHERE customer_id=$1 AND action IN ('ORDER_FAILED','ORDER_NEEDS_REVIEW')
		AND details LIKE '%reembols%'`, custID).Scan(&count)
	if count != 0 {
		t.Errorf("no debería quedar ningún registro afirmando un reembolso (exitoso o pendiente) para un pedido que no se pudo localizar, hay %d", count)
	}

	var balance int
	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, custID).Scan(&balance)
	if balance != 500 {
		t.Errorf("el balance no debería cambiar cuando el pedido no se puede localizar, obtuve %d", balance)
	}
}

// ==================== REEMBOLSO IDEMPOTENTE (punto 3) ====================

// TestFailOrderAndRefund_LlamadaRepetidaNoDuplicaElReembolso cubre
// "reembolso repetido... después de un reinicio": failOrderAndRefund se
// llama dos veces seguidas para el MISMO pedido (simulando que el proceso
// se reinició justo después del primer reembolso y algo lo vuelve a
// procesar) — el KC solo debe devolverse una vez.
func TestFailOrderAndRefund_LlamadaRepetidaNoDuplicaElReembolso(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 500)
	defer cleanup()
	order := newShopTestOrder(t, conn, custID, 100)
	if _, err := conn.Exec(`UPDATE customers SET kc_balance=kc_balance-100 WHERE id=$1`, custID); err != nil {
		t.Fatalf("no se pudo descontar el balance inicial: %v", err)
	}

	failOrderAndRefund(conn, order, "motivo de prueba", "motivo para el cliente")
	failOrderAndRefund(conn, order, "motivo de prueba (reintento)", "motivo para el cliente")

	var balance int
	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, custID).Scan(&balance)
	if balance != 500 {
		t.Errorf("dos llamadas a failOrderAndRefund para el mismo pedido NUNCA deben reembolsar dos veces — esperaba 500, obtuve %d", balance)
	}

	var status string
	conn.QueryRow(`SELECT status FROM orders WHERE id=$1`, order.ID).Scan(&status)
	if status != "refunded" {
		t.Errorf("el pedido debería quedar 'refunded', quedó en %q", status)
	}

	// AddAuditLog es fire-and-forget (corre en su propia goroutine, para no
	// bloquear el camino principal) — se espera un poco a que el INSERT
	// asíncrono termine en vez de asumir que ya corrió apenas failOrderAndRefund
	// retorna.
	refundedLogs := countAuditLogsEventually(t, conn, custID, "ORDER_FAILED", "%KC reembolsados%")
	if refundedLogs != 1 {
		t.Errorf("debería haber exactamente 1 registro de auditoría confirmando el reembolso, hay %d", refundedLogs)
	}
}

// countAuditLogsEventually espera hasta 2s a que aparezca al menos un
// registro de auditoría coincidente — AddAuditLog inserta en su propia
// goroutine (fire-and-forget), así que consultar audit_logs inmediatamente
// después de una llamada síncrona puede correr una carrera contra ese
// INSERT asíncrono.
func countAuditLogsEventually(t *testing.T, conn *sql.DB, custID uuid.UUID, action, detailsLike string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var count int
	for {
		conn.QueryRow(`SELECT count(*) FROM audit_logs WHERE customer_id=$1 AND action=$2 AND details LIKE $3`, custID, action, detailsLike).Scan(&count)
		if count > 0 || time.Now().After(deadline) {
			return count
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestFailOrderAndRefund_LlamadasConcurrentesNoDuplicanElReembolso cubre
// "reembolso... concurrente": dos goroutines llaman a failOrderAndRefund
// para el MISMO pedido al mismo tiempo — el row lock de RefundOrder (FOR
// UPDATE) y la transición condicional de MarkOrderFailedIfNotTerminal
// deben serializar esto de forma que el KC solo se devuelva una vez.
func TestFailOrderAndRefund_LlamadasConcurrentesNoDuplicanElReembolso(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 500)
	defer cleanup()
	order := newShopTestOrder(t, conn, custID, 100)
	if _, err := conn.Exec(`UPDATE customers SET kc_balance=kc_balance-100 WHERE id=$1`, custID); err != nil {
		t.Fatalf("no se pudo descontar el balance inicial: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			failOrderAndRefund(conn, order, "motivo de prueba concurrente", "motivo para el cliente")
		}()
	}
	wg.Wait()

	var balance int
	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, custID).Scan(&balance)
	if balance != 500 {
		t.Errorf("5 llamadas concurrentes a failOrderAndRefund para el mismo pedido deben reembolsar UNA sola vez — esperaba 500, obtuve %d", balance)
	}
}

// ==================== RECUPERACIÓN DE NOWPAYMENTS SIMULADA (punto 4) ====================

// TestProcessNOWPaymentsPaymentID_FalloAlGuardarPaymentIDQuedaComoError
// cubre "fallo al guardar un webhook y su identificador de pago": si
// SetProviderPaymentID falla (acá, forzado con un *sql.DB ya cerrado), el
// outcome debe empezar con "error:" — nunca "ignored" — para que
// RetryFailedWebhookEvents lo vuelva a intentar más tarde. Usa un servidor
// simulado (httptest) en vez de la API real de NOWPayments.
func TestProcessNOWPaymentsPaymentID_FalloAlGuardarPaymentIDQuedaComoError(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()

	txID := uuid.New()
	if _, err := conn.Exec(`
		INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, created_at, updated_at)
		VALUES ($1,$2,'nowpayments','kc_recharge','starter','Starter',10.40,2.80,800,'invoice-test','pending',NOW(),NOW())`,
		txID, custID); err != nil {
		t.Fatalf("insert payment_transactions: %v", err)
	}
	defer conn.Exec(`DELETE FROM payment_transactions WHERE id=$1`, txID)

	prevURL := nowPaymentsBaseURL
	prevCfg := paymentCfg
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"payment_status":"waiting","order_id":%q}`, txID.String())
	}))
	defer server.Close()
	nowPaymentsBaseURL = server.URL
	paymentCfg.NOWPaymentsAPIKey = "test-key"
	defer func() { nowPaymentsBaseURL = prevURL; paymentCfg = prevCfg }()

	// *sql.DB ya cerrado — CUALQUIER operación (incluido SetProviderPaymentID)
	// falla de forma determinística con sql.ErrConnDone, sin necesitar
	// tocar la conexión real compartida por el resto de las pruebas.
	closedDB, err := sql.Open("postgres", "")
	if err != nil { t.Fatalf("sql.Open: %v", err) }
	closedDB.Close()

	outcome := processNOWPaymentsPaymentID(closedDB, 999999)
	if !strings.HasPrefix(outcome, "error:") {
		t.Errorf("si SetProviderPaymentID falla, el outcome debe empezar con \"error:\" (para que se reintente) — obtuve %q", outcome)
	}
	if strings.Contains(outcome, "ignored") {
		t.Errorf("un fallo al guardar el payment_id NUNCA debe terminar como \"ignored\" (eso lo excluiría de los reintentos) — obtuve %q", outcome)
	}
}

// TestProcessNOWPaymentsPaymentID_PendienteNoEsError confirma la otra
// mitad de la distinción pedida: un pago genuinamente pendiente (la
// pasarela simulada dice "waiting", y SetProviderPaymentID SÍ se guarda
// bien) no debe tratarse como un fallo interno ni reintentarse como
// webhook — es un estado normal y esperado mientras se confirma.
func TestProcessNOWPaymentsPaymentID_PendienteNoEsError(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()

	txID := uuid.New()
	if _, err := conn.Exec(`
		INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, created_at, updated_at)
		VALUES ($1,$2,'nowpayments','kc_recharge','starter','Starter',10.40,2.80,800,'invoice-test2','pending',NOW(),NOW())`,
		txID, custID); err != nil {
		t.Fatalf("insert payment_transactions: %v", err)
	}
	defer conn.Exec(`DELETE FROM payment_transactions WHERE id=$1`, txID)

	prevURL := nowPaymentsBaseURL
	prevCfg := paymentCfg
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"payment_status":"waiting","order_id":%q}`, txID.String())
	}))
	defer server.Close()
	nowPaymentsBaseURL = server.URL
	paymentCfg.NOWPaymentsAPIKey = "test-key"
	defer func() { nowPaymentsBaseURL = prevURL; paymentCfg = prevCfg }()

	outcome := processNOWPaymentsPaymentID(conn, 123123)
	if strings.HasPrefix(outcome, "error:") {
		t.Errorf("un pago genuinamente pendiente (provider_payment_id sí se guardó) no debe reportarse como error, obtuve %q", outcome)
	}

	var providerID sql.NullString
	conn.QueryRow(`SELECT provider_payment_id FROM payment_transactions WHERE id=$1`, txID).Scan(&providerID)
	if !providerID.Valid || providerID.String != "123123" {
		t.Errorf("provider_payment_id debería haberse guardado (123123), obtuve %v", providerID)
	}
}

// TestProcessNOWPaymentsPaymentID_ConfirmadoAcreditaUnaVez cubre "conserva
// la acreditación exactamente una vez": con la pasarela simulada diciendo
// "finished", el pago debe acreditarse — y una segunda llamada (IPN
// duplicado o reintento) no debe volver a sumar KC.
func TestProcessNOWPaymentsPaymentID_ConfirmadoAcreditaUnaVez(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()

	txID := uuid.New()
	if _, err := conn.Exec(`
		INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, created_at, updated_at)
		VALUES ($1,$2,'nowpayments','kc_recharge','starter','Starter',10.40,2.80,800,'invoice-test3','pending',NOW(),NOW())`,
		txID, custID); err != nil {
		t.Fatalf("insert payment_transactions: %v", err)
	}
	defer conn.Exec(`DELETE FROM payment_transactions WHERE id=$1`, txID)

	prevURL := nowPaymentsBaseURL
	prevCfg := paymentCfg
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"payment_status":"finished","order_id":%q}`, txID.String())
	}))
	defer server.Close()
	nowPaymentsBaseURL = server.URL
	paymentCfg.NOWPaymentsAPIKey = "test-key"
	defer func() { nowPaymentsBaseURL = prevURL; paymentCfg = prevCfg }()

	outcome1 := processNOWPaymentsPaymentID(conn, 555555)
	if outcome1 != "processed" {
		t.Fatalf("primer intento: esperaba \"processed\", obtuve %q", outcome1)
	}
	var balance int
	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, custID).Scan(&balance)
	if balance != 800 {
		t.Fatalf("esperaba 800 KC acreditados, obtuve %d", balance)
	}

	// IPN duplicado / reintento — no debe volver a acreditar.
	processNOWPaymentsPaymentID(conn, 555555)
	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, custID).Scan(&balance)
	if balance != 800 {
		t.Errorf("una notificación repetida NUNCA debe acreditar KC dos veces — esperaba seguir en 800, obtuve %d", balance)
	}
}

// ==================== COMPROBANTES INTERNACIONALES ANTIGUOS (punto 6) ====================

// TestComprobanteDeRecargaVinculada_UsaImporteYDivisaReales cubre
// "comprobantes antiguos de pagos internacionales": una fila de
// kc_recharges vinculada a un pago por dLocal Go en una divisa distinta a
// soles debe resolver, siguiendo exactamente la misma composición que usa
// HandlerRechargeVoucher (GetKCRechargeByID → GetPaymentTransaction →
// ChargedAmountAndCurrency), al monto y la divisa REALES cobrados — nunca
// "S/", que sería directamente incorrecto para una pasarela que no cobra
// en soles. El enlace sigue siendo /comprobantes/recarga/:id (compatible
// con cualquier enlace viejo), pero el contenido refleja el pago real.
func TestComprobanteDeRecargaVinculada_UsaImporteYDivisaReales(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()

	txID := uuid.New()
	if _, err := conn.Exec(`
		INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, currency_code, amount_local, kc_amount, external_id, status, created_at, updated_at)
		VALUES ($1,$2,'dlocalgo','kc_recharge','starter','Starter Internacional',10.40,2.80,'MXN',55.30,800,'ext-intl','approved',NOW()-INTERVAL '90 days',NOW()-INTERVAL '90 days')`,
		txID, custID); err != nil {
		t.Fatalf("insert payment_transactions: %v", err)
	}
	defer conn.Exec(`DELETE FROM payment_transactions WHERE id=$1`, txID)

	rechargeID := uuid.New()
	if _, err := conn.Exec(`
		INSERT INTO kc_recharges (id, customer_id, amount_kc, amount_soles, method, note, approved_by, payment_transaction_id, created_at)
		VALUES ($1,$2,800,10.40,'dlocalgo','Pago automático','dlocalgo',$3,NOW()-INTERVAL '90 days')`,
		rechargeID, custID, txID); err != nil {
		t.Fatalf("insert kc_recharges: %v", err)
	}
	defer conn.Exec(`DELETE FROM kc_recharges WHERE id=$1`, rechargeID)

	// Misma composición que HandlerRechargeVoucher: resolver la recarga por
	// ID (como llegaría un enlace viejo a /comprobantes/recarga/:id),
	// reconocer que está vinculada a un pago, y usar el importe/divisa
	// reales de ESE pago.
	r, err := db.GetKCRechargeByID(conn, rechargeID)
	if err != nil {
		t.Fatalf("GetKCRechargeByID: %v", err)
	}
	if r.PaymentTransactionID == nil {
		t.Fatal("la recarga debería estar vinculada a un pago")
	}
	tx, err := db.GetPaymentTransaction(conn, *r.PaymentTransactionID)
	if err != nil {
		t.Fatalf("GetPaymentTransaction: %v", err)
	}
	chargedAmount, chargedCurrency := ChargedAmountAndCurrency(tx.Gateway, tx.AmountPEN, tx.AmountUSD, tx.AmountLocal, tx.CurrencyCode)
	if chargedCurrency != "MXN" {
		t.Errorf("la divisa real cobrada era MXN, obtuve %q — nunca debería inventarse ni asumirse PEN", chargedCurrency)
	}
	if chargedAmount != 55.30 {
		t.Errorf("el importe real cobrado era 55.30 MXN, obtuve %v", chargedAmount)
	}
}

// TestHandlerRechargeVoucher_PagoVinculadoIlegibleNoInventaMonedaPEN cubre
// "comprobante cuando falla la consulta del pago vinculado" (punto 3): si
// r.PaymentTransactionID apunta a un pago que ya no se puede leer,
// HandlerRechargeVoucher NUNCA debe caer al bloque de "recarga manual" (que
// usaría amount_soles — el equivalente en PEN de referencia guardado por
// CreditPaymentOnce — como si fuera la divisa realmente cobrada). Debe
// responder con éxito pero dejando claro que el importe/divisa no están
// disponibles, sin inventar "PEN".
func TestHandlerRechargeVoucher_PagoVinculadoIlegibleNoInventaMonedaPEN(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()

	// payment_transaction_id apunta a un UUID que nunca existió — simula
	// tanto una fila borrada como cualquier error de lectura: en ambos casos
	// GetPaymentTransaction falla. kc_recharges.payment_transaction_id tiene
	// una FK real hacia payment_transactions(id), así que insertar esto
	// directamente violaría la integridad referencial — se desactivan los
	// triggers de FK solo para este INSERT, en UNA conexión reservada
	// (para que el SET quede en la misma conexión física que el INSERT), y
	// se reactivan enseguida.
	ctx := context.Background()
	sqlConn, err := conn.Conn(ctx)
	if err != nil {
		t.Fatalf("conn.Conn: %v", err)
	}
	defer sqlConn.Close()

	missingPaymentID := uuid.New()
	rechargeID := uuid.New()
	if _, err := sqlConn.ExecContext(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatalf("SET session_replication_role: %v", err)
	}
	_, insertErr := sqlConn.ExecContext(ctx, `
		INSERT INTO kc_recharges (id, customer_id, amount_kc, amount_soles, method, note, payment_transaction_id, created_at)
		VALUES ($1,$2,800,10.40,'mercadopago','Pago automático',$3,NOW())`,
		rechargeID, custID, missingPaymentID)
	if _, err := sqlConn.ExecContext(ctx, `SET session_replication_role = DEFAULT`); err != nil {
		t.Fatalf("SET session_replication_role DEFAULT: %v", err)
	}
	if insertErr != nil {
		t.Fatalf("insert kc_recharges: %v", insertErr)
	}
	defer conn.Exec(`DELETE FROM kc_recharges WHERE id=$1`, rechargeID)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/v/:id", func(c *gin.Context) {
		c.Set("customer_id", custID.String())
		HandlerRechargeVoucher(conn)(c)
	})
	req := httptest.NewRequest("GET", "/v/"+rechargeID.String(), nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("esperaba 200 (comprobante accesible aunque el pago vinculado no se pueda leer), obtuve %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Success bool `json:"success"`
		Voucher struct {
			ChargedAmount     *float64 `json:"charged_amount"`
			ChargedCurrency   *string  `json:"charged_currency"`
			AmountPEN         *float64 `json:"amount_pen"`
			AmountUnavailable bool     `json:"amount_unavailable"`
			KCAmount          int      `json:"kc_amount"`
		} `json:"voucher"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("respuesta no es el JSON esperado: %v (body: %s)", err, w.Body.String())
	}
	if !resp.Success {
		t.Fatalf("esperaba success=true, body: %s", w.Body.String())
	}
	if !resp.Voucher.AmountUnavailable {
		t.Errorf("el comprobante debería marcar explícitamente que el importe/divisa no están disponibles")
	}
	if resp.Voucher.ChargedCurrency != nil {
		t.Errorf("charged_currency NUNCA debe inventarse como PEN cuando no se pudo leer el pago vinculado, obtuve %v", *resp.Voucher.ChargedCurrency)
	}
	if resp.Voucher.ChargedAmount != nil {
		t.Errorf("charged_amount NUNCA debe inventarse (era el equivalente en PEN de referencia, no lo realmente cobrado), obtuve %v", *resp.Voucher.ChargedAmount)
	}
	if resp.Voucher.KCAmount != 800 {
		t.Errorf("el KC acreditado sí debería seguir mostrándose (es un dato propio, confiable, de kc_recharges), esperaba 800 obtuve %d", resp.Voucher.KCAmount)
	}
}

// ==================== RECUPERACIÓN AUTOMÁTICA DE PAGOS (punto 1) ====================

// TestReconcilePendingPayments_AcreditacionFallidaRotaYPermiteProcesarUnPagoPosterior
// cubre el escenario completo pedido: 100 pagos ya confirmados por la
// pasarela cuya acreditación de KC falla (acá, simulando una cuenta
// desactivada), más un pago posterior legítimo. Antes de la corrección,
// reconcileOnePayment no tocaba updated_at cuando processApprovedPayment
// fallaba, así que esos 100 pagos SIEMPRE volvían a ser los primeros 100 de
// GetStalePendingPayments (ORDER BY updated_at ASC LIMIT 100) y el pago
// #101 nunca llegaba a procesarse. Usa un servidor NOWPayments simulado
// (httptest) — nunca la pasarela real.
func TestReconcilePendingPayments_AcreditacionFallidaRotaYPermiteProcesarUnPagoPosterior(t *testing.T) {
	conn := setupShopTestDB(t)

	// Cliente inactivo: CreditPaymentOnce falla al acreditar KC porque su
	// UPDATE lleva "AND is_active=true" — 0 filas afectadas, error, rollback.
	inactiveCustID := uuid.New()
	suffix := inactiveCustID.String()[:8]
	if _, err := conn.Exec(`
		INSERT INTO customers (id, epic_username, email, password_hash, kc_balance, is_verified, is_active, is_admin, has_password, created_at, updated_at)
		VALUES ($1,$2,$3,'x',0,true,false,false,true,NOW(),NOW())`,
		inactiveCustID, "inactive_"+suffix, "inactive-"+suffix+"@example.com"); err != nil {
		t.Fatalf("insert cliente inactivo: %v", err)
	}
	defer conn.Exec(`DELETE FROM customers WHERE id=$1`, inactiveCustID)

	const numStuck = 100
	orderIDByProviderPaymentID := map[int64]uuid.UUID{}
	stuckTxIDs := make([]uuid.UUID, numStuck)
	for i := 1; i <= numStuck; i++ {
		txID := uuid.New()
		stuckTxIDs[i-1] = txID
		orderIDByProviderPaymentID[int64(i)] = txID
		if _, err := conn.Exec(`
			INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, provider_payment_id, status, created_at, updated_at)
			VALUES ($1,$2,'nowpayments','kc_recharge','starter','Starter',10.40,2.80,800,$3,$4,'pending',NOW() - INTERVAL '10 minutes', NOW() - INTERVAL '10 minutes')`,
			txID, inactiveCustID, fmt.Sprintf("invoice-stuck-%d", i), strconv.Itoa(i)); err != nil {
			t.Fatalf("insert stuck payment[%d]: %v", i, err)
		}
	}
	defer func() {
		for _, id := range stuckTxIDs { conn.Exec(`DELETE FROM payment_transactions WHERE id=$1`, id) }
	}()

	// Pago posterior legítimo, de un cliente ACTIVO — más nuevo en
	// updated_at que los 100 anteriores, así el primer barrido (LIMIT 100)
	// no llega a verlo todavía.
	activeCustID, cleanupActive := newShopTestCustomer(t, conn, 0)
	defer cleanupActive()
	const newPaymentProviderID = int64(99999)
	newTxID := uuid.New()
	orderIDByProviderPaymentID[newPaymentProviderID] = newTxID
	if _, err := conn.Exec(`
		INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, provider_payment_id, status, created_at, updated_at)
		VALUES ($1,$2,'nowpayments','kc_recharge','starter','Starter',10.40,2.80,800,'invoice-new',$3,'pending',NOW() - INTERVAL '5 minutes', NOW() - INTERVAL '5 minutes')`,
		newTxID, activeCustID, strconv.FormatInt(newPaymentProviderID, 10)); err != nil {
		t.Fatalf("insert pago posterior: %v", err)
	}
	defer conn.Exec(`DELETE FROM payment_transactions WHERE id=$1`, newTxID)

	prevURL := nowPaymentsBaseURL
	prevCfg := paymentCfg
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idStr := strings.TrimPrefix(r.URL.Path, "/payment/")
		id, _ := strconv.ParseInt(idStr, 10, 64)
		orderID, ok := orderIDByProviderPaymentID[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"payment_status":"confirmed","order_id":%q}`, orderID.String())
	}))
	defer server.Close()
	nowPaymentsBaseURL = server.URL
	paymentCfg.NOWPaymentsAPIKey = "test-key"
	defer func() { nowPaymentsBaseURL = prevURL; paymentCfg = prevCfg }()

	// ── Primer barrido: los 100 pagos "atascados" se intentan, todos fallan
	// al acreditar (cuenta inactiva) — el pago posterior NO debería tocarse
	// todavía (sigue siendo el más nuevo de los elegibles).
	ReconcilePendingPayments(conn)

	var activeBalanceAfterFirst int
	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, activeCustID).Scan(&activeBalanceAfterFirst)
	if activeBalanceAfterFirst != 0 {
		t.Fatalf("el pago posterior no debería haberse procesado todavía en el primer barrido, balance esperado 0, obtuve %d", activeBalanceAfterFirst)
	}
	var stuckStatus string
	conn.QueryRow(`SELECT status FROM payment_transactions WHERE id=$1`, stuckTxIDs[0]).Scan(&stuckStatus)
	if stuckStatus != "pending" {
		t.Errorf("un pago con acreditación fallida debe seguir 'pending' (para poder reintentarse), no %q — nunca debe pasar a 'failed'/'review' solo porque la acreditación falló", stuckStatus)
	}

	// ── Segundo barrido: como el fix rota updated_at incluso cuando la
	// acreditación falla, los 100 atascados ya no acaparan los primeros 100
	// resultados — el pago posterior debe llegar a procesarse ahora.
	ReconcilePendingPayments(conn)

	var activeBalanceAfterSecond int
	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, activeCustID).Scan(&activeBalanceAfterSecond)
	if activeBalanceAfterSecond != 800 {
		t.Errorf("el pago posterior debería haberse acreditado en el segundo barrido (los 100 atascados ya no deberían bloquear la cola) — esperaba 800, obtuve %d", activeBalanceAfterSecond)
	}
	var newPaymentStatus string
	conn.QueryRow(`SELECT status FROM payment_transactions WHERE id=$1`, newTxID).Scan(&newPaymentStatus)
	if newPaymentStatus != "approved" {
		t.Errorf("el pago posterior debería quedar 'approved' tras acreditarse, obtuve %q", newPaymentStatus)
	}
}
