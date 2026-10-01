package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// Regresión: detrás de Railway, todas las visitas llegaban desde 100.64.0.x (la
// red interna del proxy). Sin confiar en ese rango, c.ClientIP() devolvía la IP
// del proxy para TODOS los clientes y los límites por IP se compartían.
func TestTrustedProxies_IPRealDelClienteDetrasDeRailway(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if err := r.SetTrustedProxies(trustedProxyCIDRs); err != nil {
		t.Fatal(err)
	}
	r.GET("/ip", func(c *gin.Context) { c.String(http.StatusOK, c.ClientIP()) })

	cases := []struct {
		name, remote, xff, want string
	}{
		// El proxy de Railway (100.64.x.x) agrega la IP real al final.
		{"cliente detrás de Railway", "100.64.0.3:12345", "181.66.10.20", "181.66.10.20"},
		// Un cliente que intenta falsificar el header: Railway agrega su IP real al final,
		// y se toma la del proxy de confianza más cercano, no la inventada.
		{"X-Forwarded-For falsificado", "100.64.0.20:5555", "1.2.3.4, 181.66.10.20", "181.66.10.20"},
		// Conexión directa desde internet (sin pasar por el proxy): el header se ignora.
		{"conexión directa no confiable", "200.48.1.1:4444", "1.2.3.4", "200.48.1.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ip", nil)
			req.RemoteAddr = c.remote
			req.Header.Set("X-Forwarded-For", c.xff)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if got := w.Body.String(); got != c.want {
				t.Errorf("ClientIP = %s, se esperaba %s", got, c.want)
			}
		})
	}
}
