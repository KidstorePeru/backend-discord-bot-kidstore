package db

// Pruebas de regresión para las correcciones del audit de 25 puntos —
// cuentas, 2FA, pagos duplicados, fallos de acreditación, reinicios,
// devoluciones y paginación. Son pruebas de integración (no puramente
// unitarias): este proyecto no tiene una capa de DB simulada, así que
// necesitan una Postgres real. Cada prueba crea su propio cliente de
// prueba con un epic_username/email único (sufijo aleatorio) y lo borra al
// terminar.
//
// IMPORTANTE — aislamiento de la base de datos: estas pruebas usan
// EXCLUSIVAMENTE las variables TEST_DB_* (cargadas desde .env.test, nunca
// desde .env) — jamás caen de vuelta a las DB_* que usa la app en
// producción, ni siquiera si TEST_DB_* no está configurado. Antes esto sí
// cargaba .env (las credenciales reales de producción) y, si DB_HOST
// estaba definido ahí (como está siempre en este proyecto), las pruebas
// corrían contra la base de PRODUCCIÓN. Eso era especialmente peligroso
// con TestClaimPendingOrders_RecuperaPedidosAtascados: ClaimPendingOrders
// opera sobre TODA la tabla orders (no solo las filas que inserta la
// prueba) — correrla contra producción reclamaría y marcaría 'processing'
// pedidos reales de clientes reales, interfiriendo con el worker real.
//
// setupTestDB además rechaza explícitamente (t.Fatal, no un simple skip)
// si TEST_DB_HOST/TEST_DB_NAME resultan iguales a DB_HOST/DB_NAME — por si
// alguien configura TEST_DB_* apuntando por error a la misma base.
//
// Ver .env.test.example para las variables necesarias si querés apuntar a
// una base de pruebas propia. NO hace falta configurar nada para correr
// estas pruebas normalmente: TestMain (testmain_test.go) arranca solo una
// Postgres real pero embebida y descartable (ver src/testdb) cuando
// TEST_DB_HOST no está seteado — eso es lo que deja TEST_DB_HOST
// configurado antes de que cualquier prueba llegue a este setupTestDB. Si
// por lo que sea ni eso ni una base externa están disponibles, las pruebas
// se saltan en vez de fallar — no tiene sentido que "no hay una base de
// pruebas" se reporte como "la prueba falló", y mucho menos que eso empuje
// a alguien a apuntarlas a la base real "para que pasen".

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

var testDB *sql.DB

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	if testDB != nil {
		return testDB
	}
	// `go test ./src/db/...` corre con el directorio de trabajo puesto en
	// el paquete (src/db), no en la raíz del proyecto donde viven los
	// archivos .env — se intenta cargar .env.test desde ambos lugares.
	// Deliberadamente NUNCA se carga .env (las credenciales reales de la
	// app) ni se cae al entorno del proceso para las variables DB_* —
	// si alguien ya las exportó para correr la app, eso no debe colar acá.
	godotenv.Load(".env.test")
	godotenv.Load("../../.env.test")

	host := os.Getenv("TEST_DB_HOST")
	if host == "" {
		t.Skip("TEST_DB_HOST no configurado — se salta esta prueba de integración (necesita una base de datos de PRUEBA separada, nunca la de producción; ver .env.test.example)")
	}
	dbName := os.Getenv("TEST_DB_NAME")

	// Barrera de seguridad adicional: si TEST_DB_* apunta exactamente a la
	// misma base que usa la app real (DB_HOST/DB_NAME), abortar en vez de
	// arriesgarse — sin esto, una configuración accidental (ej. copiar
	// .env a .env.test sin cambiar nada) volvería a correr las pruebas
	// contra producción, exactamente el problema que este archivo entero
	// existe para evitar.
	if host == os.Getenv("DB_HOST") && dbName != "" && dbName == os.Getenv("DB_NAME") {
		t.Fatal("TEST_DB_HOST/TEST_DB_NAME apuntan a la MISMA base que DB_HOST/DB_NAME (producción) — configura una base de datos de pruebas separada, nunca reutilices la real")
	}

	port := os.Getenv("TEST_DB_PORT")
	if port == "" {
		port = "5432"
	}
	// sslmode por defecto "disable" — a diferencia de la app real (que
	// siempre exige "require"), una base de pruebas es local o embebida
	// (ver src/testdb) y no tiene SSL configurado. TEST_DB_SSLMODE permite
	// pedir "require" explícitamente si algún día la base de pruebas real
	// provista externamente (ej. en CI) sí lo necesita.
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
	// La base de pruebas puede estar completamente vacía (un contenedor
	// Postgres recién creado, por ejemplo) — se asegura el esquema antes de
	// que cualquier prueba intente insertar filas. Es seguro llamarlo
	// tantas veces como haga falta (todo el esquema usa CREATE TABLE IF
	// NOT EXISTS / DO $$ ... IF NOT EXISTS).
	if err := CreateTables(conn); err != nil {
		t.Fatalf("no se pudo preparar el esquema en la base de pruebas: %v", err)
	}
	testDB = conn
	return testDB
}

// newTestCustomer inserta un cliente de prueba mínimo y devuelve su ID +
// una función de limpieza que hay que llamar con defer.
func newTestCustomer(t *testing.T, conn *sql.DB, opts ...func(*testCustomerOpts)) (uuid.UUID, func()) {
	t.Helper()
	o := testCustomerOpts{kcBalance: 0, isAdmin: false}
	for _, apply := range opts {
		apply(&o)
	}
	id := uuid.New()
	suffix := id.String()[:8]
	epic := "regtest_" + suffix
	email := "regtest-" + suffix + "@example.com"
	_, err := conn.Exec(`
		INSERT INTO customers (id, epic_username, email, password_hash, kc_balance, is_verified, is_active, is_admin, has_password, created_at, updated_at)
		VALUES ($1,$2,$3,'x',$4,true,true,$5,true,NOW(),NOW())`,
		id, epic, email, o.kcBalance, o.isAdmin)
	if err != nil {
		t.Fatalf("no se pudo crear el cliente de prueba: %v", err)
	}
	cleanup := func() {
		conn.Exec(`DELETE FROM kc_recharges WHERE customer_id=$1`, id)
		conn.Exec(`DELETE FROM payment_transactions WHERE customer_id=$1`, id)
		conn.Exec(`DELETE FROM orders WHERE customer_id=$1`, id)
		conn.Exec(`DELETE FROM customers WHERE id=$1`, id)
	}
	return id, cleanup
}

