package store

// Pruebas de la verificación criptográfica de webhooks de Mercado Pago y
// PayPal: firma válida, firma inválida, encabezados ausentes, y secreto /
// configuración ausente (rechazo en producción, advertencia en desarrollo).
// PayPal se verifica contra un httptest.Server que simula su endpoint oficial
// verify-webhook-signature — ninguna prueba llama a una pasarela real.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

const mpTestSecret = "mp-test-webhook-secret"

func mpSignature(secret, dataID, requestID, ts string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(mercadoPagoManifest(dataID, requestID, ts)))
	return "ts=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func withPaymentCfg(t *testing.T, mutate func(*PaymentConfig)) {
	t.Helper()
	prev := paymentCfg
	mutate(&paymentCfg)
	t.Cleanup(func() { paymentCfg = prev })
}

func mpRequest(query string, headers map[string]string) *http.Request {
	r := httptest.NewRequest("POST", "/store/webhook/mercadopago"+query, strings.NewReader(`{}`))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// ── Mercado Pago: función de verificación ──

func TestVerifyMercadoPagoWebhook(t *testing.T) {
	const ts = "1704908010"
	const reqID = "bb56a2f1-6aae-46ac-982e-9dcd3581d08e"

	t.Run("firma válida (manifest oficial id;request-id;ts): acepta", func(t *testing.T) {
		withPaymentCfg(t, func(c *PaymentConfig) { c.MercadoPagoWebhookSecret = mpTestSecret; c.AllowUnsignedWebhooks = false })
		r := mpRequest("?data.id=123456&type=payment", map[string]string{
			"x-signature": mpSignature(mpTestSecret, "123456", reqID, ts), "x-request-id": reqID})
		if err := verifyMercadoPagoWebhook(r); err != nil {
			t.Fatalf("una firma válida debe aceptarse, obtuve %v", err)
		}
	})
	t.Run("data.id alfanumérico se firma en minúsculas (según la documentación)", func(t *testing.T) {
		withPaymentCfg(t, func(c *PaymentConfig) { c.MercadoPagoWebhookSecret = mpTestSecret; c.AllowUnsignedWebhooks = false })
		r := mpRequest("?data.id=ABC123", map[string]string{
			"x-signature": mpSignature(mpTestSecret, "abc123", reqID, ts), "x-request-id": reqID})
		if err := verifyMercadoPagoWebhook(r); err != nil {
			t.Fatalf("data.id en mayúsculas debe normalizarse a minúsculas, obtuve %v", err)
		}
	})
	t.Run("sin data.id en la URL: el manifest omite esa parte", func(t *testing.T) {
		withPaymentCfg(t, func(c *PaymentConfig) { c.MercadoPagoWebhookSecret = mpTestSecret; c.AllowUnsignedWebhooks = false })
		r := mpRequest("", map[string]string{"x-signature": mpSignature(mpTestSecret, "", reqID, ts), "x-request-id": reqID})
		if err := verifyMercadoPagoWebhook(r); err != nil {
			t.Fatalf("obtuve %v", err)
		}
	})
	t.Run("firma con otro secreto: inválida", func(t *testing.T) {
		withPaymentCfg(t, func(c *PaymentConfig) { c.MercadoPagoWebhookSecret = mpTestSecret; c.AllowUnsignedWebhooks = false })
		r := mpRequest("?data.id=123456", map[string]string{
			"x-signature": mpSignature("otro-secreto", "123456", reqID, ts), "x-request-id": reqID})
		if err := verifyMercadoPagoWebhook(r); err != errWebhookBadSignature {
			t.Fatalf("esperaba firma inválida, obtuve %v", err)
		}
	})
	t.Run("firma válida pero para OTRO data.id: inválida", func(t *testing.T) {
		withPaymentCfg(t, func(c *PaymentConfig) { c.MercadoPagoWebhookSecret = mpTestSecret; c.AllowUnsignedWebhooks = false })
		r := mpRequest("?data.id=999999", map[string]string{
			"x-signature": mpSignature(mpTestSecret, "123456", reqID, ts), "x-request-id": reqID})
		if err := verifyMercadoPagoWebhook(r); err != errWebhookBadSignature {
			t.Fatalf("una firma de otro pago no debe servir, obtuve %v", err)
		}
	})
	t.Run("ts alterado: inválida", func(t *testing.T) {
		withPaymentCfg(t, func(c *PaymentConfig) { c.MercadoPagoWebhookSecret = mpTestSecret; c.AllowUnsignedWebhooks = false })
		sig := mpSignature(mpTestSecret, "123456", reqID, ts)
		r := mpRequest("?data.id=123456", map[string]string{
			"x-signature": strings.Replace(sig, "ts="+ts, "ts=1704908011", 1), "x-request-id": reqID})
		if err := verifyMercadoPagoWebhook(r); err != errWebhookBadSignature {
			t.Fatalf("obtuve %v", err)
		}
	})
	t.Run("encabezados ausentes: sin x-signature, sin v1, sin x-request-id", func(t *testing.T) {
		withPaymentCfg(t, func(c *PaymentConfig) { c.MercadoPagoWebhookSecret = mpTestSecret; c.AllowUnsignedWebhooks = false })
		cases := map[string]map[string]string{
			"sin x-signature":  {"x-request-id": reqID},
			"x-signature roto": {"x-signature": "basura", "x-request-id": reqID},
			"sin v1":           {"x-signature": "ts=" + ts, "x-request-id": reqID},
			"sin x-request-id": {"x-signature": mpSignature(mpTestSecret, "123456", reqID, ts)},
		}
		for name, h := range cases {
			if err := verifyMercadoPagoWebhook(mpRequest("?data.id=123456", h)); err != errWebhookMissingHeader {
				t.Errorf("%s: esperaba encabezados ausentes, obtuve %v", name, err)
			}
		}
	})
	t.Run("secreto ausente: en producción se rechaza, en desarrollo se acepta con advertencia", func(t *testing.T) {
		withPaymentCfg(t, func(c *PaymentConfig) { c.MercadoPagoWebhookSecret = ""; c.AllowUnsignedWebhooks = false })
		if err := verifyMercadoPagoWebhook(mpRequest("?data.id=1", nil)); err != errWebhookNotConfigured {
			t.Fatalf("en producción sin secreto debe rechazar, obtuve %v", err)
		}
		withPaymentCfg(t, func(c *PaymentConfig) { c.MercadoPagoWebhookSecret = ""; c.AllowUnsignedWebhooks, c.AppEnv = true, AppEnvDevelopment })
		if err := verifyMercadoPagoWebhook(mpRequest("?data.id=1", nil)); err != nil {
			t.Fatalf("en desarrollo local sin secreto se acepta, obtuve %v", err)
		}
	})
}

// El modo seguro NO depende de FRONTEND_URL: con FRONTEND_URL ausente o con el
// valor por defecto de localhost, sin ALLOW_UNSIGNED_WEBHOOKS se rechaza igual;
// solo el opt-in explícito de desarrollo acepta webhooks sin firma.
func TestVerifyWebhooks_FallaCerradoSinDependerDeFrontendURL(t *testing.T) {
	for _, frontendURL := range []string{"", "http://localhost:5173", "https://kidstoreperu.net"} {
		withPaymentCfg(t, func(c *PaymentConfig) {
			*c = PaymentConfig{FrontendURL: frontendURL} // AllowUnsignedWebhooks queda en false (valor por defecto)
		})
		if err := verifyMercadoPagoWebhook(mpRequest("?data.id=1", nil)); err != errWebhookNotConfigured {
			t.Errorf("MP con FRONTEND_URL=%q y sin opt-in debe rechazar, obtuve %v", frontendURL, err)
		}
		if err := verifyPayPalWebhook(paypalHeaders(), []byte(`{}`)); err != errWebhookNotConfigured {
			t.Errorf("PayPal con FRONTEND_URL=%q y sin opt-in debe rechazar, obtuve %v", frontendURL, err)
		}
	}
}

func TestVerifyWebhooks_ModoDesarrolloExplicito(t *testing.T) {
	withPaymentCfg(t, func(c *PaymentConfig) {
		*c = PaymentConfig{FrontendURL: "https://kidstoreperu.net", AllowUnsignedWebhooks: true, AppEnv: AppEnvDevelopment}
	})
	if err := verifyMercadoPagoWebhook(mpRequest("?data.id=1", nil)); err != nil {
		t.Errorf("con ALLOW_UNSIGNED_WEBHOOKS explícito y sin secreto MP debe aceptar, obtuve %v", err)
	}
	if err := verifyPayPalWebhook(paypalHeaders(), []byte(`{}`)); err != nil {
		t.Errorf("con ALLOW_UNSIGNED_WEBHOOKS explícito y sin webhook ID PayPal debe aceptar, obtuve %v", err)
	}
	// El opt-in no debilita la verificación cuando SÍ hay secreto configurado.
	withPaymentCfg(t, func(c *PaymentConfig) {
		*c = PaymentConfig{MercadoPagoWebhookSecret: mpTestSecret, AllowUnsignedWebhooks: true, AppEnv: AppEnvDevelopment}
	})
	if err := verifyMercadoPagoWebhook(mpRequest("?data.id=1", map[string]string{"x-signature": "ts=1,v1=00", "x-request-id": "r"})); err != errWebhookBadSignature {
		t.Errorf("con secreto configurado la firma inválida se rechaza aunque el opt-in esté activo, obtuve %v", err)
	}
}

// El opt-in NO funciona fuera de desarrollo: una configuración de producción
// (o sin APP_ENV) rechaza webhooks sin firma aunque ALLOW_UNSIGNED_WEBHOOKS=true.
func TestVerifyWebhooks_ProduccionRechazaSinFirmaAunqueElOptInEsteActivo(t *testing.T) {
	for _, env := range []string{"production", "", "staging", "Development "} {
		withPaymentCfg(t, func(c *PaymentConfig) {
			*c = PaymentConfig{FrontendURL: "http://localhost:5173", AllowUnsignedWebhooks: true, AppEnv: env}
		})
		if err := verifyMercadoPagoWebhook(mpRequest("?data.id=1", nil)); err != errWebhookNotConfigured {
			t.Errorf("MP con APP_ENV=%q y opt-in debe rechazar, obtuve %v", env, err)
		}
		if err := verifyPayPalWebhook(paypalHeaders(), []byte(`{}`)); err != errWebhookNotConfigured {
			t.Errorf("PayPal con APP_ENV=%q y opt-in debe rechazar, obtuve %v", env, err)
		}
	}
}

func TestResolveUnsignedWebhooks(t *testing.T) {
	if ok, msg := ResolveUnsignedWebhooks(false, "development"); ok || msg != "" {
		t.Errorf("sin opt-in: false y sin mensaje, obtuve %v %q", ok, msg)
	}
	if ok, msg := ResolveUnsignedWebhooks(true, "development"); !ok || msg != "" {
		t.Errorf("opt-in en desarrollo: true, obtuve %v %q", ok, msg)
	}
	for _, env := range []string{"production", "", "staging"} {
		if ok, msg := ResolveUnsignedWebhooks(true, env); ok || !strings.Contains(msg, "APP_ENV=development") {
			t.Errorf("opt-in con APP_ENV=%q debe ignorarse con un error claro, obtuve %v %q", env, ok, msg)
		}
	}
}

// ── Mercado Pago: handler ──

func TestHandlerMercadoPagoWebhook_AutenticaAntesDeRegistrarOProcesar(t *testing.T) {
	conn := setupShopTestDB(t)
	fake := stubGateways(t)
	since := time.Now().Add(-time.Second)
	defer cleanupEvents(conn, "mercadopago", since)
	router := webhookRouter(map[string]gin.HandlerFunc{"/mp": HandlerMercadoPagoWebhook(conn)})

	const ts = "1704908010"
	const reqID = "req-1"
	body := `{"type":"payment","data":{"id":"424242"},"payer":{"email":"secreto-mp@example.com"}}`
	sign := func(dataID string) map[string]string {
		return map[string]string{"x-signature": mpSignature(mpTestSecret, dataID, reqID, ts), "x-request-id": reqID}
	}

	t.Run("firma inválida: 401, ni se procesa ni se guarda el cuerpo — solo metadatos", func(t *testing.T) {
		withPaymentCfg(t, func(c *PaymentConfig) { c.MercadoPagoWebhookSecret = mpTestSecret; c.AllowUnsignedWebhooks = false })
		w := postRaw(router, "/mp?data.id=424242", strings.NewReader(body), map[string]string{
			"x-signature": mpSignature("otro-secreto", "424242", reqID, ts), "x-request-id": reqID})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("esperaba 401, obtuve %d", w.Code)
		}
		var raw *string
		var size int
		var hash, outcome string
		if err := conn.QueryRow(`SELECT raw_body, body_size, body_sha256, outcome FROM webhook_events WHERE gateway='mercadopago' AND received_at >= $1`, since).Scan(&raw, &size, &hash, &outcome); err != nil {
			t.Fatalf("el rechazo debe quedar auditado con metadatos: %v", err)
		}
		if raw != nil {
			t.Errorf("no debe guardarse el cuerpo de un evento rechazado: %q", *raw)
		}
		sum := sha256.Sum256([]byte(body))
		if size != len(body) || hash != hex.EncodeToString(sum[:]) || !strings.HasPrefix(outcome, "rejected:") {
			t.Errorf("metadatos incorrectos: size=%d hash=%s outcome=%s", size, hash, outcome)
		}
		unresolved, _ := conn.Query(`SELECT 1 FROM webhook_events WHERE gateway='mercadopago' AND received_at >= $1 AND (processed_at IS NULL OR outcome LIKE 'error:%')`, since)
		if unresolved.Next() {
			t.Error("un rechazo nunca debe quedar como candidato a reintento")
		}
		unresolved.Close()
		if fake.calls != 0 {
			t.Errorf("no debe consultarse a la pasarela por un evento no autenticado, hubo %d llamadas", fake.calls)
		}
	})
	conn.Exec(`DELETE FROM webhook_events WHERE gateway='mercadopago' AND received_at >= $1`, since)

	t.Run("encabezados ausentes: 401 sin procesar", func(t *testing.T) {
		withPaymentCfg(t, func(c *PaymentConfig) { c.MercadoPagoWebhookSecret = mpTestSecret; c.AllowUnsignedWebhooks = false })
		if w := postRaw(router, "/mp?data.id=424242", strings.NewReader(body), nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("esperaba 401, obtuve %d", w.Code)
		}
		if fake.calls != 0 {
			t.Error("no debe consultarse a la pasarela")
		}
	})
	conn.Exec(`DELETE FROM webhook_events WHERE gateway='mercadopago' AND received_at >= $1`, since)

	t.Run("producción sin MERCADOPAGO_WEBHOOK_SECRET: 503 y NADA se guarda ni se procesa", func(t *testing.T) {
		withPaymentCfg(t, func(c *PaymentConfig) { c.MercadoPagoWebhookSecret = ""; c.AllowUnsignedWebhooks = false })
		if w := postRaw(router, "/mp?data.id=424242", strings.NewReader(body), sign("424242")); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("esperaba 503, obtuve %d", w.Code)
		}
		if n := countEvents(t, conn, "mercadopago", since); n != 0 {
			t.Errorf("no debe persistirse nada, encontré %d filas", n)
		}
		if fake.calls != 0 {
			t.Error("no debe consultarse a la pasarela")
		}
	})

	t.Run("data.id de la URL distinto del cuerpo: 400 aunque la firma sea válida", func(t *testing.T) {
		withPaymentCfg(t, func(c *PaymentConfig) { c.MercadoPagoWebhookSecret = mpTestSecret; c.AllowUnsignedWebhooks = false })
		if w := postRaw(router, "/mp?data.id=111", strings.NewReader(body), sign("111")); w.Code != http.StatusBadRequest {
			t.Fatalf("esperaba 400, obtuve %d", w.Code)
		}
		if n := countEvents(t, conn, "mercadopago", since); n != 0 {
			t.Errorf("no debe persistirse nada, encontré %d filas", n)
		}
	})

	t.Run("firma válida: 200, se guarda solo el evento normalizado y se consulta el estado real", func(t *testing.T) {
		withPaymentCfg(t, func(c *PaymentConfig) { c.MercadoPagoWebhookSecret = mpTestSecret; c.AllowUnsignedWebhooks = false })
		w := postRaw(router, "/mp?data.id=424242&type=payment", strings.NewReader(body), sign("424242"))
		if w.Code != http.StatusOK {
			t.Fatalf("esperaba 200, obtuve %d: %s", w.Code, w.Body.String())
		}
		waitEventsProcessed(t, conn, "mercadopago", since)
		var stored string
		if err := conn.QueryRow(`SELECT raw_body FROM webhook_events WHERE gateway='mercadopago' AND received_at >= $1`, since).Scan(&stored); err != nil {
			t.Fatalf("el evento válido debe registrarse: %v", err)
		}
		if strings.Contains(stored, "secreto-mp@example.com") {
			t.Errorf("no debe guardarse el correo del pagador: %s", stored)
		}
		if fake.calls == 0 {
			t.Error("el evento autenticado debe seguir consultando el estado real a la pasarela")
		}
	})
}

