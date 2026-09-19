package store

// Pruebas de integración para el webhook de NOWPayments — cubren el punto 4
// del pedido de correcciones: una notificación con firma inválida debe
// rechazarse SIN convertirse en un trabajo de recuperación. Usa la misma
// base de datos de PRUEBA que shop_db_test.go (setupShopTestDB) — nunca
// producción.

import (
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