type testCustomerOpts struct {
	kcBalance int
	isAdmin   bool
}

func withKCBalance(n int) func(*testCustomerOpts) {
	return func(o *testCustomerOpts) { o.kcBalance = n }
}

// ==================== PAGOS DUPLICADOS / FALLOS DE ACREDITACIÓN (puntos 5, 10) ====================

func TestCreditPaymentOnce_NuncaAcreditaDosVeces(t *testing.T) {
	conn := setupTestDB(t)
	custID, cleanup := newTestCustomer(t, conn)
	defer cleanup()

	txID := uuid.New()
	_, err := conn.Exec(`
		INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, created_at, updated_at)
		VALUES ($1,$2,'mercadopago','kc_recharge','starter','Starter',10.40,2.80,800,'ext-test','pending',NOW(),NOW())`,
		txID, custID)
	if err != nil {
		t.Fatalf("insert payment_transactions: %v", err)
	}

	credited, ptx, err := CreditPaymentOnce(conn, txID)
	if err != nil {
		t.Fatalf("primera llamada: error inesperado: %v", err)
	}
	if !credited {
		t.Fatal("primera llamada: esperaba credited=true")
	}
	if ptx.Status != "approved" {
		t.Errorf("status esperado 'approved', obtuve %q", ptx.Status)
	}

	var balance int
	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, custID).Scan(&balance)
	if balance != 800 {
		t.Fatalf("balance esperado 800, obtuve %d", balance)
	}

	// Webhook duplicado — no debe volver a acreditar.
	credited2, _, err := CreditPaymentOnce(conn, txID)
	if err != nil {
		t.Fatalf("segunda llamada: error inesperado: %v", err)
	}
	if credited2 {
		t.Error("segunda llamada (duplicada) no debería acreditar de nuevo")
	}

	// PUNTO 10: aunque alguien cambie el status manualmente (ej. desde el
	// panel admin) a 'failed' y se vuelva a "aprobar", la unicidad depende
	// de kc_credited_at, no del status — no debe duplicar el KC.
	conn.Exec(`UPDATE payment_transactions SET status='failed' WHERE id=$1`, txID)
	credited3, ptx3, err := CreditPaymentOnce(conn, txID)
	if err != nil {
		t.Fatalf("tercera llamada (tras status manual a failed): error inesperado: %v", err)
	}
	if credited3 {
		t.Error("no debería volver a acreditar solo porque el status visible cambió")
	}
	if ptx3.Status != "approved" {
		t.Errorf("el status debería emparejarse de vuelta a 'approved', obtuve %q", ptx3.Status)
	}

	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, custID).Scan(&balance)
	if balance != 800 {
		t.Fatalf("el balance no debería haber cambiado (sigue en 800), obtuve %d", balance)
	}

	var rechargeCount int
	conn.QueryRow(`SELECT count(*) FROM kc_recharges WHERE payment_transaction_id=$1`, txID).Scan(&rechargeCount)
	if rechargeCount != 1 {
		t.Errorf("esperaba exactamente 1 fila en kc_recharges para este pago, hay %d", rechargeCount)
	}
}

func TestCreditPaymentOnce_ConcurrenciaAcreditaUnaSolaVez(t *testing.T) {
	conn := setupTestDB(t)
	custID, cleanup := newTestCustomer(t, conn)
	defer cleanup()

	txID := uuid.New()
	_, err := conn.Exec(`
		INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, created_at, updated_at)
		VALUES ($1,$2,'paypal','kc_recharge','gamer','Gamer',31.20,8.40,2400,'ext-test-2','pending',NOW(),NOW())`,
		txID, custID)
	if err != nil {
		t.Fatalf("insert payment_transactions: %v", err)
	}

	const concurrency = 20
	results := make(chan bool, concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			credited, _, err := CreditPaymentOnce(conn, txID)
			if err != nil {
				results <- false
				return
			}
			results <- credited
		}()
	}
	creditedCount := 0
	for i := 0; i < concurrency; i++ {
		if <-results {
			creditedCount++
		}
	}
	if creditedCount != 1 {
		t.Errorf("con %d llamadas concurrentes, exactamente 1 debería acreditar — acreditaron %d", concurrency, creditedCount)
	}

	var balance int
	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, custID).Scan(&balance)
	if balance != 2400 {
		t.Errorf("balance esperado 2400 tras la carrera, obtuve %d", balance)
	}
}

// ==================== REINICIOS: PEDIDOS ATASCADOS (punto 8) ====================