// ── PayPal ──

// fakePayPal simula token OAuth y verify-webhook-signature.
type fakePayPal struct {
	server        *httptest.Server
	verifyStatus  string // "SUCCESS" / "FAILURE"
	verifyHTTP    int
	verifyCalls   int
	lastVerifyRaw string
}

func newFakePayPal(t *testing.T) *fakePayPal {
	t.Helper()
	f := &fakePayPal{verifyStatus: "SUCCESS", verifyHTTP: 200}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/oauth2/token":
			fmt.Fprint(w, `{"access_token":"tok"}`)
		case "/v1/notifications/verify-webhook-signature":
			f.verifyCalls++
			b, _ := io.ReadAll(r.Body)
			f.lastVerifyRaw = string(b)
			w.WriteHeader(f.verifyHTTP)
			fmt.Fprintf(w, `{"verification_status":%q}`, f.verifyStatus)
		default:
			w.WriteHeader(404)
		}
	}))
	prev := payPalBaseURLOverride
	payPalBaseURLOverride = f.server.URL
	t.Cleanup(func() { payPalBaseURLOverride = prev; f.server.Close() })
	return f
}

func paypalHeaders() http.Header {
	h := http.Header{}
	h.Set("PAYPAL-AUTH-ALGO", "SHA256withRSA")
	h.Set("PAYPAL-CERT-URL", "https://api.paypal.com/v1/notifications/certs/CERT-1")
	h.Set("PAYPAL-TRANSMISSION-ID", "tid-1")
	h.Set("PAYPAL-TRANSMISSION-SIG", "sig-1")
	h.Set("PAYPAL-TRANSMISSION-TIME", "2026-09-25T10:00:00Z")
	return h
}

