package oauth

// Regresión: un login OAuth (Google/Discord) de una cuenta INACTIVA se
// rechaza ANTES de emitir cualquier JWT, refresh token o código de login.
// Antes, las consultas por google_id/discord_id/email solo veían cuentas
// activas, así que una cuenta desactivada caía en la rama "cliente nuevo".
// Usa la misma base de pruebas embebida que el resto de los paquetes.

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"KidStoreStore/src/db"
	"KidStoreStore/src/testdb"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

func TestMain(m *testing.M) {
	cleanup, err := testdb.StartEmbeddedIfNeeded()
	if err != nil {
		fmt.Fprintln(os.Stderr, "aviso: no se pudo iniciar Postgres embebida para pruebas — las pruebas de integración se saltarán:", err)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

var oauthTestDB *sql.DB

func setupOAuthTestDB(t *testing.T) *sql.DB {
	t.Helper()
	if oauthTestDB != nil {
		return oauthTestDB
	}
	host := os.Getenv("TEST_DB_HOST")
	if host == "" {
		t.Skip("TEST_DB_HOST no configurado — se salta esta prueba de integración (base de PRUEBA, nunca producción)")
	}
	port := os.Getenv("TEST_DB_PORT")
	if port == "" {
		port = "5432"
	}
	conn, err := sql.Open("postgres", fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		host, port, os.Getenv("TEST_DB_USER"), os.Getenv("TEST_DB_PASSWORD"), os.Getenv("TEST_DB_NAME")))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Skipf("no se pudo conectar a la base de pruebas: %v", err)
	}
	if err := db.CreateTables(conn); err != nil {
		t.Fatalf("esquema: %v", err)
	}
	oauthTestDB = conn
	return conn
}

func insertOAuthCustomer(t *testing.T, conn *sql.DB, googleID, email string, active bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	suffix := id.String()[:8]
	if _, err := conn.Exec(`INSERT INTO customers (id, epic_username, email, password_hash, kc_balance, google_id, is_verified, is_active, is_admin, has_password, created_at, updated_at)
		VALUES ($1,$2,$3,'x',0,$4,true,$5,false,false,NOW(),NOW())`, id, "oauthtest_"+suffix, email, googleID, active); err != nil {
		t.Fatalf("insert customer: %v", err)
	}
	t.Cleanup(func() {
		conn.Exec(`DELETE FROM refresh_tokens WHERE customer_id=$1`, id)
		conn.Exec(`DELETE FROM audit_logs WHERE customer_id=$1`, id)
		conn.Exec(`DELETE FROM customers WHERE id=$1`, id)
	})
	return id
}

func runFinishOAuthLogin(t *testing.T, conn *sql.DB, googleID string, email *string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/auth/google/callback", nil)
	finishOAuthLogin(c, conn, Config{FrontendURL: "http://front.test", SecretKey: "oauth-test-secret"}, "google", googleID, email, "Tester")
	return w
}

func countRows(t *testing.T, conn *sql.DB, query string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("contar: %v", err)
	}
	return n
}

