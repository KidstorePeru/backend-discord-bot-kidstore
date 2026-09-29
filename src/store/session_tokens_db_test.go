package store

// Regresiones de sesiones y cuentas inactivas:
//   - un refresh token se consume de forma ATÓMICA (dos refresh simultáneos
//     con el mismo token no pueden rotarlo los dos);
//   - refresh y login de una cuenta inactiva no emiten JWT ni refresh tokens;
//   - (el JWT anterior a la desactivación ya se cubre en
//     session_revocation_db_test.go).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"KidStoreStore/src/db"
	"KidStoreStore/src/middleware"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

func authTokensRouter(database *sql.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/store/refresh", HandlerRefreshToken(database, sessionTestSecret))
	r.POST("/store/login", HandlerLogin(database, sessionTestSecret))
	return r
}

func postJSON(r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func issueRefreshToken(t *testing.T, conn *sql.DB, custID uuid.UUID) string {
	t.Helper()
	plain, hash, err := middleware.GenerateRefreshToken()
	if err != nil {
		t.Fatalf("GenerateRefreshToken: %v", err)
	}
	if err := db.CreateRefreshToken(conn, custID, hash, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateRefreshToken: %v", err)
	}
	return plain
}

func refreshTokenCount(t *testing.T, conn *sql.DB, custID uuid.UUID) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM refresh_tokens WHERE customer_id=$1`, custID).Scan(&n); err != nil {
		t.Fatalf("contar refresh tokens: %v", err)
	}
	return n
}

func cleanupTokens(conn *sql.DB, custID uuid.UUID) { conn.Exec(`DELETE FROM refresh_tokens WHERE customer_id=$1`, custID) }

func TestConsumeRefreshToken_SoloUnaSolicitudConcurrenteLoConsume(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	defer cleanupTokens(conn, custID)

	plain := issueRefreshToken(t, conn, custID)
	hash := middleware.HashRefreshToken(plain)

	const n = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := db.ConsumeRefreshToken(conn, hash)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if err != sql.ErrNoRows {
			t.Errorf("error inesperado: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("exactamente UNA solicitud debe consumir el token, lo consumieron %d", wins)
	}
}

func TestHandlerRefreshToken_DosRefreshSimultaneosNoRotanElMismoToken(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	defer cleanupTokens(conn, custID)
	router := authTokensRouter(conn)

	plain := issueRefreshToken(t, conn, custID)
	body := `{"refresh_token":"` + plain + `"}`

	const n = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	codes := make(chan int, n)
	newTokens := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			w := postJSON(router, "/store/refresh", body)
			codes <- w.Code
			if w.Code == http.StatusOK {
				var resp struct {
					RefreshToken string `json:"refresh_token"`
				}
				json.Unmarshal(w.Body.Bytes(), &resp)
				newTokens <- resp.RefreshToken
			}
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	close(newTokens)

	ok, denied := 0, 0
	for c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusUnauthorized:
			denied++
		default:
			t.Errorf("código inesperado %d", c)
		}
	}
	if ok != 1 || denied != n-1 {
		t.Fatalf("solo UNA rotación debe tener éxito: ok=%d denegadas=%d de %d", ok, denied, n)
	}
	if got := refreshTokenCount(t, conn, custID); got != 1 {
		t.Errorf("debe quedar exactamente 1 refresh token (el nuevo), hay %d", got)
	}
	// El token original ya no sirve.
	if w := postJSON(router, "/store/refresh", body); w.Code != http.StatusUnauthorized {
		t.Errorf("reusar el token ya rotado debe dar 401, obtuve %d", w.Code)
	}
}

func TestHandlerRefreshToken_CuentaInactivaNoRecibeTokens(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	defer cleanupTokens(conn, custID)
	router := authTokensRouter(conn)

	// Refresh token vigente de una cuenta que se desactivó sin que se borraran
	// sus tokens (p. ej. un UPDATE directo, o un camino futuro que olvide
	// revocarlos): igual no debe obtener JWT ni un refresh nuevo.
	plain := issueRefreshToken(t, conn, custID)
	if _, err := conn.Exec(`UPDATE customers SET is_active=false WHERE id=$1`, custID); err != nil {
		t.Fatalf("desactivar: %v", err)
	}

	w := postJSON(router, "/store/refresh", `{"refresh_token":"`+plain+`"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("esperaba 401, obtuve %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"token"`) || strings.Contains(w.Body.String(), "refresh_token\":\"") {
		t.Errorf("la respuesta no debe traer ningún token: %s", w.Body.String())
	}
	if got := refreshTokenCount(t, conn, custID); got != 0 {
		t.Errorf("no debe quedar ni emitirse ningún refresh token para una cuenta inactiva, hay %d", got)
	}
}

func TestHandlerLogin_CuentaInactivaNoRecibeTokens(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	defer cleanupTokens(conn, custID)
	router := authTokensRouter(conn)

	hash, _ := bcrypt.GenerateFromPassword([]byte("clave-correcta-123"), bcrypt.MinCost)
	var email string
	if err := conn.QueryRow(`UPDATE customers SET password_hash=$1, has_password=true, is_verified=true WHERE id=$2 RETURNING email`, string(hash), custID).Scan(&email); err != nil {
		t.Fatalf("preparar cuenta: %v", err)
	}
	loginBody := `{"email":"` + email + `","password":"clave-correcta-123"}`

	// Control: activa, el login funciona.
	if w := postJSON(router, "/store/login", loginBody); w.Code != http.StatusOK {
		t.Fatalf("con la cuenta activa el login debe funcionar, obtuve %d: %s", w.Code, w.Body.String())
	}
	cleanupTokens(conn, custID)

	if _, err := conn.Exec(`UPDATE customers SET is_active=false WHERE id=$1`, custID); err != nil {
		t.Fatalf("desactivar: %v", err)
	}
	w := postJSON(router, "/store/login", loginBody)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("una cuenta inactiva con la contraseña CORRECTA no debe iniciar sesión (401), obtuve %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"token"`) || strings.Contains(w.Body.String(), "refresh_token") {
		t.Errorf("no debe emitirse ningún token: %s", w.Body.String())
	}
	if got := refreshTokenCount(t, conn, custID); got != 0 {
		t.Errorf("no debe crearse ningún refresh token, hay %d", got)
	}
}

// Si el nuevo refresh token no se puede persistir, NO se responde con éxito y
// el consumo del anterior se revierte (misma transacción): el usuario conserva
// su sesión renovable y puede reintentar.
func TestHandlerRefreshToken_FalloAlPersistirElNuevoTokenNoRespondeExitoYConservaElAnterior(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	defer cleanupTokens(conn, custID)
	router := authTokensRouter(conn)

	plain := issueRefreshToken(t, conn, custID)
	body := `{"refresh_token":"` + plain + `"}`

	// Trigger de PRUEBA (solo para este cliente): cualquier INSERT de un refresh
	// token de este cliente falla, simulando una caída de la escritura.
	fn := "kc_test_fail_rt_" + strings.ReplaceAll(custID.String()[:8], "-", "")
	if _, err := conn.Exec(`CREATE OR REPLACE FUNCTION ` + fn + `() RETURNS trigger AS $$ BEGIN
		IF NEW.customer_id = '` + custID.String() + `' THEN RAISE EXCEPTION 'fallo simulado'; END IF;
		RETURN NEW; END; $$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("crear función trigger: %v", err)
	}
	if _, err := conn.Exec(`CREATE TRIGGER ` + fn + ` BEFORE INSERT ON refresh_tokens FOR EACH ROW EXECUTE FUNCTION ` + fn + `()`); err != nil {
		t.Fatalf("crear trigger: %v", err)
	}
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		conn.Exec(`DROP TRIGGER IF EXISTS ` + fn + ` ON refresh_tokens`)
		conn.Exec(`DROP FUNCTION IF EXISTS ` + fn + `()`)
	}
	defer drop()

	w := postJSON(router, "/store/refresh", body)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("si el token nuevo no se persiste debe responder 500, obtuve %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"token"`) || strings.Contains(w.Body.String(), "refresh_token\":\"") {
		t.Errorf("no debe entregarse ningún token que no quedó persistido: %s", w.Body.String())
	}
	if got := refreshTokenCount(t, conn, custID); got != 1 {
		t.Fatalf("el token anterior debe conservarse (rollback), hay %d tokens", got)
	}

	// Restablecida la escritura, el MISMO token anterior sigue sirviendo.
	drop()
	if w := postJSON(router, "/store/refresh", body); w.Code != http.StatusOK {
		t.Fatalf("tras recuperarse la escritura el refresh debe funcionar, obtuve %d: %s", w.Code, w.Body.String())
	}
	if got := refreshTokenCount(t, conn, custID); got != 1 {
		t.Errorf("debe quedar exactamente 1 token (el nuevo), hay %d", got)
	}
}

// Un error OPERATIVO al comprobar la cuenta NO es "cuenta inactiva": se revierte
// y se responde un error temporal (503) SIN consumir el refresh token.
func TestHandlerRefreshToken_ErrorOperativoAlConsultarLaCuentaNoConsumeElToken(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	defer cleanupTokens(conn, custID)
	router := authTokensRouter(conn)

	plain := issueRefreshToken(t, conn, custID)
	body := `{"refresh_token":"` + plain + `"}`

	prev := refreshLockCustomer
	refreshLockCustomer = func(tx *sql.Tx, id uuid.UUID) (bool, error) { return false, errors.New("fallo simulado de la base de datos") }
	w := postJSON(router, "/store/refresh", body)
	refreshLockCustomer = prev

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("un error operativo debe dar 503 (temporal), obtuve %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "ACCOUNT_INACTIVE") || strings.Contains(w.Body.String(), `"token"`) {
		t.Errorf("no debe presentarse como cuenta inactiva ni entregar tokens: %s", w.Body.String())
	}
	if got := refreshTokenCount(t, conn, custID); got != 1 {
		t.Fatalf("el refresh token no debe consumirse ante un error operativo (hay %d)", got)
	}
	// Sin el fallo, el mismo token sigue funcionando.
	if w := postJSON(router, "/store/refresh", body); w.Code != http.StatusOK {
		t.Fatalf("tras recuperarse la base de datos el refresh debe funcionar, obtuve %d: %s", w.Code, w.Body.String())
	}
}

// sql.ErrNoRows (cuenta inexistente) sí es "no autorizado" y revoca el token.
func TestHandlerRefreshToken_CuentaInexistenteDa401YRevocaElToken(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	defer cleanupTokens(conn, custID)
	router := authTokensRouter(conn)

	plain := issueRefreshToken(t, conn, custID)
	prev := refreshLockCustomer
	refreshLockCustomer = func(tx *sql.Tx, id uuid.UUID) (bool, error) { return false, sql.ErrNoRows }
	w := postJSON(router, "/store/refresh", `{"refresh_token":"`+plain+`"}`)
	refreshLockCustomer = prev

	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "ACCOUNT_INACTIVE") {
		t.Fatalf("esperaba 401 ACCOUNT_INACTIVE, obtuve %d: %s", w.Code, w.Body.String())
	}
	if got := refreshTokenCount(t, conn, custID); got != 0 {
		t.Errorf("el token de una cuenta inexistente debe consumirse (hay %d)", got)
	}
}

