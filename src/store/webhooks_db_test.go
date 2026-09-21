package store

// Pruebas de integración para el webhook de NOWPayments — cubren el punto 4
// del pedido de correcciones: una notificación con firma inválida debe
// rechazarse SIN convertirse en un trabajo de recuperación. Usa la misma
// base de datos de PRUEBA que shop_db_test.go (setupShopTestDB) — nunca
// producción.

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"KidStoreStore/src/db"

	"github.com/gin-gonic/gin"
)

// TestHandlerNOWPaymentsWebhook_FirmaInvalidaNuncaSeConvierteEnTrabajoDeRecuperacion
// simula un POST forjado (sin la firma real de NOWPayments) contra el
// webhook. Antes de esta corrección no existía ninguna validación de firma:
// cualquiera podía mandar un payment_id ajeno o inventado y el evento se
// registraba igual que uno legítimo, quedando disponible para que
// RetryFailedWebhookEvents lo reprocesara más tarde sin volver a exigir
// firma. Ahora debe: (1) responder con un error (nunca 200 fingiendo
// aceptación silenciosa), (2) dejar registrado el intento con un outcome
// que NO empiece con "error:" (para que GetUnresolvedWebhookEvents lo
// excluya para siempre), y (3) un RetryFailedWebhookEvents posterior no debe
// ni siquiera intentar consultar la pasarela por ese payment_id.
func TestHandlerNOWPaymentsWebhook_FirmaInvalidaNuncaSeConvierteEnTrabajoDeRecuperacion(t *testing.T) {
	conn := setupShopTestDB(t)

	prevCfg := paymentCfg
	paymentCfg.NOWPaymentsIPNSecret = "test-ipn-secret"
	defer func() { paymentCfg = prevCfg }()

	// La pasarela simulada NUNCA debería recibir ninguna consulta en esta
	// prueba — si eso pasara, significaría que el evento con firma inválida
	// se coló en el flujo de procesamiento/reintento.
	queried := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queried = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"payment_status":"finished","order_id":"00000000-0000-0000-0000-000000000000"}`)
	}))
	defer server.Close()
	prevURL := nowPaymentsBaseURL
	nowPaymentsBaseURL = server.URL
	defer func() { nowPaymentsBaseURL = prevURL }()

	const rawBody = `{"payment_id":999888777,"payment_status":"finished","order_id":"00000000-0000-0000-0000-000000000000"}`

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/store/webhook/nowpayments", HandlerNOWPaymentsWebhook(conn))
	req := httptest.NewRequest("POST", "/store/webhook/nowpayments", strings.NewReader(rawBody))
	req.Header.Set("x-nowpayments-sig", "0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Errorf("una notificación con firma inválida no debería responder 200 fingiendo que se aceptó, obtuve %d: %s", w.Code, w.Body.String())
	}

	var eventID string
	var outcome, rawBodyStored string
	err := conn.QueryRow(`SELECT id, outcome, raw_body FROM webhook_events WHERE gateway='nowpayments' AND raw_body=$1 ORDER BY received_at DESC LIMIT 1`, rawBody).
		Scan(&eventID, &outcome, &rawBodyStored)
	if err != nil {
		t.Fatalf("el intento debería quedar registrado en webhook_events para auditoría: %v", err)
	}
	defer conn.Exec(`DELETE FROM webhook_events WHERE id=$1`, eventID)

	if strings.HasPrefix(outcome, "error:") {
		t.Errorf("el outcome de una firma inválida NUNCA debe empezar con 'error:' — eso es justo lo que GetUnresolvedWebhookEvents usa para decidir qué reintentar, obtuve %q", outcome)
	}
	if outcome == "processed" {
		t.Errorf("una notificación con firma inválida nunca debe terminar marcada como 'processed'")
	}

	// El barrido de recuperación (mismo que corre periódicamente desde
	// main.go) no debe ni siquiera intentar consultar la pasarela para este
	// evento.
	RetryFailedWebhookEvents(conn)
	if queried {
		t.Error("RetryFailedWebhookEvents NUNCA debió consultar la pasarela para un evento con firma inválida — se coló como trabajo de recuperación")
	}

	unresolved, err := db.GetUnresolvedWebhookEvents(conn, "nowpayments", 0)
	if err != nil {
		t.Fatalf("GetUnresolvedWebhookEvents: %v", err)
	}
	for _, ev := range unresolved {
		if ev.ID.String() == eventID {
			t.Error("el evento con firma inválida sigue apareciendo como 'sin resolver' — quedaría disponible para reintentarse indefinidamente")
		}
	}
}

// TestHandlerNOWPaymentsWebhook_FirmaInvalidaConFalloDeEscritura_NuncaConsultaAlProveedor
// cubre el punto 2 del pedido de correcciones: antes, el rechazo por firma
// inválida se registraba con DOS escrituras separadas (LogWebhookEvent +
// MarkWebhookEventProcessed) — si la segunda fallaba, o el proceso se caía
// justo entre medio, el evento quedaba con processed_at NULL, la misma
// condición que usa GetUnresolvedWebhookEvents para decidir qué reintentar.
// Acá se simula justo esa falla de escritura (con una conexión a la base de
// datos inalcanzable) y se comprueba que, aun así, la pasarela NUNCA se
// consulta — ni en la misma request, ni en un barrido de recuperación
// posterior — porque con la escritura atómica (LogRejectedWebhookEvent) un
// fallo no deja ninguna fila a medio resolver: no queda ninguna fila en
// absoluto.
func TestHandlerNOWPaymentsWebhook_FirmaInvalidaConFalloDeEscritura_NuncaConsultaAlProveedor(t *testing.T) {
	conn := setupShopTestDB(t)

	prevCfg := paymentCfg
	paymentCfg.NOWPaymentsIPNSecret = "test-ipn-secret"
	defer func() { paymentCfg = prevCfg }()

	queried := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queried = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"payment_status":"finished","order_id":"00000000-0000-0000-0000-000000000000"}`)
	}))
	defer server.Close()
	prevURL := nowPaymentsBaseURL
	nowPaymentsBaseURL = server.URL
	defer func() { nowPaymentsBaseURL = prevURL }()

	// Conexión rota a propósito — cualquier consulta contra ella falla (puerto
	// reservado, nadie escucha ahí). sql.Open no conecta de inmediato (es
	// perezoso), así que esto recién falla en el primer intento de uso real,
	// igual que pasaría con un problema real de red/base de datos.
	brokenDB, err := sql.Open("postgres", "host=127.0.0.1 port=1 sslmode=disable connect_timeout=1")
	if err != nil {
		t.Fatalf("sql.Open no debería fallar acá (es perezoso, no conecta todavía): %v", err)
	}
	defer brokenDB.Close()

	const rawBody = `{"payment_id":555444333,"payment_status":"finished","order_id":"00000000-0000-0000-0000-000000000000"}`

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/store/webhook/nowpayments", HandlerNOWPaymentsWebhook(brokenDB))
	req := httptest.NewRequest("POST", "/store/webhook/nowpayments", strings.NewReader(rawBody))
	req.Header.Set("x-nowpayments-sig", "0000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Errorf("una notificación con firma inválida no debería responder 200, obtuve %d: %s", w.Code, w.Body.String())
	}
	if queried {
		t.Fatal("la pasarela NUNCA debió consultarse al manejar una firma inválida, ni siquiera si la escritura del rechazo falla")
	}

	// Con la conexión REAL de pruebas: no debe haber quedado ninguna fila
	// para este evento — la escritura atómica falló entera, así que no hay
	// ni una fila a medio resolver ni una resuelta.
	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM webhook_events WHERE gateway='nowpayments' AND raw_body=$1`, rawBody).Scan(&count); err != nil {
		t.Fatalf("no se pudo verificar webhook_events: %v", err)
	}
	if count != 0 {
		t.Errorf("no debería haber quedado ninguna fila para este evento tras el fallo de escritura, encontré %d", count)
		conn.Exec(`DELETE FROM webhook_events WHERE gateway='nowpayments' AND raw_body=$1`, rawBody)
	}

	// Por las dudas: un barrido de recuperación posterior tampoco debe
	// encontrar (ni mucho menos consultar) nada para este evento.
	RetryFailedWebhookEvents(conn)
	if queried {
		t.Error("RetryFailedWebhookEvents NUNCA debió consultar la pasarela — no había ningún evento registrado para este payment_id")
	}
}