func TestClaimPendingOrders_RecuperaPedidosAtascados(t *testing.T) {
	conn := setupTestDB(t)
	custID, cleanup := newTestCustomer(t, conn)
	defer cleanup()

	stuckOrder := uuid.New()
	recentOrder := uuid.New()
	pendingOrder := uuid.New()

	_, err := conn.Exec(`
		INSERT INTO orders (id, customer_id, epic_username, item_offer_id, item_name, price_kc, price_vbucks, status, created_at, updated_at)
		VALUES
		($1,$4,'recv','offer-1','Atascado',100,100,'processing',NOW()-INTERVAL '25 minutes',NOW()-INTERVAL '20 minutes'),
		($2,$4,'recv','offer-2','Reciente',100,100,'processing',NOW()-INTERVAL '2 minutes',NOW()-INTERVAL '2 minutes'),
		($3,$4,'recv','offer-3','Pendiente',100,100,'pending',NOW(),NOW())`,
		stuckOrder, recentOrder, pendingOrder, custID)
	if err != nil {
		t.Fatalf("insert orders: %v", err)
	}

	claimed, err := ClaimPendingOrders(conn)
	if err != nil {
		t.Fatalf("ClaimPendingOrders: %v", err)
	}
	claimedIDs := map[uuid.UUID]bool{}
	for _, o := range claimed {
		claimedIDs[o.ID] = true
	}

	if !claimedIDs[stuckOrder] {
		t.Error("un pedido 'processing' de 20 minutos debería reclamarse (recuperación tras reinicio)")
	}
	if claimedIDs[recentOrder] {
		t.Error("un pedido 'processing' de solo 2 minutos NO debería tocarse (todavía puede estar en curso)")
	}
	if !claimedIDs[pendingOrder] {
		t.Error("un pedido 'pending' normal debería seguir reclamándose (regresión)")
	}

	var recentStatus string
	conn.QueryRow(`SELECT status FROM orders WHERE id=$1`, recentOrder).Scan(&recentStatus)
	if recentStatus != "processing" {
		t.Errorf("el pedido reciente no debería haber cambiado de status, quedó en %q", recentStatus)
	}

	conn.Exec(`DELETE FROM orders WHERE id IN ($1,$2,$3)`, stuckOrder, recentOrder, pendingOrder)
}

// ==================== DEVOLUCIONES (punto 11) ====================

func TestRefundOrder_RecuperaReembolsosPendientesSinDuplicar(t *testing.T) {
	conn := setupTestDB(t)
	custID, cleanup := newTestCustomer(t, conn)
	defer cleanup()

	stuckOrder := uuid.New()
	recentOrder := uuid.New()
	_, err := conn.Exec(`
		INSERT INTO orders (id, customer_id, epic_username, item_offer_id, item_name, price_kc, price_vbucks, status, created_at, updated_at)
		VALUES
		($1,$3,'recv','offer-1','Item Atascado',500,100,'failed',NOW()-INTERVAL '15 minutes',NOW()-INTERVAL '10 minutes'),
		($2,$3,'recv','offer-2','Item Reciente',300,100,'failed',NOW()-INTERVAL '1 minutes',NOW()-INTERVAL '1 minutes')`,
		stuckOrder, recentOrder, custID)
	if err != nil {
		t.Fatalf("insert orders: %v", err)
	}
	defer conn.Exec(`DELETE FROM orders WHERE id IN ($1,$2)`, stuckOrder, recentOrder)

	pending, err := GetOrdersPendingRefund(conn)
	if err != nil {
		t.Fatalf("GetOrdersPendingRefund: %v", err)
	}
	found := map[uuid.UUID]bool{}
	for _, o := range pending {
		found[o.ID] = true
	}
	if !found[stuckOrder] {
		t.Error("un pedido 'failed' de 10 minutos debería aparecer como pendiente de reembolso")
	}
	if found[recentOrder] {
		t.Error("un pedido 'failed' de 1 minuto NO debería aparecer todavía (failOrderAndRefund puede seguir en curso)")
	}

	if err := RefundOrder(conn, stuckOrder); err != nil {
		t.Fatalf("RefundOrder (reintento) falló inesperadamente: %v", err)
	}
	var balance int
	var status string
	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, custID).Scan(&balance)
	conn.QueryRow(`SELECT status FROM orders WHERE id=$1`, stuckOrder).Scan(&status)
	if balance != 500 {
		t.Errorf("balance esperado 500 tras el reembolso, obtuve %d", balance)
	}
	if status != "refunded" {
		t.Errorf("status esperado 'refunded', obtuve %q", status)
	}

	// Un segundo reembolso del mismo pedido debe rechazarse — nunca duplicar.
	if err := RefundOrder(conn, stuckOrder); err == nil {
		t.Error("un segundo RefundOrder sobre un pedido ya 'refunded' debería fallar, no duplicar el balance")
	}
	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, custID).Scan(&balance)
	if balance != 500 {
		t.Errorf("el balance no debería haber cambiado tras el segundo intento, obtuve %d", balance)
	}
}

// ==================== 2FA (punto 4) ====================

func TestTOTP_SecretoActivoNoCambiaHastaConfirmar(t *testing.T) {
	conn := setupTestDB(t)
	custID, cleanup := newTestCustomer(t, conn)
	defer cleanup()

	if err := SetPendingTOTPSecret(conn, custID, "secreto-cifrado-v1"); err != nil {
		t.Fatalf("SetPendingTOTPSecret: %v", err)
	}
	if err := PromotePendingTOTPSecret(conn, custID); err != nil {
		t.Fatalf("PromotePendingTOTPSecret: %v", err)
	}

	var active string
	var pending sql.NullString
	conn.QueryRow(`SELECT totp_secret_enc, totp_pending_secret_enc FROM customers WHERE id=$1`, custID).Scan(&active, &pending)
	if active != "secreto-cifrado-v1" {
		t.Fatalf("el secreto activo debería ser el que se confirmó, obtuve %q", active)
	}
	if pending.Valid {
		t.Error("la columna pendiente debería quedar NULL tras confirmar")
	}

	// Reemplazo: setear un secreto pendiente NUEVO no debe tocar el activo
	// hasta que también se confirme — así protege contra un JWT robado sin
	// contraseña que solo pueda LLAMAR a /2fa/setup pero no confirmar.
	if err := SetPendingTOTPSecret(conn, custID, "secreto-cifrado-v2"); err != nil {
		t.Fatalf("SetPendingTOTPSecret (reemplazo): %v", err)
	}
	conn.QueryRow(`SELECT totp_secret_enc, totp_pending_secret_enc FROM customers WHERE id=$1`, custID).Scan(&active, &pending)
	if active != "secreto-cifrado-v1" {
		t.Errorf("el secreto activo NO debería cambiar solo por generar uno pendiente, quedó %q", active)
	}
	if !pending.Valid || pending.String != "secreto-cifrado-v2" {
		t.Errorf("el secreto pendiente debería ser el nuevo, obtuve %+v", pending)
	}

	if err := PromotePendingTOTPSecret(conn, custID); err != nil {
		t.Fatalf("PromotePendingTOTPSecret (reemplazo): %v", err)
	}
	conn.QueryRow(`SELECT totp_secret_enc, totp_pending_secret_enc FROM customers WHERE id=$1`, custID).Scan(&active, &pending)
	if active != "secreto-cifrado-v2" {
		t.Errorf("tras confirmar el reemplazo, el activo debería ser el nuevo, obtuve %q", active)
	}
	if pending.Valid {
		t.Error("la columna pendiente debería quedar NULL de nuevo tras confirmar el reemplazo")
	}
}