// Refresh concurrente con la desactivación (admin o autoeliminación): sin
// deadlocks y, pase lo que pase, al terminar NO queda ningún refresh token
// vivo — ni el viejo ni uno emitido justo antes/después de revocar.
func TestHandlerRefreshToken_ConcurrenteConDesactivacionNuncaDejaTokensVivos(t *testing.T) {
	conn := setupShopTestDB(t)
	router := authTokensRouter(conn)

	deactivators := map[string]func(*sql.DB, uuid.UUID) error{
		"DeactivateCustomerByAdmin": db.DeactivateCustomerByAdmin,
		"DeleteOwnAccount":          db.DeleteOwnAccount,
	}
	for name, deactivate := range deactivators {
		wins, losses := 0, 0
		for i := 0; i < 25; i++ {
			custID, cleanup := newShopTestCustomer(t, conn, 0)
			plain := issueRefreshToken(t, conn, custID)
			body := `{"refresh_token":"` + plain + `"}`

			var wg sync.WaitGroup
			start := make(chan struct{})
			var code int
			var derr error
			wg.Add(2)
			go func() { defer wg.Done(); <-start; code = postJSON(router, "/store/refresh", body).Code }()
			go func() { defer wg.Done(); <-start; derr = deactivate(conn, custID) }()
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			close(start)
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatalf("%s: posible deadlock entre refresh y desactivación", name)
			}

			if derr != nil {
				t.Errorf("%s: la desactivación falló: %v", name, derr)
			}
			switch code {
			case http.StatusOK:
				wins++
			case http.StatusUnauthorized:
				losses++
			default:
				t.Errorf("%s: código inesperado del refresh: %d", name, code)
			}
			if got := refreshTokenCount(t, conn, custID); got != 0 {
				t.Errorf("%s (refresh=%d): tras desactivar no debe quedar ningún refresh token, hay %d", name, code, got)
			}
			cleanupTokens(conn, custID)
			cleanup()
		}
		t.Logf("%s: refresh ganó %d veces, perdió %d", name, wins, losses)
	}
}

