package store

// Regresiones: cambiar o restablecer la contraseña revoca TODAS las sesiones de
// forma atómica con la actualización, incluso frente a renovaciones de token
// concurrentes; los errores de base de datos se manejan (rollback + 500) y no
// se pierde ni el token de restablecimiento ni la contraseña anterior.

import (
	"database/sql"
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

func passwordRouter(database *sql.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/store/refresh", HandlerRefreshToken(database, sessionTestSecret))
	r.POST("/store/reset-password", HandlerResetPassword(database))
	g := r.Group("/store")
	g.Use(middleware.CustomerAuthMiddleware(database, sessionTestSecret))
	g.PUT("/profile", HandlerUpdateProfile(database, sessionTestSecret))
	return r
}

func putJSON(r *gin.Engine, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func setPassword(t *testing.T, conn *sql.DB, id uuid.UUID, plain string) string {
	t.Helper()
	h, _ := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.MinCost)
	if _, err := conn.Exec(`UPDATE customers SET password_hash=$1, has_password=true WHERE id=$2`, string(h), id); err != nil {
		t.Fatalf("fijar contraseña: %v", err)
	}
	return string(h)
}

func passwordHashOf(t *testing.T, conn *sql.DB, id uuid.UUID) string {
	t.Helper()
	var h string
	if err := conn.QueryRow(`SELECT password_hash FROM customers WHERE id=$1`, id).Scan(&h); err != nil {
		t.Fatal(err)
	}
	return h
}

// blockRefreshTokenDeletes instala un trigger de PRUEBA (solo para este
// cliente) que hace fallar cualquier DELETE de sus refresh tokens.
func blockRefreshTokenDeletes(t *testing.T, conn *sql.DB, custID uuid.UUID) (unblock func()) {
	t.Helper()
	fn := "kc_test_block_del_" + strings.ReplaceAll(custID.String()[:8], "-", "")
	if _, err := conn.Exec(`CREATE OR REPLACE FUNCTION ` + fn + `() RETURNS trigger AS $$ BEGIN
		IF OLD.customer_id = '` + custID.String() + `' THEN RAISE EXCEPTION 'fallo simulado'; END IF;
		RETURN OLD; END; $$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("crear función: %v", err)
	}
	if _, err := conn.Exec(`CREATE TRIGGER ` + fn + ` BEFORE DELETE ON refresh_tokens FOR EACH ROW EXECUTE FUNCTION ` + fn + `()`); err != nil {
		t.Fatalf("crear trigger: %v", err)
	}
	done := false
	unblock = func() {
		if done {
			return
		}
		done = true
		conn.Exec(`DROP TRIGGER IF EXISTS ` + fn + ` ON refresh_tokens`)
		conn.Exec(`DROP FUNCTION IF EXISTS ` + fn + `()`)
	}
	t.Cleanup(unblock)
	return unblock
}

func TestUpdateProfile_CambioDeContrasenaRevocaSesionesEnLaMismaTransaccion(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	defer cleanupTokens(conn, custID)
	router := passwordRouter(conn)
	setPassword(t, conn, custID, "clave-actual-123")
	issueRefreshToken(t, conn, custID)
	issueRefreshToken(t, conn, custID)

	w := putJSON(router, "/store/profile", tokenFor(t, custID), `{"current_password":"clave-actual-123","new_password":"clave-nueva-456"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("esperaba 200, obtuve %d: %s", w.Code, w.Body.String())
	}
	if got := refreshTokenCount(t, conn, custID); got != 0 {
		t.Errorf("todas las sesiones deben revocarse, quedan %d", got)
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHashOf(t, conn, custID)), []byte("clave-nueva-456")) != nil {
		t.Error("la contraseña nueva debe haberse guardado")
	}
}

// Si la revocación falla, NO se cambia la contraseña (rollback) y se responde 500:
// nunca queda la contraseña nueva con sesiones viejas vivas.
func TestUpdateProfile_FalloAlRevocarRevierteLaContrasenaYDa500(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	defer cleanupTokens(conn, custID)
	router := passwordRouter(conn)
	oldHash := setPassword(t, conn, custID, "clave-actual-123")
	issueRefreshToken(t, conn, custID)
	blockRefreshTokenDeletes(t, conn, custID)

	w := putJSON(router, "/store/profile", tokenFor(t, custID), `{"current_password":"clave-actual-123","new_password":"clave-nueva-456"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("esperaba 500, obtuve %d: %s", w.Code, w.Body.String())
	}
	if passwordHashOf(t, conn, custID) != oldHash {
		t.Error("la contraseña no debe cambiar si la revocación de sesiones falla")
	}
	if got := refreshTokenCount(t, conn, custID); got != 1 {
		t.Errorf("la sesión existente se conserva tras el rollback, hay %d", got)
	}
}

// Refresh en curso (con el candado de la cuenta tomado) + cambio de contraseña:
// el cambio espera, y al terminar el token emitido por ese refresh también queda revocado.
func TestUpdateProfile_CambioDeContrasenaConRefreshEnCursoNoDejaSesionesVivas(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	defer cleanupTokens(conn, custID)
	router := passwordRouter(conn)
	setPassword(t, conn, custID, "clave-actual-123")
	plain := issueRefreshToken(t, conn, custID)

	locked, release := make(chan struct{}), make(chan struct{})
	prev := refreshLockCustomer
	refreshLockCustomer = func(tx *sql.Tx, id uuid.UUID) (bool, error) {
		a, err := db.LockCustomerActive(tx, id)
		close(locked)
		<-release
		return a, err
	}
	defer func() { refreshLockCustomer = prev }()

	refreshRes := make(chan int, 1)
	go func() { refreshRes <- postJSON(router, "/store/refresh", `{"refresh_token":"`+plain+`"}`).Code }()
	<-locked

	changeRes := make(chan int, 1)
	go func() {
		changeRes <- putJSON(router, "/store/profile", tokenFor(t, custID), `{"current_password":"clave-actual-123","new_password":"clave-nueva-456"}`).Code
	}()
	select {
	case code := <-changeRes:
		t.Fatalf("el cambio de contraseña debe esperar al refresh en curso (respondió %d)", code)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if code := <-refreshRes; code != http.StatusOK {
		t.Fatalf("el refresh en curso se completa, obtuve %d", code)
	}
	select {
	case code := <-changeRes:
		if code != http.StatusOK {
			t.Fatalf("cambio de contraseña: %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("el cambio de contraseña quedó bloqueado (¿deadlock?)")
	}
	if got := refreshTokenCount(t, conn, custID); got != 0 {
		t.Errorf("el token emitido por el refresh debe quedar revocado, hay %d", got)
	}
}

// Aleatorio: refresh y cambio de contraseña en paralelo, sin deadlock y sin sesiones vivas.
func TestUpdateProfile_ConcurrenteConRefreshNuncaDejaSesionesVivas(t *testing.T) {
	conn := setupShopTestDB(t)
	router := passwordRouter(conn)
	for i := 0; i < 25; i++ {
		custID, cleanup := newShopTestCustomer(t, conn, 0)
		setPassword(t, conn, custID, "clave-actual-123")
		plain := issueRefreshToken(t, conn, custID)
		jwt := tokenFor(t, custID)

		var wg sync.WaitGroup
		start := make(chan struct{})
		var refreshCode, changeCode int
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			refreshCode = postJSON(router, "/store/refresh", `{"refresh_token":"`+plain+`"}`).Code
		}()
		go func() {
			defer wg.Done()
			<-start
			changeCode = putJSON(router, "/store/profile", jwt, `{"current_password":"clave-actual-123","new_password":"clave-nueva-456"}`).Code
		}()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		close(start)
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("posible deadlock entre refresh y cambio de contraseña")
		}
		if changeCode != http.StatusOK {
			t.Errorf("cambio de contraseña: %d", changeCode)
		}
		if refreshCode != http.StatusOK && refreshCode != http.StatusUnauthorized {
			t.Errorf("refresh: código inesperado %d", refreshCode)
		}
		if got := refreshTokenCount(t, conn, custID); got != 0 {
			t.Errorf("(refresh=%d) no debe sobrevivir ninguna sesión anterior, hay %d", refreshCode, got)
		}
		cleanupTokens(conn, custID)
		cleanup()
	}
}

func newResetToken(t *testing.T, conn *sql.DB, custID uuid.UUID) string {
	t.Helper()
	tok := strings.ReplaceAll(uuid.New().String(), "-", "") + strings.ReplaceAll(uuid.New().String(), "-", "")
	if err := db.CreatePasswordResetToken(conn, custID, tok); err != nil {
		t.Fatalf("token de restablecimiento: %v", err)
	}
	t.Cleanup(func() { conn.Exec(`DELETE FROM password_reset_tokens WHERE customer_id=$1`, custID) })
	return tok
}

func TestResetPassword_RevocaSesionesYConsumeElTokenUnaSolaVez(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	defer cleanupTokens(conn, custID)
	router := passwordRouter(conn)
	issueRefreshToken(t, conn, custID)
	issueRefreshToken(t, conn, custID)
	tok := newResetToken(t, conn, custID)

	// Varios usos simultáneos del MISMO token: solo uno gana.
	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			codes <- postJSON(router, "/store/reset-password", `{"token":"`+tok+`","password":"clave-nueva-456"}`).Code
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	ok, bad := 0, 0
	for c := range codes {
		if c == http.StatusOK {
			ok++
		} else if c == http.StatusBadRequest {
			bad++
		} else {
			t.Errorf("código inesperado %d", c)
		}
	}
	if ok != 1 || bad != n-1 {
		t.Fatalf("el token debe consumirse una sola vez: ok=%d rechazados=%d", ok, bad)
	}
	if got := refreshTokenCount(t, conn, custID); got != 0 {
		t.Errorf("todas las sesiones deben revocarse, quedan %d", got)
	}
	if bcrypt.CompareHashAndPassword([]byte(passwordHashOf(t, conn, custID)), []byte("clave-nueva-456")) != nil {
		t.Error("la contraseña nueva debe haberse guardado")
	}
}

func TestResetPassword_FalloAlRevocarRevierteTodoYConservaElToken(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	defer cleanupTokens(conn, custID)
	router := passwordRouter(conn)
	oldHash := setPassword(t, conn, custID, "clave-actual-123")
	issueRefreshToken(t, conn, custID)
	tok := newResetToken(t, conn, custID)
	unblock := blockRefreshTokenDeletes(t, conn, custID)

	w := postJSON(router, "/store/reset-password", `{"token":"`+tok+`","password":"clave-nueva-456"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("esperaba 500, obtuve %d: %s", w.Code, w.Body.String())
	}
	if passwordHashOf(t, conn, custID) != oldHash {
		t.Error("la contraseña no debe cambiar si falla la revocación")
	}
	// El token NO se quemó: recuperada la base, el mismo enlace sigue sirviendo.
	unblock()
	if w := postJSON(router, "/store/reset-password", `{"token":"`+tok+`","password":"clave-nueva-456"}`); w.Code != http.StatusOK {
		t.Fatalf("tras recuperarse, el token debe seguir vigente: %d %s", w.Code, w.Body.String())
	}
	if got := refreshTokenCount(t, conn, custID); got != 0 {
		t.Errorf("sesiones vivas tras el restablecimiento: %d", got)
	}
}

// Restablecimiento concurrente con un refresh: sin deadlock y sin sesiones vivas.
func TestResetPassword_ConcurrenteConRefreshNuncaDejaSesionesVivas(t *testing.T) {
	conn := setupShopTestDB(t)
	router := passwordRouter(conn)
	for i := 0; i < 25; i++ {
		custID, cleanup := newShopTestCustomer(t, conn, 0)
		plain := issueRefreshToken(t, conn, custID)
		tok := newResetToken(t, conn, custID)

		var wg sync.WaitGroup
		start := make(chan struct{})
		var refreshCode, resetCode int
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			refreshCode = postJSON(router, "/store/refresh", `{"refresh_token":"`+plain+`"}`).Code
		}()
		go func() {
			defer wg.Done()
			<-start
			resetCode = postJSON(router, "/store/reset-password", `{"token":"`+tok+`","password":"clave-nueva-456"}`).Code
		}()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		close(start)
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("posible deadlock entre refresh y restablecimiento")
		}
		if resetCode != http.StatusOK {
			t.Errorf("restablecimiento: %d", resetCode)
		}
		if got := refreshTokenCount(t, conn, custID); got != 0 {
			t.Errorf("(refresh=%d) no debe sobrevivir ninguna sesión anterior, hay %d", refreshCode, got)
		}
		cleanupTokens(conn, custID)
		cleanup()
	}
}