func TestFinishOAuthLogin_CuentaInactiva_SeRechazaSinEmitirTokens(t *testing.T) {
	conn := setupOAuthTestDB(t)
	suffix := uuid.New().String()[:8]
	googleID := "g-inactive-" + suffix
	email := "inactive-" + suffix + "@example.com"
	custID := insertOAuthCustomer(t, conn, googleID, email, false)
	codesBefore := countRows(t, conn, `SELECT COUNT(*) FROM oauth_login_codes`)
	pendingBefore := countRows(t, conn, `SELECT COUNT(*) FROM pending_oauth_registrations`)

	cases := map[string]struct {
		googleID string
		email    *string
	}{
		"por google_id vinculado":        {googleID, nil},
		"por correo (google_id nuevo)":   {"g-otro-" + suffix, &email},
		"por correo en otra capitalización": {"g-otro2-" + suffix, func() *string { e := strings.ToUpper(email); return &e }()},
	}
	for name, tc := range cases {
		w := runFinishOAuthLogin(t, conn, tc.googleID, tc.email)
		loc := w.Header().Get("Location")
		if w.Code != http.StatusFound || !strings.Contains(loc, "oauth_error=account_inactive") {
			t.Errorf("%s: debe redirigir con account_inactive, obtuve %d %q", name, w.Code, loc)
		}
		if strings.Contains(loc, "code=") || strings.Contains(loc, "/auth/complete") {
			t.Errorf("%s: no debe emitir código de login ni abrir un registro nuevo: %q", name, loc)
		}
	}
	if got := countRows(t, conn, `SELECT COUNT(*) FROM refresh_tokens WHERE customer_id=$1`, custID); got != 0 {
		t.Errorf("no debe emitirse ningún refresh token (hay %d)", got)
	}
	if got := countRows(t, conn, `SELECT COUNT(*) FROM oauth_login_codes`); got != codesBefore {
		t.Errorf("no debe crearse ningún código de login OAuth (antes %d, ahora %d)", codesBefore, got)
	}
	if got := countRows(t, conn, `SELECT COUNT(*) FROM pending_oauth_registrations`); got != pendingBefore {
		t.Errorf("no debe abrirse ningún registro pendiente (antes %d, ahora %d)", pendingBefore, got)
	}
}

func TestFinishOAuthLogin_CuentaActivaSigueFuncionando(t *testing.T) {
	conn := setupOAuthTestDB(t)
	suffix := uuid.New().String()[:8]
	googleID := "g-active-" + suffix
	custID := insertOAuthCustomer(t, conn, googleID, "active-"+suffix+"@example.com", true)

	w := runFinishOAuthLogin(t, conn, googleID, nil)
	loc := w.Header().Get("Location")
	if w.Code != http.StatusFound || !strings.Contains(loc, "/auth/callback?code=") {
		t.Fatalf("una cuenta activa debe recibir su código de login: %d %q", w.Code, loc)
	}
	if got := countRows(t, conn, `SELECT COUNT(*) FROM refresh_tokens WHERE customer_id=$1`, custID); got != 1 {
		t.Errorf("la cuenta activa debe recibir su refresh token (hay %d)", got)
	}
}

func TestFinishOAuthLogin_UsuarioNuevoSigueViendoElRegistro(t *testing.T) {
	conn := setupOAuthTestDB(t)
	suffix := uuid.New().String()[:8]
	email := "nuevo-" + suffix + "@example.com"
	t.Cleanup(func() { conn.Exec(`DELETE FROM pending_oauth_registrations WHERE provider_id=$1`, "g-new-"+suffix) })

	w := runFinishOAuthLogin(t, conn, "g-new-"+suffix, &email)
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "/auth/complete?token=") {
		t.Errorf("un usuario realmente nuevo debe seguir al registro: %q", loc)
	}
}

func TestIssueLoginRedirect_NuncaEmiteTokensParaCuentaInactiva(t *testing.T) {
	conn := setupOAuthTestDB(t)
	suffix := uuid.New().String()[:8]
	custID := insertOAuthCustomer(t, conn, "g-guard-"+suffix, "guard-"+suffix+"@example.com", true)
	customer, err := db.GetCustomerByID(conn, custID)
	if err != nil {
		t.Fatalf("GetCustomerByID: %v", err)
	}
	customer.IsActive = false // el guard no debe depender de cómo se resolvió la cuenta

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/x", nil)
	issueLoginRedirect(c, conn, Config{FrontendURL: "http://front.test", SecretKey: "oauth-test-secret"}, customer)

	if loc := w.Header().Get("Location"); !strings.Contains(loc, "account_inactive") {
		t.Errorf("debe rechazar con account_inactive: %q", loc)
	}
	if got := countRows(t, conn, `SELECT COUNT(*) FROM refresh_tokens WHERE customer_id=$1`, custID); got != 0 {
		t.Errorf("no debe emitirse ningún refresh token (hay %d)", got)
	}
}