// ==================== CUENTAS: REGISTRO PENDIENTE (punto 2) ====================

func TestUpdatePendingRegistrationToken_ReemplazaTodosLosDatos(t *testing.T) {
	conn := setupTestDB(t)
	email := "regtest-pending-" + uuid.New().String()[:8] + "@example.com"
	defer conn.Exec(`DELETE FROM pending_registrations WHERE email=$1`, email)

	if err := CreatePendingRegistration(conn, "AtacanteUsername", email, "hash-del-atacante", "token-viejo", "es"); err != nil {
		t.Fatalf("CreatePendingRegistration: %v", err)
	}

	// El dueño real del correo se registra de nuevo con SUS propios datos —
	// el intento más reciente debe reemplazar TODO, no solo el token.
	if err := UpdatePendingRegistrationToken(conn, "DuenioReal", email, "hash-del-dueno-real", "token-nuevo", "en"); err != nil {
		t.Fatalf("UpdatePendingRegistrationToken devolvió error inesperado: %v", err)
	}

	pending, err := GetPendingRegistration(conn, "token-nuevo")
	if err != nil {
		t.Fatalf("GetPendingRegistration: %v", err)
	}
	if pending.EpicUsername != "DuenioReal" {
		t.Errorf("epic_username esperado 'DuenioReal', obtuve %q — el atacante seguiría eligiendo la cuenta", pending.EpicUsername)
	}
	if pending.PasswordHash != "hash-del-dueno-real" {
		t.Errorf("password_hash esperado el del dueño real, obtuve %q — el atacante conservaría su contraseña", pending.PasswordHash)
	}
	if pending.Lang != "en" {
		t.Errorf("lang esperado 'en', obtuve %q", pending.Lang)
	}

	// El token viejo ya no debe servir para activar la cuenta con los datos del atacante.
	if _, err := GetPendingRegistration(conn, "token-viejo"); err == nil {
		t.Error("el token viejo no debería seguir siendo válido tras el reemplazo")
	}
}

// ==================== PAGINACIÓN Y BÚSQUEDA (punto 14) ====================

func TestGetAllCustomers_BusquedaYPaginacion(t *testing.T) {
	conn := setupTestDB(t)
	suffix := uuid.New().String()[:8]
	prefix := "pagtest" + suffix
	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		id := uuid.New()
		ids = append(ids, id)
		_, err := conn.Exec(`
			INSERT INTO customers (id, epic_username, email, password_hash, kc_balance, is_verified, is_active, created_at, updated_at)
			VALUES ($1,$2,$3,'x',0,true,true,NOW(),NOW())`,
			id, fmt.Sprintf("%s_%d", prefix, i), fmt.Sprintf("%s-%d@example.com", prefix, i))
		if err != nil {
			t.Fatalf("insert customer %d: %v", i, err)
		}
	}
	defer func() {
		for _, id := range ids {
			conn.Exec(`DELETE FROM customers WHERE id=$1`, id)
		}
	}()

	results, total, err := GetAllCustomers(conn, 1, 50, prefix)
	if err != nil {
		t.Fatalf("GetAllCustomers (búsqueda): %v", err)
	}
	if total != 3 || len(results) != 3 {
		t.Errorf("esperaba encontrar los 3 clientes de prueba por prefijo común, encontré %d (total=%d)", len(results), total)
	}

	// Paginación real: con limit=1, cada página debe traer un cliente DISTINTO.
	seen := map[uuid.UUID]bool{}
	for page := 1; page <= 3; page++ {
		pageResults, _, err := GetAllCustomers(conn, page, 1, prefix)
		if err != nil {
			t.Fatalf("GetAllCustomers (página %d): %v", page, err)
		}
		if len(pageResults) != 1 {
			t.Fatalf("página %d: esperaba 1 resultado, obtuve %d", page, len(pageResults))
		}
		if seen[pageResults[0].ID] {
			t.Errorf("página %d devolvió un cliente repetido — la paginación no está avanzando", page)
		}
		seen[pageResults[0].ID] = true
	}
	if len(seen) != 3 {
		t.Errorf("las 3 páginas deberían haber cubierto los 3 clientes distintos, cubrieron %d", len(seen))
	}
}

func TestGetAllOrders_BusquedaFiltroYPaginacion(t *testing.T) {
	conn := setupTestDB(t)
	custID, cleanup := newTestCustomer(t, conn)
	defer cleanup()

	suffix := uuid.New().String()[:8]
	epic := "ordtest_" + suffix
	statuses := []string{"sent", "sent", "pending", "failed"}
	var orderIDs []uuid.UUID
	for i, st := range statuses {
		id := uuid.New()
		orderIDs = append(orderIDs, id)
		_, err := conn.Exec(`
			INSERT INTO orders (id, customer_id, epic_username, item_offer_id, item_name, price_kc, price_vbucks, status, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,100,50,$6,NOW(),NOW())`,
			id, custID, epic, fmt.Sprintf("offer-%d", i), fmt.Sprintf("Item %d", i), st)
		if err != nil {
			t.Fatalf("insert order %d: %v", i, err)
		}
	}
	defer func() {
		for _, id := range orderIDs {
			conn.Exec(`DELETE FROM orders WHERE id=$1`, id)
		}
	}()

	all, total, err := GetAllOrders(conn, 1, 50, epic, "")
	if err != nil {
		t.Fatalf("GetAllOrders (búsqueda sin filtro): %v", err)
	}
	if total != 4 || len(all) != 4 {
		t.Errorf("esperaba 4 pedidos para este epic_username, obtuve %d (total=%d)", len(all), total)
	}

	sentOnly, sentTotal, err := GetAllOrders(conn, 1, 50, epic, "sent")
	if err != nil {
		t.Fatalf("GetAllOrders (filtro status=sent): %v", err)
	}
	if sentTotal != 2 || len(sentOnly) != 2 {
		t.Errorf("esperaba 2 pedidos 'sent', obtuve %d (total=%d)", len(sentOnly), sentTotal)
	}
}

