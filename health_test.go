package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type fakePinger func(ctx context.Context) error

func (f fakePinger) PingContext(ctx context.Context) error { return f(ctx) }

func healthRequest(t *testing.T, method string, db pinger) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/health", healthHandler(db))
	r.HEAD("/health", healthHandler(db))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(method, "/health", nil))
	return w
}

func TestHealth_OKConBaseDeDatos(t *testing.T) {
	w := healthRequest(t, http.MethodGet, fakePinger(func(context.Context) error { return nil }))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Errorf("status=%d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("la respuesta no debería quedar en caché (el monitor vería un estado viejo)")
	}
	if w := healthRequest(t, http.MethodHead, fakePinger(func(context.Context) error { return nil })); w.Code != http.StatusOK {
		t.Errorf("HEAD /health = %d", w.Code)
	}
}

func TestHealth_503SiLaBaseDeDatosFalla(t *testing.T) {
	w := healthRequest(t, http.MethodGet, fakePinger(func(context.Context) error {
		return errors.New("dial tcp 10.0.0.1:5432: connection refused (password=secreta)")
	}))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, se esperaba 503", w.Code)
	}
	if strings.Contains(w.Body.String(), "secreta") || strings.Contains(w.Body.String(), "10.0.0.1") {
		t.Errorf("la respuesta pública no debe incluir detalles del error: %s", w.Body.String())
	}
}

func TestHealth_503SiLaBaseDeDatosNoResponde(t *testing.T) {
	prev := healthTimeout
	healthTimeout = 50 * time.Millisecond
	t.Cleanup(func() { healthTimeout = prev })

	start := time.Now()
	w := healthRequest(t, http.MethodGet, fakePinger(func(ctx context.Context) error {
		<-ctx.Done() // base colgada: no responde nunca
		return ctx.Err()
	}))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, se esperaba 503", w.Code)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("una base colgada no debería dejar colgado al monitor")
	}
}
