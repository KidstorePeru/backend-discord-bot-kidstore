package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// pinger — lo único que /health necesita de la base de datos (*sql.DB lo
// cumple); interfaz para poder probar el handler sin una base real.
type pinger interface {
	PingContext(ctx context.Context) error
}

// healthTimeout — si la base tarda más que esto en responder, se considera
// caída: para el cliente, una tienda que no responde es una tienda caída.
var healthTimeout = 3 * time.Second

// healthHandler responde 200 solo si el servidor Y la base de datos
// funcionan. Lo consulta un monitor externo (HetrixTools) cada minuto:
// "GET /" responde "ok" aunque la base esté caída, así que no sirve para
// eso. No devuelve detalles del error (es una ruta pública).
func healthHandler(database pinger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(c.Request.Context(), healthTimeout)
		defer cancel()
		if err := database.PingContext(ctx); err != nil {
			slog.Warn("Health: la base de datos no responde", "error", err)
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "error", "database": "down"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "database": "up"})
	}
}