// ==================== ESTADÍSTICAS (punto 15, relacionado a fallos de acreditación) ====================

func TestGetCustomerRechargeStats_ExcluyePendientesYFallidos(t *testing.T) {
	conn := setupTestDB(t)
	custID, cleanup := newTestCustomer(t, conn)
	defer cleanup()

	payments := []struct {
		amount float64
		status string
	}{
		{10.40, "approved"},
		{31.20, "pending"},
		{58.50, "failed"},
	}
	var payIDs []uuid.UUID
	for _, p := range payments {
		id := uuid.New()
		payIDs = append(payIDs, id)
		_, err := conn.Exec(`
			INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, created_at, updated_at)
			VALUES ($1,$2,'mercadopago','kc_recharge','starter','Starter',$3,0,800,'ext','` + p.status + `',NOW(),NOW())`,
			id, custID, p.amount)
		if err != nil {
			t.Fatalf("insert payment: %v", err)
		}
	}
	defer func() {
		for _, id := range payIDs {
			conn.Exec(`DELETE FROM payment_transactions WHERE id=$1`, id)
		}
	}()

	_, totalPEN, pendingCount, err := GetCustomerRechargeStats(conn, custID)
	if err != nil {
		t.Fatalf("GetCustomerRechargeStats: %v", err)
	}
	if totalPEN != 10.40 {
		t.Errorf("total pagado esperado 10.40 (solo el pago approved) — NUNCA la suma de los 3 (100.10) — obtuve %.2f", totalPEN)
	}
	if pendingCount != 1 {
		t.Errorf("pagos pendientes esperado 1, obtuve %d", pendingCount)
	}
}

// ==================== ESTADOS SOBRESCRITOS POR CONCURRENCIA (punto 4) ====================

// TestAdminUpdatePaymentStatus_NoSobrescribeUnPagoYaAcreditado reproduce la
// secuencia exacta del hallazgo: una consulta de reconciliación arranca
// mientras el pago está 'pending', pero para cuando termina y decide
// marcarlo 'failed', un webhook concurrente YA lo acreditó de verdad
// (CreditPaymentOnce). La escritura tardía no debe poder deshacer la
// acreditación real.
func TestAdminUpdatePaymentStatus_NoSobrescribeUnPagoYaAcreditado(t *testing.T) {
	conn := setupTestDB(t)
	custID, cleanup := newTestCustomer(t, conn)
	defer cleanup()

	txID := uuid.New()
	_, err := conn.Exec(`
		INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, created_at, updated_at)
		VALUES ($1,$2,'nowpayments','kc_recharge','starter','Starter',10.40,2.80,800,'ext-test','pending',NOW(),NOW())`,
		txID, custID)
	if err != nil {
		t.Fatalf("insert payment_transactions: %v", err)
	}

	// El webhook "gana la carrera": acredita de verdad antes que la
	// reconciliación tardía intente marcarlo failed/expired/review.
	credited, ptx, err := CreditPaymentOnce(conn, txID)
	if err != nil || !credited {
		t.Fatalf("CreditPaymentOnce: credited=%v err=%v", credited, err)
	}
	if ptx.Status != "approved" {
		t.Fatalf("status esperado 'approved' tras acreditar, obtuve %q", ptx.Status)
	}

	for _, staleStatus := range []string{"failed", "expired", "review"} {
		if err := AdminUpdatePaymentStatus(conn, txID, staleStatus); err != nil {
			t.Fatalf("AdminUpdatePaymentStatus(%q): error inesperado: %v", staleStatus, err)
		}
		got, err := GetPaymentByID(conn, txID)
		if err != nil {
			t.Fatalf("GetPaymentByID: %v", err)
		}
		if got.Status != "approved" {
			t.Errorf("una escritura tardía a %q pisó un pago ya acreditado — status quedó en %q, se esperaba que siguiera 'approved'", staleStatus, got.Status)
		}
	}

	var balance int
	conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, custID).Scan(&balance)
	if balance != 800 {
		t.Fatalf("el balance debería seguir en 800 (el KC ya se acreditó y nunca se revirtió), obtuve %d", balance)
	}
}

// TestAdminUpdatePaymentStatus_SiPermiteTransicionesNormales confirma que el
// chequeo de concurrencia no bloquea las transiciones normales — un pago
// que sigue genuinamente pendiente (nunca se acreditó) sí debe poder
// marcarse failed/expired/review sin problema.
func TestAdminUpdatePaymentStatus_SiPermiteTransicionesNormales(t *testing.T) {
	conn := setupTestDB(t)
	custID, cleanup := newTestCustomer(t, conn)
	defer cleanup()

	txID := uuid.New()
	_, err := conn.Exec(`
		INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, created_at, updated_at)
		VALUES ($1,$2,'mercadopago','kc_recharge','starter','Starter',10.40,2.80,800,'ext-test','pending',NOW(),NOW())`,
		txID, custID)
	if err != nil {
		t.Fatalf("insert payment_transactions: %v", err)
	}

	if err := AdminUpdatePaymentStatus(conn, txID, "review"); err != nil {
		t.Fatalf("AdminUpdatePaymentStatus: %v", err)
	}
	got, err := GetPaymentByID(conn, txID)
	if err != nil {
		t.Fatalf("GetPaymentByID: %v", err)
	}
	if got.Status != "review" {
		t.Errorf("un pago genuinamente pendiente (nunca acreditado) debería poder pasar a 'review', quedó en %q", got.Status)
	}
}

