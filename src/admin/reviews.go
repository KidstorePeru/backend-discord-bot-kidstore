package admin

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"KidStoreStore/src/db"
	"KidStoreStore/src/store"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// HandlerGetReviews lista reseñas (?status=pending|approved|rejected, vacío = todas).
func HandlerGetReviews(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		status := c.Query("status")
		if status != "" && status != "pending" && status != "approved" && status != "rejected" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "estado inválido"})
			return
		}
		list, err := db.AdminListReviews(database, status, 200)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo reseñas"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "reviews": list})
	}
}

// HandlerModerateReview aprueba o rechaza una reseña, con respuesta opcional
// de la tienda (se muestra debajo de la reseña en la portada).
func HandlerModerateReview(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "ID inválido"})
			return
		}
		var req struct {
			Status string `json:"status"`
			Reply  string `json:"reply"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || (req.Status != "approved" && req.Status != "rejected" && req.Status != "pending") {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "datos inválidos"})
			return
		}
		reply := strings.TrimSpace(req.Reply)
		if len([]rune(reply)) > 500 {
			reply = string([]rune(reply)[:500])
		}
		if err := db.ModerateReview(database, id, req.Status, reply); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "reseña no encontrada"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error guardando la reseña"})
			return
		}
		store.InvalidatePublicReviews()
		db.AddAuditLog(database, nil, "REVIEW_"+strings.ToUpper(req.Status), id.String(), c.ClientIP())
		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}