func withPayPalCfg(t *testing.T, webhookID string, require bool) {
	withPaymentCfg(t, func(c *PaymentConfig) {
		c.PayPalClientID = "cid"
		c.PayPalClientSecret = "csecret"
		c.PayPalWebhookID = webhookID
		c.AllowUnsignedWebhooks, c.AppEnv = !require, AppEnvDevelopment
	})
}

func TestVerifyPayPalWebhook(t *testing.T) {
	// Cuerpo con espacios y saltos de línea A PROPÓSITO: PayPal calcula un
	// CRC32 del cuerpo exacto, así que se incrusta sin re-serializar.
	body := []byte("{ \"event_type\": \"CHECKOUT.ORDER.APPROVED\",\n  \"resource\": {\"id\": \"ORD1\"} }")

	t.Run("firma válida: acepta y envía el evento EXACTO + webhook_id + encabezados", func(t *testing.T) {
		f := newFakePayPal(t)
		withPayPalCfg(t, "WH-123", true)
		if err := verifyPayPalWebhook(paypalHeaders(), body); err != nil {
			t.Fatalf("una firma SUCCESS debe aceptarse, obtuve %v", err)
		}
		if !strings.Contains(f.lastVerifyRaw, `"webhook_event":`+string(body)) {
			t.Errorf("el evento debe incrustarse byte por byte: %s", f.lastVerifyRaw)
		}
		for _, want := range []string{`"webhook_id":"WH-123"`, `"transmission_id":"tid-1"`, `"transmission_sig":"sig-1"`, `"auth_algo":"SHA256withRSA"`} {
			if !strings.Contains(f.lastVerifyRaw, want) {
				t.Errorf("falta %s en la verificación: %s", want, f.lastVerifyRaw)
			}
		}
	})
	t.Run("firma inválida (FAILURE): rechaza", func(t *testing.T) {
		f := newFakePayPal(t)
		f.verifyStatus = "FAILURE"
		withPayPalCfg(t, "WH-123", true)
		if err := verifyPayPalWebhook(paypalHeaders(), body); err != errWebhookBadSignature {
			t.Fatalf("esperaba firma inválida, obtuve %v", err)
		}
	})
	t.Run("encabezados ausentes: rechaza SIN llamar a PayPal", func(t *testing.T) {
		f := newFakePayPal(t)
		withPayPalCfg(t, "WH-123", true)
		for _, name := range []string{"PAYPAL-AUTH-ALGO", "PAYPAL-CERT-URL", "PAYPAL-TRANSMISSION-ID", "PAYPAL-TRANSMISSION-SIG", "PAYPAL-TRANSMISSION-TIME"} {
			h := paypalHeaders()
			h.Del(name)
			if err := verifyPayPalWebhook(h, body); err != errWebhookMissingHeader {
				t.Errorf("sin %s: esperaba encabezados ausentes, obtuve %v", name, err)
			}
		}
		if f.verifyCalls != 0 {
			t.Errorf("no debe llamarse a PayPal sin encabezados completos (%d llamadas)", f.verifyCalls)
		}
	})
	t.Run("cuerpo que no es JSON: rechaza sin llamar a PayPal", func(t *testing.T) {
		f := newFakePayPal(t)
		withPayPalCfg(t, "WH-123", true)
		if err := verifyPayPalWebhook(paypalHeaders(), []byte("no es json")); err != errWebhookBadSignature {
			t.Fatalf("obtuve %v", err)
		}
		if f.verifyCalls != 0 {
			t.Error("no debe llamarse a PayPal")
		}
	})
	t.Run("PAYPAL_WEBHOOK_ID ausente: en producción se rechaza, en desarrollo se acepta", func(t *testing.T) {
		f := newFakePayPal(t)
		withPayPalCfg(t, "", true)
		if err := verifyPayPalWebhook(paypalHeaders(), body); err != errWebhookNotConfigured {
			t.Fatalf("en producción sin webhook id debe rechazar, obtuve %v", err)
		}
		withPayPalCfg(t, "", false)
		if err := verifyPayPalWebhook(paypalHeaders(), body); err != nil {
			t.Fatalf("en desarrollo local se acepta, obtuve %v", err)
		}
		if f.verifyCalls != 0 {
			t.Error("sin webhook id no hay nada que verificar contra PayPal")
		}
	})
	t.Run("PayPal no disponible o webhook_id inválido (HTTP != 200): no es 'firma inválida'", func(t *testing.T) {
		f := newFakePayPal(t)
		f.verifyHTTP = 500
		withPayPalCfg(t, "WH-123", true)
		err := verifyPayPalWebhook(paypalHeaders(), body)
		if err == nil || !strings.Contains(err.Error(), errWebhookVerifyUnavailable.Error()) {
			t.Fatalf("esperaba verificación no disponible, obtuve %v", err)
		}
	})
}