// ==================== PAGOS INCIERTOS RECUPERABLES (punto 3) ====================

// TestGetStalePendingPayments_IncluyePagosEnRevision confirma que un pago
// que ya pasó a 'review' (el backend nunca pudo confirmarlo NI
// descartarlo, ver reconcileDeadLetterAfter) sigue entrando en la
// reconciliación automática — si no, una confirmación tardía de la
// pasarela nunca se llegaría a acreditar solo porque el status ya no es
// 'pending'.
func TestGetStalePendingPayments_IncluyePagosEnRevision(t *testing.T) {
	conn := setupTestDB(t)
	custID, cleanup := newTestCustomer(t, conn)
	defer cleanup()

	txID := uuid.New()
	_, err := conn.Exec(`
		INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, created_at, updated_at)
		VALUES ($1,$2,'nowpayments','kc_recharge','starter','Starter',10.40,2.80,800,'ext-test','review',NOW()-INTERVAL '7 hours',NOW())`,
		txID, custID)
	if err != nil {
		t.Fatalf("insert payment_transactions: %v", err)
	}

	stale, err := GetStalePendingPayments(conn)
	if err != nil {
		t.Fatalf("GetStalePendingPayments: %v", err)
	}
	found := false
	for _, p := range stale {
		if p.ID == txID {
			found = true
		}
	}
	if !found {
		t.Error("un pago en 'review' con external_id debería seguir siendo candidato a reconciliación automática (confirmaciones tardías)")
	}
}

// ==================== HISTORIAL SIN DUPLICADOS (punto 5) ====================

// TestGetRechargesByCustomer_DistingueManualDeAutomatico confirma que
// payment_transaction_id llega correctamente en la respuesta — es la señal
// que usa el dashboard del cliente para no mostrar dos veces la misma
// operación (una vez como "recarga" y otra como el pago que la generó).
func TestGetRechargesByCustomer_DistingueManualDeAutomatico(t *testing.T) {
	conn := setupTestDB(t)
	custID, cleanup := newTestCustomer(t, conn)
	defer cleanup()

	// Recarga manual genuina (Yape aprobado a mano por un admin) — sin
	// payment_transaction_id.
	manualID := uuid.New()
	if _, err := conn.Exec(`
		INSERT INTO kc_recharges (id, customer_id, amount_kc, amount_soles, method, note, approved_by, created_at)
		VALUES ($1,$2,500,6.50,'yape','recarga manual de prueba','admin_test',NOW())`,
		manualID, custID); err != nil {
		t.Fatalf("insert kc_recharges manual: %v", err)
	}

	// Pago por pasarela acreditado automáticamente — CreditPaymentOnce
	// inserta la fila de kc_recharges CON payment_transaction_id.
	txID := uuid.New()
	if _, err := conn.Exec(`
		INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, created_at, updated_at)
		VALUES ($1,$2,'paypal','kc_recharge','starter','Starter',10.40,2.80,800,'ext-test','pending',NOW(),NOW())`,
		txID, custID); err != nil {
		t.Fatalf("insert payment_transactions: %v", err)
	}
	if credited, _, err := CreditPaymentOnce(conn, txID); err != nil || !credited {
		t.Fatalf("CreditPaymentOnce: credited=%v err=%v", credited, err)
	}

	recharges, err := GetRechargesByCustomer(conn, custID)
	if err != nil {
		t.Fatalf("GetRechargesByCustomer: %v", err)
	}
	if len(recharges) != 2 {
		t.Fatalf("esperaba 2 filas (una manual, una del pago), obtuve %d", len(recharges))
	}
	var manualSeen, linkedSeen bool
	for _, r := range recharges {
		if r.ID == manualID {
			manualSeen = true
			if r.PaymentTransactionID != nil {
				t.Error("la recarga manual no debería tener payment_transaction_id")
			}
		}
		if r.PaymentTransactionID != nil && *r.PaymentTransactionID == txID {
			linkedSeen = true
		}
	}
	if !manualSeen {
		t.Error("no se encontró la recarga manual en el resultado")
	}
	if !linkedSeen {
		t.Error("la recarga generada por CreditPaymentOnce debería traer payment_transaction_id apuntando al pago que la generó")
	}
}

// ==================== RECUPERACIÓN DE WEBHOOKS (punto 2) ====================

// TestGetUnresolvedWebhookEvents_DetectaEventosSinResolver cubre "fallo de
// la primera notificación de NOWPayments": un evento se registra (como
// hace LogWebhookEvent ANTES de procesar), la primera consulta a la
// pasarela falla, y el evento debe seguir apareciendo como candidato a
// reintento hasta que de verdad se procese con éxito.
func TestGetUnresolvedWebhookEvents_DetectaEventosSinResolver(t *testing.T) {
	conn := setupTestDB(t)

	failedID, err := LogWebhookEvent(conn, "nowpayments", `{"payment_id":123456}`)
	if err != nil { t.Fatalf("LogWebhookEvent: %v", err) }
	defer conn.Exec(`DELETE FROM webhook_events WHERE id=$1`, failedID)
	MarkWebhookEventProcessed(conn, failedID, "error: status query failed")

	crashedID, err := LogWebhookEvent(conn, "nowpayments", `{"payment_id":789012}`)
	if err != nil { t.Fatalf("LogWebhookEvent: %v", err) }
	defer conn.Exec(`DELETE FROM webhook_events WHERE id=$1`, crashedID)
	// Nunca se llama a MarkWebhookEventProcessed — simula que el proceso se
	// cayó a mitad de camino (processed_at se queda NULL para siempre si
	// nada lo reintenta).

	processedID, err := LogWebhookEvent(conn, "nowpayments", `{"payment_id":345678}`)
	if err != nil { t.Fatalf("LogWebhookEvent: %v", err) }
	defer conn.Exec(`DELETE FROM webhook_events WHERE id=$1`, processedID)
	MarkWebhookEventProcessed(conn, processedID, "processed")

	// minAge=0 para no esperar el margen real de 3 minutos en la prueba.
	unresolved, err := GetUnresolvedWebhookEvents(conn, "nowpayments", 0, 48*time.Hour)
	if err != nil {
		t.Fatalf("GetUnresolvedWebhookEvents: %v", err)
	}
	ids := map[uuid.UUID]bool{}
	for _, e := range unresolved {
		ids[e.ID] = true
	}
	if !ids[failedID] {
		t.Error("un evento cuya primera consulta falló ('error: ...') debería quedar disponible para reintento")
	}
	if !ids[crashedID] {
		t.Error("un evento nunca marcado como procesado (proceso caído a mitad de camino) debería quedar disponible para reintento")
	}
	if ids[processedID] {
		t.Error("un evento ya procesado con éxito NO debería reintentarse")
	}
}

