package db

// Pruebas de regresión para las correcciones del audit de 25 puntos —
// cuentas, 2FA, pagos duplicados, fallos de acreditación, reinicios,
// devoluciones y paginación. Se conectan a la MISMA base de datos que usa
// la app (mismas variables de entorno DB_*) porque este proyecto no tiene
// una capa de DB simulada — son pruebas de integración, no puramente
// unitarias. Cada prueba crea su propio cliente de prueba con un
// epic_username/email único (sufijo aleatorio) y lo borra al terminar, sin
// tocar nunca la cuenta admin real ni datos de clientes reales.
//
// Si las variables DB_* no están configuradas (ej. una corrida de CI sin
// acceso a la base), las pruebas se saltan en vez de fallar — no tiene
// sentido que "no hay credenciales" se reporte como "la prueba falló".

import (
	"database/sql"
	"fmt"
	"os"
	"testing"

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
	// el paquete (src/db), no en la raíz del proyecto donde vive .env —
	// se intenta cargar desde ambos lugares; si ninguno tiene las
	// variables, se cae al entorno del proceso tal cual (por si ya
	// estaban exportadas).
	godotenv.Load()
	godotenv.Load("../../.env")
	host := os.Getenv("DB_HOST")
	if host == "" {
		t.Skip("DB_HOST no configurado — se salta esta prueba de integración (necesita la base de datos real)")
	}
	port := os.Getenv("DB_PORT")
	if port == "" {
		port = "5432"
	}
	psqlInfo := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=require",
		host, port, os.Getenv("DB_USER"), os.Getenv("DB_PASSWORD"), os.Getenv("DB_NAME"))
	conn, err := sql.Open("postgres", psqlInfo)
	if err != nil {
		t.Fatalf("no se pudo abrir la conexión de prueba: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Skipf("no se pudo conectar a la base de datos de prueba: %v", err)
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
