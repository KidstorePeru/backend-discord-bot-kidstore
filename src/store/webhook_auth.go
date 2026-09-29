package store

// Verificación criptográfica de los webhooks de Mercado Pago y PayPal.
// (NOWPayments y dLocal Go ya tienen la suya: verifyNOWPaymentsSignature y
// verifyDLocalGoSignature.) Ningún handler registra ni procesa un evento
// antes de que esta verificación lo autentique.
//
// CONFIGURACIÓN (solo nombres de variables de entorno — los valores se
// cargan en el panel de Railway, nunca en el repositorio):
//   - MERCADOPAGO_WEBHOOK_SECRET: "Clave secreta" de la firma de webhooks,
//     en Mercado Pago Developers → tu aplicación → Webhooks → Configurar
//     notificaciones (es distinta del Access Token).
//   - PAYPAL_WEBHOOK_ID: el ID del webhook registrado en PayPal Developer
//     Dashboard → tu app → Webhooks (apuntando a /store/webhook/paypal). Debe
//     corresponder al mismo modo (sandbox/live) que PAYPAL_MODE.
// En producción (FRONTEND_URL distinto del de desarrollo) un webhook se
// RECHAZA si falta la variable correspondiente; en desarrollo local se acepta
// sin firma con una advertencia, porque MercadoPago/PayPal no pueden llamar a
// localhost de todas formas.

import (
	"KidStoreStore/src/db"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

var (
	// errWebhookNotConfigured: falta el secreto/ID necesario en producción —
	// problema NUESTRO, no del emisor.
	errWebhookNotConfigured = errors.New("webhook: falta la configuración de verificación de firma")
	// errWebhookMissingHeader: la petición no trae los encabezados de firma.
	errWebhookMissingHeader = errors.New("webhook: faltan encabezados de firma")
	// errWebhookBadSignature: la firma no es válida — no viene de la pasarela.
	errWebhookBadSignature = errors.New("webhook: firma inválida")
	// errWebhookVerifyUnavailable: no se pudo completar la verificación (p. ej.
	// PayPal no respondió) — no dice nada sobre la autenticidad del evento.
	errWebhookVerifyUnavailable = errors.New("webhook: no se pudo completar la verificación de firma")
)

// payPalBaseURLOverride permite que las pruebas apunten la verificación (y el
// token OAuth) a un httptest.Server en vez de la API real de PayPal.
var payPalBaseURLOverride string

func payPalBaseURL() string {
	if payPalBaseURLOverride != "" {
		return payPalBaseURLOverride
	}
	if paymentCfg.PayPalMode == "live" {
		return "https://api-m.paypal.com"
	}
	return "https://api-m.sandbox.paypal.com"
}

// ── Mercado Pago ──

// mercadoPagoManifest arma el "manifest" que Mercado Pago firma, según su
// documentación oficial de webhooks:
//   id:[data.id];request-id:[x-request-id];ts:[ts];
// donde data.id sale del query string de la URL (en minúsculas si es
// alfanumérico) y cada parte que no venga se omite del manifest.
func mercadoPagoManifest(dataID, requestID, ts string) string {
	var b strings.Builder
	if dataID != "" {
		b.WriteString("id:" + strings.ToLower(dataID) + ";")
	}
	if requestID != "" {
		b.WriteString("request-id:" + requestID + ";")
	}
	if ts != "" {
		b.WriteString("ts:" + ts + ";")
	}
	return b.String()
}

// parseMercadoPagoSignatureHeader extrae ts y v1 de "ts=...,v1=...".
func parseMercadoPagoSignatureHeader(h string) (ts, v1 string) {
	for _, part := range strings.Split(h, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch strings.TrimSpace(kv[0]) {
		case "ts":
			ts = strings.TrimSpace(kv[1])
		case "v1":
			v1 = strings.TrimSpace(kv[1])
		}
	}
	return ts, v1
}

// verifyMercadoPagoWebhook valida x-signature (HMAC-SHA256 hex del manifest
// con MERCADOPAGO_WEBHOOK_SECRET). Devuelve nil solo si la firma es válida
// (o si no hay secreto y el opt-in de desarrollo está activo).
func verifyMercadoPagoWebhook(r *http.Request) error {
	secret := paymentCfg.MercadoPagoWebhookSecret
	if secret == "" {
		if !unsignedWebhooksAllowed() {
			return errWebhookNotConfigured
		}
		slog.Warn("MercadoPago webhook aceptado SIN verificar firma: MERCADOPAGO_WEBHOOK_SECRET no está configurado (ALLOW_UNSIGNED_WEBHOOKS=true con APP_ENV=development)")
		return nil
	}
	ts, v1 := parseMercadoPagoSignatureHeader(r.Header.Get("x-signature"))
	if ts == "" || v1 == "" {
		return errWebhookMissingHeader
	}
	requestID := r.Header.Get("x-request-id")
	if requestID == "" {
		return errWebhookMissingHeader
	}
	manifest := mercadoPagoManifest(r.URL.Query().Get("data.id"), requestID, ts)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(manifest))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(strings.ToLower(v1)), []byte(expected)) {
		return errWebhookBadSignature
	}
	return nil
}

// ── PayPal ──