// Preservación: un refresh normal sobre una cuenta activa sigue rotando y deja
// exactamente un token.
func TestHandlerRefreshToken_CuentaActivaConservaUnSoloTokenRotado(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	defer cleanupTokens(conn, custID)
	router := authTokensRouter(conn)

	plain := issueRefreshToken(t, conn, custID)
	w := postJSON(router, "/store/refresh", `{"refresh_token":"`+plain+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("esperaba 200, obtuve %d: %s", w.Code, w.Body.String())
	}
	if got := refreshTokenCount(t, conn, custID); got != 1 {
		t.Errorf("debe quedar exactamente 1 token (el nuevo), hay %d", got)
	}
}

// Entrelazados deterministas (la prueba aleatoria de arriba casi siempre gana la
// desactivación): se fuerza cada orden posible y se comprueba el resultado.
func TestHandlerRefreshToken_EntrelazadosDeterministasConDesactivacion(t *testing.T) {
	conn := setupShopTestDB(t)
	router := authTokensRouter(conn)

	t.Run("la desactivación toma la cuenta primero: el refresh espera, ve inactiva y no emite nada", func(t *testing.T) {
		custID, cleanup := newShopTestCustomer(t, conn, 0)
		defer cleanup()
		defer cleanupTokens(conn, custID)
		plain := issueRefreshToken(t, conn, custID)

		tx, err := conn.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.Exec(`UPDATE customers SET is_active=false WHERE id=$1`, custID); err != nil {
			t.Fatal(err)
		}
		res := make(chan int, 1)
		go func() { res <- postJSON(router, "/store/refresh", `{"refresh_token":"`+plain+`"}`).Code }()
		select {
		case code := <-res:
			t.Fatalf("el refresh debe esperar el candado de la cuenta, respondió %d", code)
		case <-time.After(300 * time.Millisecond):
		}
		if _, err := tx.Exec(`DELETE FROM refresh_tokens WHERE customer_id=$1`, custID); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		select {
		case code := <-res:
			// El token ya fue revocado por la desactivación: 401 en cualquier caso.
			if code != http.StatusUnauthorized {
				t.Errorf("esperaba 401, obtuve %d", code)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("el refresh quedó bloqueado")
		}
		if got := refreshTokenCount(t, conn, custID); got != 0 {
			t.Errorf("no debe quedar ningún token, hay %d", got)
		}
	})

	t.Run("el refresh toma la cuenta primero: la desactivación espera y revoca también el token nuevo", func(t *testing.T) {
		custID, cleanup := newShopTestCustomer(t, conn, 0)
		defer cleanup()
		defer cleanupTokens(conn, custID)
		plain := issueRefreshToken(t, conn, custID)

		locked := make(chan struct{})
		release := make(chan struct{})
		prev := refreshLockCustomer
		refreshLockCustomer = func(tx *sql.Tx, id uuid.UUID) (bool, error) {
			active, err := db.LockCustomerActive(tx, id)
			close(locked)
			<-release // el refresh sigue dentro de su transacción, con el candado tomado
			return active, err
		}
		defer func() { refreshLockCustomer = prev }()

		refreshRes := make(chan int, 1)
		go func() { refreshRes <- postJSON(router, "/store/refresh", `{"refresh_token":"`+plain+`"}`).Code }()
		<-locked

		deactRes := make(chan error, 1)
		go func() { deactRes <- db.DeactivateCustomerByAdmin(conn, custID) }()
		select {
		case err := <-deactRes:
			t.Fatalf("la desactivación debe esperar al refresh en curso (terminó con %v)", err)
		case <-time.After(300 * time.Millisecond):
		}
		close(release)
		if code := <-refreshRes; code != http.StatusOK {
			t.Fatalf("el refresh en curso debe completarse (200), obtuve %d", code)
		}
		select {
		case err := <-deactRes:
			if err != nil {
				t.Fatalf("desactivación: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("la desactivación quedó bloqueada (¿deadlock?)")
		}
		if got := refreshTokenCount(t, conn, custID); got != 0 {
			t.Errorf("el token emitido por el refresh debe quedar revocado por la desactivación, hay %d", got)
		}
	})
}
