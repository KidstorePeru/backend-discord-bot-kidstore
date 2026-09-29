package store

// Pruebas de regresión de la revocación inmediata de sesiones: un JWT de
// acceso emitido ANTES de desactivar o autoeliminar una cuenta no debe
// seguir dando acceso a rutas de cliente (historial de pedidos,
// estadísticas, perfil, etc.) — antes solo se borraban los refresh tokens y
// el JWT ya emitido seguía funcionando hasta 1h más. Las rutas de prueba
// usan el middleware REAL (CustomerAuthMiddleware) y los handlers REALES.

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"KidStoreStore/src/db"
	"KidStoreStore/src/middleware"
	"KidStoreStore/src/types"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

const sessionTestSecret = "session-test-secret"

func customerRouter(database *sql.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/store")
	g.Use(middleware.CustomerAuthMiddleware(database, sessionTestSecret))
	g.GET("/me", HandlerMe(database))
	g.GET("/orders", HandlerGetMyOrders(database))
	g.GET("/orders/stats", HandlerGetMyOrderStats(database))
	g.DELETE("/account", HandlerDeleteOwnAccount(database))
	return r
}

func tokenFor(t *testing.T, id uuid.UUID) string {
	t.Helper()
	tok, err := middleware.GenerateCustomerToken(types.Customer{ID: id, EpicUsername: "sessiontest"}, sessionTestSecret)
	if err != nil {
		t.Fatalf("GenerateCustomerToken: %v", err)
	}
	return tok
}

func doGet(r *gin.Engine, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

var customerRoutes = []string{"/store/orders", "/store/orders/stats", "/store/me"}

func TestCustomerAuthMiddleware_TokenPrevioAlaDesactivacionYaNoFunciona(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	router := customerRouter(conn)
	token := tokenFor(t, custID)

	for _, path := range customerRoutes {
		if w := doGet(router, path, token); w.Code != http.StatusOK {
			t.Fatalf("%s: con la cuenta activa el token debe funcionar, obtuve %d: %s", path, w.Code, w.Body.String())
		}
	}

	if err := db.DeactivateCustomerByAdmin(conn, custID); err != nil {
		t.Fatalf("DeactivateCustomerByAdmin: %v", err)
	}

	for _, path := range customerRoutes {
		w := doGet(router, path, token)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: el JWT emitido antes de la desactivación debe dar 401, obtuve %d: %s", path, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "ACCOUNT_INACTIVE") {
			t.Errorf("%s: la respuesta debe indicar ACCOUNT_INACTIVE: %s", path, w.Body.String())
		}
	}

	// Reactivar la cuenta (p. ej. un admin la habilita de nuevo) devuelve el
	// acceso: la comprobación refleja el estado ACTUAL, no un flag en el token.
	if _, err := conn.Exec(`UPDATE customers SET is_active=true WHERE id=$1`, custID); err != nil {
		t.Fatalf("reactivar: %v", err)
	}
	if w := doGet(router, "/store/orders", token); w.Code != http.StatusOK {
		t.Errorf("tras reactivar la cuenta el mismo token debe volver a funcionar, obtuve %d", w.Code)
	}
}

func TestCustomerAuthMiddleware_TokenPrevioALaAutoeliminacionYaNoFunciona(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()

	hash, err := bcrypt.GenerateFromPassword([]byte("clave-de-prueba-123"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	if _, err := conn.Exec(`UPDATE customers SET password_hash=$1, has_password=true WHERE id=$2`, string(hash), custID); err != nil {
		t.Fatalf("preparar contraseña: %v", err)
	}

	router := customerRouter(conn)
	token := tokenFor(t, custID)
	if w := doGet(router, "/store/orders", token); w.Code != http.StatusOK {
		t.Fatalf("con la cuenta activa el token debe funcionar, obtuve %d", w.Code)
	}

	// Autoeliminación real por HTTP (mismo handler que producción).
	req := httptest.NewRequest("DELETE", "/store/account", strings.NewReader(`{"password":"clave-de-prueba-123"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("la autoeliminación debía completarse, obtuve %d: %s", w.Code, w.Body.String())
	}

	for _, path := range customerRoutes {
		if w := doGet(router, path, token); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: el JWT previo a la autoeliminación debe dar 401, obtuve %d: %s", path, w.Code, w.Body.String())
		}
	}
	// Ni siquiera un segundo intento de eliminar la cuenta con ese token.
	req2 := httptest.NewRequest("DELETE", "/store/account", strings.NewReader(`{"password":"clave-de-prueba-123"}`))
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("repetir la eliminación con el token previo debe dar 401, obtuve %d", w2.Code)
	}
}

func TestCustomerAuthMiddleware_CuentaInexistenteYTokenInvalido(t *testing.T) {
	conn := setupShopTestDB(t)
	router := customerRouter(conn)

	if w := doGet(router, "/store/orders", tokenFor(t, uuid.New())); w.Code != http.StatusUnauthorized {
		t.Errorf("un customer_id que no existe debe dar 401, obtuve %d", w.Code)
	}
	if w := doGet(router, "/store/orders", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("sin token debe dar 401, obtuve %d", w.Code)
	}
	if w := doGet(router, "/store/orders", "esto.no.es.un.jwt"); w.Code != http.StatusUnauthorized {
		t.Errorf("token basura debe dar 401, obtuve %d", w.Code)
	}
}

// TestCustomerAuthMiddleware_FallaCerradoSiLaBaseDeDatosFalla: si no se puede
// confirmar que la cuenta sigue activa, la petición NUNCA pasa (ni llega al
// handler) — no se asume "activa" por defecto.
func TestCustomerAuthMiddleware_FallaCerradoSiLaBaseDeDatosFalla(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	token := tokenFor(t, custID)

	broken, err := sql.Open("postgres", "host=127.0.0.1 port=1 sslmode=disable connect_timeout=1")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer broken.Close()

	reached := false
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/store")
	g.Use(middleware.CustomerAuthMiddleware(broken, sessionTestSecret))
	g.GET("/orders", func(c *gin.Context) { reached = true; c.Status(http.StatusOK) })

	w := doGet(r, "/store/orders", token)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("con la base caída debe fallar cerrado con 503, obtuve %d", w.Code)
	}
	if reached {
		t.Error("el handler NUNCA debió ejecutarse si no se pudo verificar la cuenta")
	}
}