// ==================== ROTACIÓN DE PAGOS PENDIENTES (punto 5) ====================

// TestGetStalePendingPayments_RotaEntreMasDe100PagosAntiguos cubre "más de
// 100 pagos antiguos sin resolver junto con pagos nuevos": con 120 pagos
// viejos sin resolver, la primera pasada trae 100 (el límite) — pero tras
// tocarlos (como hace reconcileOnePayment vía TouchPaymentReconciled) para
// simular que el barrido ya los revisó, la SEGUNDA pasada debe traer los
// 20 restantes en vez de repetir siempre los mismos 100 primeros.
func TestGetStalePendingPayments_RotaEntreMasDe100PagosAntiguos(t *testing.T) {
	conn := setupTestDB(t)
	custID, cleanup := newTestCustomer(t, conn)
	defer cleanup()

	const total = 120
	ids := make([]uuid.UUID, total)
	for i := 0; i < total; i++ {
		ids[i] = uuid.New()
		// created_at bien en el pasado para que pase el margen de 2 minutos;
		// updated_at escalonado (más antiguo cuanto más bajo el índice) para
		// que el orden inicial sea determinístico.
		_, err := conn.Exec(`
			INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, created_at, updated_at)
			VALUES ($1,$2,'nowpayments','kc_recharge','starter','Starter',10.40,2.80,800,$3,'pending',NOW()-INTERVAL '1 hour',NOW()-INTERVAL '1 hour' + ($4 * INTERVAL '1 second'))`,
			ids[i], custID, fmt.Sprintf("ext-%d", i), i)
		if err != nil {
			t.Fatalf("insert payment_transactions[%d]: %v", i, err)
		}
	}
	defer func() {
		for _, id := range ids {
			conn.Exec(`DELETE FROM payment_transactions WHERE id=$1`, id)
		}
	}()

	// Primera pasada: los 100 con el updated_at más antiguo (índices 0-99).
	firstBatch, err := GetStalePendingPayments(conn)
	if err != nil {
		t.Fatalf("GetStalePendingPayments (primera pasada): %v", err)
	}
	if len(firstBatch) != 100 {
		t.Fatalf("esperaba 100 resultados en la primera pasada (el límite), obtuve %d", len(firstBatch))
	}
	firstIDs := map[uuid.UUID]bool{}
	for _, p := range firstBatch {
		firstIDs[p.ID] = true
		if err := TouchPaymentReconciled(conn, p.ID); err != nil {
			t.Fatalf("TouchPaymentReconciled: %v", err)
		}
	}

	// Segunda pasada: como los 100 de la primera pasada ya se tocaron
	// (updated_at = ahora, más reciente que los 20 restantes, que nunca se
	// tocaron), esta pasada debe traer esos 20 restantes primero.
	secondBatch, err := GetStalePendingPayments(conn)
	if err != nil {
		t.Fatalf("GetStalePendingPayments (segunda pasada): %v", err)
	}
	newlySeen := 0
	for _, p := range secondBatch {
		if !firstIDs[p.ID] {
			newlySeen++
		}
	}
	if newlySeen != 20 {
		t.Errorf("la segunda pasada debería traer los 20 pagos que la primera no alcanzó a revisar, encontró %d pagos nuevos de %d", newlySeen, len(secondBatch))
	}
}

// ==================== HISTORIAL COMPLETO DE PAGOS (punto 6) ====================

