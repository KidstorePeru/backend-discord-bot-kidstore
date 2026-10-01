package backup

// Pruebas del respaldo con una Postgres REAL embebida y descartable (ver
// src/testdb), nunca la base de producción. Cada prueba crea sus propias
// bases de datos vacías dentro de esa instancia.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"KidStoreStore/src/db"
	"KidStoreStore/src/testdb"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

const testKey = "clave-de-prueba-de-respaldo-con-mas-de-32-caracteres"

func TestMain(m *testing.M) {
	cleanup, err := testdb.StartEmbeddedIfNeeded()
	if err != nil {
		fmt.Fprintln(os.Stderr, "aviso: no se pudo iniciar Postgres embebida — las pruebas de integración se saltarán:", err)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func connString(dbName string) string {
	port := os.Getenv("TEST_DB_PORT")
	if port == "" {
		port = "5432"
	}
	ssl := os.Getenv("TEST_DB_SSLMODE")
	if ssl == "" {
		ssl = "disable"
	}
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		os.Getenv("TEST_DB_HOST"), port, os.Getenv("TEST_DB_USER"), os.Getenv("TEST_DB_PASSWORD"), dbName, ssl)
}

// newEmptyDB crea una base de datos nueva con el esquema de la app.
func newEmptyDB(t *testing.T) *sql.DB {
	t.Helper()
	if os.Getenv("TEST_DB_HOST") == "" {
		t.Skip("sin base de datos de prueba")
	}
	admin, err := sql.Open("postgres", connString(os.Getenv("TEST_DB_NAME")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	name := "bk_" + strings.ReplaceAll(uuid.New().String()[:12], "-", "")
	if _, err := admin.Exec(`CREATE DATABASE ` + name + ` ENCODING 'UTF8' TEMPLATE template0 LC_COLLATE 'C' LC_CTYPE 'C'`); err != nil {
		t.Skipf("no se pudo crear una base de prueba: %v", err)
	}
	conn, err := sql.Open("postgres", connString(name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close()
		admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
	})
	if err := db.CreateTables(conn); err != nil {
		t.Fatalf("CreateTables: %v", err)
	}
	return conn
}

func mustExec(t *testing.T, conn *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// seed carga datos variados: relaciones entre tablas, texto con tildes y
// emojis, nulos, decimales, fechas y la configuración de los bots cambiada.
func seed(t *testing.T, conn *sql.DB) {
	t.Helper()
	c1, c2, bot := uuid.New(), uuid.New(), uuid.New()
	mustExec(t, conn, `INSERT INTO customers (id, epic_username, email, password_hash, kc_balance, created_at)
		VALUES ($1, 'Ñandú_Gamer 🎮', 'nandu@example.com', 'hash1', 2450, '2026-09-01 10:00:00'),
		       ($2, 'SinCorreo', NULL, 'hash2', 0, '2026-09-02 11:30:00')`, c1, c2)
	mustExec(t, conn, `INSERT INTO kc_recharges (customer_id, amount_kc, amount_soles, method, note)
		VALUES ($1, 2400, 31.20, 'yape', 'recarga "con comillas" y \ barra')`, c1)
	mustExec(t, conn, `INSERT INTO orders (customer_id, epic_username, item_offer_id, item_name, price_kc, price_vbucks, status, game_account_id)
		VALUES ($1, 'Ñandú_Gamer 🎮', 'v2:/abc', 'Lote Madison Beer', 3400, 3400, 'sent', $2)`, c1, bot)
	mustExec(t, conn, `INSERT INTO game_accounts (id, display_name, remaining_gifts, vbucks, access_token, refresh_token)
		VALUES ($1, 'KidStore0012', 3, 12600, 'enc:token', 'enc:refresh')`, bot)
	mustExec(t, conn, `INSERT INTO game_account_secrets (account_id, device_id, secret) VALUES ($1, 'dev-1', 'enc:secret')`, bot)
	mustExec(t, conn, `INSERT INTO consumer_complaints (reference, kind, full_name, document_type, document_number, email,
		product_description, detail, consumer_request, amount_involved, lang)
		VALUES ('R-0001', 'reclamo', 'José Pérez', 'DNI', '12345678', 'jose@example.com', 'Skin', 'No llegó', 'Reembolso', 10.50, 'en')`)
	mustExec(t, conn, `UPDATE bot_schedule SET start_hour = 8, end_hour = 22 WHERE id = 1`)
}

// fingerprint resume el contenido de todas las tablas, para comparar dos
// bases fila por fila.
func fingerprint(t *testing.T, conn *sql.DB) map[string]string {
	t.Helper()
	tables, err := listTables(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, table := range tables {
		var s string
		q := `SELECT COALESCE(string_agg(row_to_json(t)::text, E'\n' ORDER BY row_to_json(t)::text), '') FROM ` + table + ` t`
		if err := conn.QueryRow(q).Scan(&s); err != nil {
			t.Fatal(err)
		}
		out[table] = s
	}
	return out
}

func TestRespaldo_SeRestauraIgualEnUnaBaseVacia(t *testing.T) {
	src := newEmptyDB(t)
	seed(t, src)

	plain, header, err := Dump(context.Background(), src, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if header.Tables["customers"] != 2 || header.Tables["orders"] != 1 || header.Tables["bot_schedule"] != 1 {
		t.Errorf("cabecera inesperada: %v", header.Tables)
	}
	data, err := Encrypt(plain, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "nandu@example.com") {
		t.Fatal("el archivo cifrado no debería contener datos legibles")
	}

	dst := newEmptyDB(t)
	result, err := Restore(context.Background(), dst, data, testKey)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(result.SkippedColumns) != 0 {
		t.Errorf("no debería omitir columnas: %v", result.SkippedColumns)
	}
	want, got := fingerprint(t, src), fingerprint(t, dst)
	for table := range want {
		if want[table] != got[table] {
			t.Errorf("la tabla %s no quedó igual:\noriginal:  %s\nrestaurada: %s", table, want[table], got[table])
		}
	}

	// Una segunda restauración sobre la misma base (ya con datos) se niega.
	if _, err := Restore(context.Background(), dst, data, testKey); !errors.Is(err, ErrTargetNotEmpty) {
		t.Errorf("restaurar sobre una base con datos debería fallar con ErrTargetNotEmpty, obtuve %v", err)
	}
	if fp := fingerprint(t, dst); fp["customers"] != want["customers"] {
		t.Error("un intento fallido no debería cambiar nada")
	}
}

// Una copia vieja se puede cargar en un esquema más nuevo: las columnas
// nuevas toman su valor por defecto y las que ya no existen se avisan.
func TestRespaldo_EsquemaDistintoAlRestaurar(t *testing.T) {
	src := newEmptyDB(t)
	seed(t, src)
	mustExec(t, src, `ALTER TABLE customers ADD COLUMN columna_vieja TEXT DEFAULT 'x'`)
	plain, _, err := Dump(context.Background(), src, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := Encrypt(plain, testKey)

	dst := newEmptyDB(t)
	mustExec(t, dst, `ALTER TABLE customers ADD COLUMN columna_nueva INTEGER NOT NULL DEFAULT 7`)
	result, err := Restore(context.Background(), dst, data, testKey)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(result.SkippedColumns) != 1 || result.SkippedColumns[0] != "customers.columna_vieja" {
		t.Errorf("columnas omitidas = %v", result.SkippedColumns)
	}
	var n, sum int
	if err := dst.QueryRow(`SELECT COUNT(*), SUM(columna_nueva) FROM customers`).Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	if n != 2 || sum != 14 {
		t.Errorf("clientes = %d, suma de columna_nueva = %d; se esperaba 2 y 14 (valor por defecto)", n, sum)
	}
}

func TestRespaldo_ClaveIncorrectaOArchivoAlterado(t *testing.T) {
	plain := []byte("contenido")
	data, err := Encrypt(plain, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(data, testKey+"x"); !errors.Is(err, ErrWrongKeyOrCorrupt) {
		t.Errorf("clave incorrecta: obtuve %v", err)
	}
	altered := append([]byte(nil), data...)
	altered[len(altered)-1] ^= 1
	if _, err := Decrypt(altered, testKey); !errors.Is(err, ErrWrongKeyOrCorrupt) {
		t.Errorf("archivo alterado: obtuve %v", err)
	}
	if _, err := Decrypt([]byte("no es un respaldo"), testKey); !errors.Is(err, ErrNotABackup) {
		t.Errorf("archivo ajeno: obtuve %v", err)
	}
	if got, err := Decrypt(data, testKey); err != nil || string(got) != "contenido" {
		t.Errorf("Decrypt = %q, %v", got, err)
	}
}

// ── Programador (almacenamiento simulado en memoria) ──

type memStore struct {
	objects map[string][]byte
	failPut bool
}

func (m *memStore) Put(_ context.Context, key string, data []byte) error {
	if m.failPut {
		return errors.New("bucket no encontrado")
	}
	m.objects[key] = data
	return nil
}

func (m *memStore) List(_ context.Context, prefix string) ([]Object, error) {
	var out []Object
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, Object{Key: k})
		}
	}
	return out, nil
}

func (m *memStore) Delete(_ context.Context, key string) error {
	delete(m.objects, key)
	return nil
}

func (m *memStore) keys() []string {
	var k []string
	for key := range m.objects {
		k = append(k, key)
	}
	sort.Strings(k)
	return k
}

func TestJob_UnaCopiaPorDiaYRetencion(t *testing.T) {
	src := newEmptyDB(t)
	seed(t, src)
	store := &memStore{objects: map[string][]byte{}}
	firsts := 0
	OnFirstSuccess = func(int, Header) { firsts++ }
	t.Cleanup(func() { OnFirstSuccess = func(int, Header) {} })

	job := &Job{DB: src, Store: store, Passphrase: testKey, RetentionDays: 30}
	start := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

	if did, err := job.RunOnce(context.Background(), start); err != nil || !did {
		t.Fatalf("primera copia: %v, %v", did, err)
	}
	if firsts != 1 {
		t.Errorf("el aviso de primera copia se mandó %d veces", firsts)
	}
	// Una hora después no toca otra copia.
	if did, _ := job.RunOnce(context.Background(), start.Add(time.Hour)); did {
		t.Error("no debería hacer otra copia antes de 23 horas")
	}
	// La copia guardada se abre y trae los datos.
	header, err := Inspect(store.objects[objectKey(start)], testKey)
	if err != nil || header.Tables["customers"] != 2 {
		t.Fatalf("la copia guardada no se pudo abrir: %v %v", header.Tables, err)
	}

	// 40 días de copias diarias: quedan solo las de los últimos 30 días.
	for d := 1; d <= 40; d++ {
		if _, err := job.RunOnce(context.Background(), start.AddDate(0, 0, d)); err != nil {
			t.Fatal(err)
		}
	}
	keys := store.keys()
	if len(keys) != 31 {
		t.Errorf("quedaron %d copias, se esperaban 31 (hoy + 30 días)", len(keys))
	}
	if keys[0] != objectKey(start.AddDate(0, 0, 10)) {
		t.Errorf("la copia más vieja que queda es %s", keys[0])
	}
	if firsts != 1 {
		t.Errorf("el aviso de primera copia se mandó %d veces", firsts)
	}
}

func TestJob_SiempreQuedanLasUltimasSieteCopias(t *testing.T) {
	src := newEmptyDB(t)
	store := &memStore{objects: map[string][]byte{}}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for d := 0; d < 9; d++ {
		store.objects[objectKey(old.AddDate(0, 0, d))] = []byte("x")
	}
	job := &Job{DB: src, Store: store, Passphrase: testKey, RetentionDays: 30}
	// Los respaldos estuvieron caídos meses: todas las copias son viejas.
	if _, err := job.RunOnce(context.Background(), old.AddDate(0, 6, 0)); err != nil {
		t.Fatal(err)
	}
	if n := len(store.objects); n != 7 {
		t.Errorf("quedaron %d copias, se esperaban 7 (la nueva + las 6 más recientes)", n)
	}
}

func TestJob_AvisaSiFallaYCuandoSeRecupera(t *testing.T) {
	src := newEmptyDB(t)
	store := &memStore{objects: map[string][]byte{}, failPut: true}
	var failures []string
	recovered := 0
	OnFailure = func(r string) { failures = append(failures, r) }
	OnRecovered = func() { recovered++ }
	t.Cleanup(func() { OnFailure = func(string) {}; OnRecovered = func() {} })

	job := &Job{DB: src, Store: store, Passphrase: testKey, RetentionDays: 30}
	now := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	if _, err := job.RunOnce(context.Background(), now); err == nil {
		t.Fatal("debería fallar si no se puede subir")
	}
	if len(failures) != 1 || !strings.Contains(failures[0], "bucket no encontrado") {
		t.Errorf("avisos de fallo = %v", failures)
	}
	store.failPut = false
	if did, err := job.RunOnce(context.Background(), now.Add(time.Hour)); err != nil || !did {
		t.Fatalf("reintento: %v, %v", did, err)
	}
	if recovered != 1 {
		t.Errorf("aviso de recuperación = %d, se esperaba 1", recovered)
	}
}

func TestTopoSort_CargaPrimeroLasTablasReferenciadas(t *testing.T) {
	deps := map[string]map[string]bool{
		"orders":               {"customers": true},
		"kc_recharges":         {"customers": true, "payment_transactions": true},
		"payment_transactions": {"customers": true},
	}
	order, err := topoSort([]string{"orders", "kc_recharges", "payment_transactions", "customers"}, deps)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, t := range order {
		pos[t] = i
	}
	if !(pos["customers"] < pos["payment_transactions"] && pos["payment_transactions"] < pos["kc_recharges"] && pos["customers"] < pos["orders"]) {
		t.Errorf("orden incorrecto: %v", order)
	}
	if _, err := topoSort([]string{"a", "b"}, map[string]map[string]bool{"a": {"b": true}, "b": {"a": true}}); err == nil {
		t.Error("un ciclo debería ser un error")
	}
}