// verifyPayPalWebhook usa el método OFICIAL de PayPal: POST
// /v1/notifications/verify-webhook-signature con los encabezados
// PAYPAL-* y el evento EXACTO recibido (PayPal calcula un CRC32 sobre el
// cuerpo, así que no se re-serializa: se incrusta tal cual).
func verifyPayPalWebhook(h http.Header, body []byte) error {
	webhookID := paymentCfg.PayPalWebhookID
	if webhookID == "" {
		if !unsignedWebhooksAllowed() {
			return errWebhookNotConfigured
		}
		slog.Warn("PayPal webhook aceptado SIN verificar firma: PAYPAL_WEBHOOK_ID no está configurado (ALLOW_UNSIGNED_WEBHOOKS=true con APP_ENV=development)")
		return nil
	}
	authAlgo := h.Get("PAYPAL-AUTH-ALGO")
	certURL := h.Get("PAYPAL-CERT-URL")
	transmissionID := h.Get("PAYPAL-TRANSMISSION-ID")
	transmissionSig := h.Get("PAYPAL-TRANSMISSION-SIG")
	transmissionTime := h.Get("PAYPAL-TRANSMISSION-TIME")
	if authAlgo == "" || certURL == "" || transmissionID == "" || transmissionSig == "" || transmissionTime == "" {
		return errWebhookMissingHeader
	}
	if !json.Valid(body) {
		return errWebhookBadSignature
	}

	token, err := getPayPalAccessToken()
	if err != nil {
		return fmt.Errorf("%w: %v", errWebhookVerifyUnavailable, err)
	}

	scalar := func(v string) []byte { b, _ := json.Marshal(v); return b }
	var payload bytes.Buffer
	payload.WriteString(`{"auth_algo":`)
	payload.Write(scalar(authAlgo))
	payload.WriteString(`,"cert_url":`)
	payload.Write(scalar(certURL))
	payload.WriteString(`,"transmission_id":`)
	payload.Write(scalar(transmissionID))
	payload.WriteString(`,"transmission_sig":`)
	payload.Write(scalar(transmissionSig))
	payload.WriteString(`,"transmission_time":`)
	payload.Write(scalar(transmissionTime))
	payload.WriteString(`,"webhook_id":`)
	payload.Write(scalar(webhookID))
	payload.WriteString(`,"webhook_event":`)
	payload.Write(body)
	payload.WriteString(`}`)

	req, err := http.NewRequest("POST", payPalBaseURL()+"/v1/notifications/verify-webhook-signature", &payload)
	if err != nil {
		return fmt.Errorf("%w: %v", errWebhookVerifyUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errWebhookVerifyUnavailable, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		// Un 4xx acá (p. ej. webhook_id inexistente) es un problema de
		// configuración nuestro, no una firma inválida del evento.
		return fmt.Errorf("%w: HTTP %d", errWebhookVerifyUnavailable, resp.StatusCode)
	}
	var result struct {
		VerificationStatus string `json:"verification_status"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("%w: respuesta ilegible", errWebhookVerifyUnavailable)
	}
	if result.VerificationStatus != "SUCCESS" {
		return errWebhookBadSignature
	}
	return nil
}

// ── Respuesta común ──

// logRejectedWebhook guarda SOLO metadatos de un webhook rechazado (ver
// db.LogRejectedWebhookEvent): tamaño y SHA-256 del cuerpo, nunca su
// contenido. La escritura es atómica y queda ya resuelta, así que un
// rechazo jamás entra a la cola de reintentos.
func logRejectedWebhook(database *sql.DB, gateway, reason string, body []byte) error {
	sum := sha256.Sum256(body)
	return db.LogRejectedWebhookEvent(database, gateway, reason, len(body), hex.EncodeToString(sum[:]))
}

// rejectUnauthenticatedWebhook responde a un webhook cuya verificación
// falló y, cuando el motivo es que el emisor NO se autenticó (firma inválida
// o encabezados ausentes), deja constancia MÍNIMA para auditoría: pasarela,
// fecha, motivo, tamaño y SHA-256 del cuerpo — nunca el cuerpo ni sus datos
// (ver db.LogRejectedWebhookEvent). Un problema de configuración nuestro o
// una verificación no disponible NO se persisten (no son un intento hostil).
// Siempre devuelve true: el handler debe cortar.
//   - 401: no autenticado (la pasarela real, mal configurada de nuestro
//     lado, seguirá reintentando — mismo criterio que NOWPayments).
//   - 503: falta configuración o la verificación no está disponible — la
//     pasarela reintentará más tarde.
func rejectUnauthenticatedWebhook(c *gin.Context, database *sql.DB, gateway string, body []byte, verifyErr error) bool {
	switch {
	case errors.Is(verifyErr, errWebhookBadSignature), errors.Is(verifyErr, errWebhookMissingHeader):
		reason := "invalid signature"
		if errors.Is(verifyErr, errWebhookMissingHeader) {
			reason = "missing signature headers"
		}
		slog.Warn("webhook: no autenticado, se rechaza", "gateway", gateway, "reason", reason, "bytes", len(body))
		if err := logRejectedWebhook(database, gateway, reason, body); err != nil {
			slog.Error("webhook: no se pudo registrar el rechazo", "gateway", gateway, "error", err)
		}
		c.JSON(http.StatusUnauthorized, gin.H{"received": false, "error": "firma inválida"})
	case errors.Is(verifyErr, errWebhookNotConfigured):
		slog.Error("webhook: se rechaza porque falta configurar el secreto de verificación de firma", "gateway", gateway)
		c.JSON(http.StatusServiceUnavailable, gin.H{"received": false, "error": "verificación de firma no configurada"})
	default:
		slog.Error("webhook: no se pudo verificar la firma, se rechaza para que la pasarela reintente", "gateway", gateway, "error", verifyErr)
		c.JSON(http.StatusServiceUnavailable, gin.H{"received": false, "error": "no se pudo verificar la firma, reintenta"})
	}
	return true
}