// TestGetRechargeHistoryByCustomer_MasDe2000Operaciones cubre "más de 2.000
// operaciones en el historial": antes GetPaymentsByCustomer tenía un LIMIT
// 2000 fijo (y GetRechargesByCustomer, sin límite, se combinaba con él en el
// NAVEGADOR) — con más de 2000 intentos de pago, los pagos más antiguos
// desaparecían del historial y de sus propios comprobantes sin ningún
// aviso. GetRechargeHistoryByCustomer pagina de verdad en la base de datos:
// esta prueba recorre TODAS las páginas y confirma que el total real (pagos
// + recargas manuales, sin las vinculadas a un pago que ya cuenta del otro
// lado) aparece completo, sin huecos ni duplicados.
func TestGetRechargeHistoryByCustomer_MasDe2000Operaciones(t *testing.T) {
	conn := setupTestDB(t)
	custID, cleanup := newTestCustomer(t, conn)
	defer cleanup()

	const numPayments = 2005
	if _, err := conn.Exec(`
		INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, created_at, updated_at)
		SELECT gen_random_uuid(), $1, 'mercadopago', 'kc_recharge', 'starter', 'Starter', 10.40, 2.80, 800,
		       'ext-hist-' || i, 'approved', NOW() - (i * INTERVAL '1 minute'), NOW()
		FROM generate_series(1, $2) AS i`, custID, numPayments); err != nil {
		t.Fatalf("insert masivo de payment_transactions: %v", err)
	}
	defer conn.Exec(`DELETE FROM payment_transactions WHERE customer_id=$1`, custID)

	// Recargas manuales genuinas (deben contar y aparecer).
	const numManual = 3
	for i := 0; i < numManual; i++ {
		if _, err := conn.Exec(`
			INSERT INTO kc_recharges (id, customer_id, amount_kc, amount_soles, method, note, created_at)
			VALUES (gen_random_uuid(), $1, 500, 6.5, 'yape', 'manual', NOW() - ($2 * INTERVAL '1 minute'))`,
			custID, i); err != nil {
			t.Fatalf("insert kc_recharges manual[%d]: %v", i, err)
		}
	}

	// Recarga vinculada a un pago (acreditación automática, ver
	// CreditPaymentOnce) — NO debe contarse ni aparecer aparte: es la misma
	// operación que ya está del lado de payment_transactions.
	var linkedPaymentID uuid.UUID
	if err := conn.QueryRow(`SELECT id FROM payment_transactions WHERE customer_id=$1 LIMIT 1`, custID).Scan(&linkedPaymentID); err != nil {
		t.Fatalf("leer un payment_transaction para vincular: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO kc_recharges (id, customer_id, amount_kc, amount_soles, method, note, payment_transaction_id, created_at)
		VALUES (gen_random_uuid(), $1, 800, 10.40, 'mercadopago', 'auto', $2, NOW())`,
		custID, linkedPaymentID); err != nil {
		t.Fatalf("insert kc_recharges vinculada: %v", err)
	}
	defer conn.Exec(`DELETE FROM kc_recharges WHERE customer_id=$1`, custID)

	wantTotal := numPayments + numManual

	// Página 1 debe reportar el total real, no solo lo que trae esa página.
	firstPage, total, err := GetRechargeHistoryByCustomer(conn, custID, 1, 100)
	if err != nil {
		t.Fatalf("GetRechargeHistoryByCustomer (página 1): %v", err)
	}
	if total != wantTotal {
		t.Fatalf("total esperado %d (pagos + manuales, sin la vinculada), obtuve %d", wantTotal, total)
	}
	if len(firstPage) != 100 {
		t.Fatalf("esperaba 100 items en la primera página, obtuve %d", len(firstPage))
	}

	// Recorrer TODAS las páginas y verificar cobertura completa sin
	// duplicados — la prueba real de que la paginación no dejó pagos
	// antiguos inalcanzables (como pasaba con el LIMIT 2000 fijo) ni
	// repitió ninguno entre páginas.
	seen := map[uuid.UUID]bool{}
	const pageSize = 100 // tope máximo que GetRechargeHistoryByCustomer acepta por página
	pages := (wantTotal + pageSize - 1) / pageSize
	for p := 1; p <= pages; p++ {
		items, pTotal, err := GetRechargeHistoryByCustomer(conn, custID, p, pageSize)
		if err != nil {
			t.Fatalf("GetRechargeHistoryByCustomer (página %d): %v", p, err)
		}
		if pTotal != wantTotal {
			t.Fatalf("página %d: total inconsistente, esperaba %d obtuve %d", p, wantTotal, pTotal)
		}
		for _, it := range items {
			if seen[it.ID] {
				t.Fatalf("página %d: item %s ya había aparecido en otra página (duplicado)", p, it.ID)
			}
			seen[it.ID] = true
			if it.Kind == "pay" && it.ID == linkedPaymentID {
				continue // esperado: el pago vinculado SÍ debe aparecer (una sola vez, del lado 'pay')
			}
		}
	}
	if len(seen) != wantTotal {
		t.Fatalf("recorriendo todas las páginas se vieron %d items únicos, esperaba %d (¿algún pago antiguo quedó inalcanzable?)", len(seen), wantTotal)
	}
	if seen[linkedPaymentID] != true {
		t.Errorf("el pago vinculado a una recarga automática debería seguir apareciendo (del lado 'pay')")
	}
}

// ==================== TRANSICIÓN ATÓMICA A REVISIÓN (punto 1) ====================

// TestResolveOrderReview_DejaEstadoCoherente cubre "fallo entre el
// registro de incertidumbre y el paso a revisión": ResolveOrderReview hace
// las dos escrituras (limpiar send_attempted y guardar status='review') en
// una sola transacción — acá se comprueba que el resultado final sea
// coherente (nunca "review" con send_attempted todavía en true, ni
// viceversa).
func TestResolveOrderReview_DejaEstadoCoherente(t *testing.T) {
	conn := setupTestDB(t)
	custID, cleanup := newTestCustomer(t, conn)
	defer cleanup()

	orderID := uuid.New()
	_, err := conn.Exec(`
		INSERT INTO orders (id, customer_id, epic_username, item_offer_id, item_name, price_kc, price_vbucks, status, send_attempted, created_at, updated_at)
		VALUES ($1,$2,'recv','offer-1','Item',100,100,'processing',true,NOW(),NOW())`,
		orderID, custID)
	if err != nil {
		t.Fatalf("insert orders: %v", err)
	}
	defer conn.Exec(`DELETE FROM orders WHERE id=$1`, orderID)

	wasAttempting, err := ResolveOrderReview(conn, orderID, "motivo de prueba")
	if err != nil {
		t.Fatalf("ResolveOrderReview: %v", err)
	}
	if !wasAttempting {
		t.Error("con send_attempted=true, ResolveOrderReview debería devolver wasAttempting=true")
	}

	var status string
	var sendAttempted bool
	if err := conn.QueryRow(`SELECT status, send_attempted FROM orders WHERE id=$1`, orderID).Scan(&status, &sendAttempted); err != nil {
		t.Fatalf("no se pudo leer el pedido: %v", err)
	}
	if status != "review" {
		t.Errorf("status esperado 'review', obtuve %q", status)
	}
	if sendAttempted {
		t.Error("send_attempted debería quedar en false junto con el paso a 'review' — nunca 'review' con la marca todavía prendida")
	}
}