func TestHandlerPayPalWebhook_AutenticaAntesDeRegistrarOProcesar(t *testing.T) {
	conn := setupShopTestDB(t)
	// Sin stubGateways: TODA la comunicación con PayPal (token, verificación y
	// la captura del goroutine de procesamiento) va al httptest.Server de
	// newFakePayPal vía payPalBaseURLOverride — nada llega a PayPal real.
	since := time.Now().Add(-time.Second)
	defer cleanupEvents(conn, "paypal", since)
	router := webhookRouter(map[string]gin.HandlerFunc{"/pp": HandlerPayPalWebhook(conn)})
	body := `{"event_type":"CHECKOUT.ORDER.APPROVED","resource":{"id":"ORDPP9","payer":{"email_address":"secreto-pp@example.com"}}}`
	headers := map[string]string{}
	for k, v := range paypalHeaders() {
		headers[k] = v[0]
	}

	t.Run("firma inválida: 401, solo metadatos, nada procesado", func(t *testing.T) {
		f := newFakePayPal(t)
		f.verifyStatus = "FAILURE"
		withPayPalCfg(t, "WH-123", true)
		if w := postRaw(router, "/pp", strings.NewReader(body), headers); w.Code != http.StatusUnauthorized {
			t.Fatalf("esperaba 401, obtuve %d", w.Code)
		}
		var raw *string
		var outcome string
		if err := conn.QueryRow(`SELECT raw_body, outcome FROM webhook_events WHERE gateway='paypal' AND received_at >= $1`, since).Scan(&raw, &outcome); err != nil {
			t.Fatalf("el rechazo debe auditarse: %v", err)
		}
		if raw != nil || !strings.HasPrefix(outcome, "rejected:") {
			t.Errorf("solo metadatos: raw=%v outcome=%s", raw, outcome)
		}
	})
	conn.Exec(`DELETE FROM webhook_events WHERE gateway='paypal' AND received_at >= $1`, since)

	t.Run("encabezados ausentes: 401", func(t *testing.T) {
		newFakePayPal(t)
		withPayPalCfg(t, "WH-123", true)
		if w := postRaw(router, "/pp", strings.NewReader(body), nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("esperaba 401, obtuve %d", w.Code)
		}
	})
	conn.Exec(`DELETE FROM webhook_events WHERE gateway='paypal' AND received_at >= $1`, since)

	t.Run("PayPal caído al verificar: 503 (reintentable) y NO se persiste nada", func(t *testing.T) {
		f := newFakePayPal(t)
		f.verifyHTTP = 503
		withPayPalCfg(t, "WH-123", true)
		if w := postRaw(router, "/pp", strings.NewReader(body), headers); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("esperaba 503, obtuve %d", w.Code)
		}
		if n := countEvents(t, conn, "paypal", since); n != 0 {
			t.Errorf("no debe persistirse nada, encontré %d", n)
		}
	})

	t.Run("producción sin PAYPAL_WEBHOOK_ID: 503 y nada persistido", func(t *testing.T) {
		newFakePayPal(t)
		withPayPalCfg(t, "", true)
		if w := postRaw(router, "/pp", strings.NewReader(body), headers); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("esperaba 503, obtuve %d", w.Code)
		}
		if n := countEvents(t, conn, "paypal", since); n != 0 {
			t.Errorf("no debe persistirse nada, encontré %d", n)
		}
	})

	t.Run("firma válida: 200 y solo se guarda el evento normalizado", func(t *testing.T) {
		newFakePayPal(t)
		withPayPalCfg(t, "WH-123", true)
		if w := postRaw(router, "/pp", strings.NewReader(body), headers); w.Code != http.StatusOK {
			t.Fatalf("esperaba 200, obtuve %d: %s", w.Code, w.Body.String())
		}
		waitEventsProcessed(t, conn, "paypal", since)
		var stored string
		if err := conn.QueryRow(`SELECT raw_body FROM webhook_events WHERE gateway='paypal' AND received_at >= $1`, since).Scan(&stored); err != nil {
			t.Fatalf("el evento válido debe registrarse: %v", err)
		}
		if strings.Contains(stored, "secreto-pp@example.com") {
			t.Errorf("no debe guardarse el correo del pagador: %s", stored)
		}
	})
}
