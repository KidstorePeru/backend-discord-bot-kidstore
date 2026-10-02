package admin

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"KidStoreStore/src/db"
	"KidStoreStore/src/store"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// adminReviewer — quién aprueba/rechaza (queda en el registro).
func adminReviewer(c *gin.Context, database *sql.DB) string {
	if v, ok := c.Get("customer_id"); ok {
		if id, err := uuid.Parse(fmt.Sprint(v)); err == nil {
			if cust, err := db.GetCustomerByID(database, id); err == nil {
				return "Panel: " + cust.EpicUsername
			}
		}
	}
	return "Panel admin"
}

type adminManualView struct {
	db.ManualPaymentRequest
	CustomerEpic  string `json:"customer_epic"`
	CustomerEmail string `json:"customer_email"`
	DuplicateOps  int    `json:"duplicate_ops"`
}

// HandlerGetManualPayments lista los comprobantes (?status=pending|approved|rejected).
func HandlerGetManualPayments(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		status := c.Query("status")
		if status != "" && status != "pending" && status != "approved" && status != "rejected" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "estado inválido"})
			return
		}
		list, err := db.AdminListManualPayments(database, status, 200)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo comprobantes"})
			return
		}
		out := make([]adminManualView, 0, len(list))
		for _, m := range list {
			v := adminManualView{ManualPaymentRequest: m}
			if cust, err := db.GetCustomerByID(database, m.CustomerID); err == nil {
				v.CustomerEpic = cust.EpicUsername
				if cust.Email != nil {
					v.CustomerEmail = *cust.Email
				}
			}
			v.DuplicateOps, _ = db.OperationNumberUses(database, m.OperationNumber, m.ID)
			out = append(out, v)
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "requests": out})
	}
}

// HandlerGetManualPaymentProof devuelve la imagen/PDF descifrada del comprobante.
func HandlerGetManualPaymentProof(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "ID inválido"})
			return
		}
		m, err := db.GetManualPaymentRequest(database, id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "solicitud no encontrada"})
			return
		}
		data, ct, err := store.LoadProof(c.Request.Context(), m)
		if err != nil {
			c.JSON(http.StatusGone, gin.H{"success": false, "error": err.Error()})
			return
		}
		c.Header("Cache-Control", "no-store, private")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Data(http.StatusOK, ct, data)
	}
}

func HandlerApproveManualPayment(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "ID inválido"})
			return
		}
		// after_reject: el cliente reclamó un rechazo y soporte confirmó el pago.
		var req struct {
			AfterReject bool `json:"after_reject"`
		}
		_ = c.ShouldBindJSON(&req) // el cuerpo es opcional
		m, err := store.ApproveManualPayment(database, id, adminReviewer(c, database), req.AfterReject)
		if err != nil {
			respondManualError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "request": m})
	}
}

func HandlerRejectManualPayment(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "ID inválido"})
			return
		}
		var req struct {
			Reason string `json:"reason"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Reason) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "escribe el motivo del rechazo (lo verá el cliente)"})
			return
		}
		m, err := store.RejectManualPayment(database, id, req.Reason, adminReviewer(c, database))
		if err != nil {
			respondManualError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "request": m})
	}
}

func respondManualError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, db.ErrManualAlreadyReviewed):
		c.JSON(http.StatusConflict, gin.H{"success": false, "error": "este comprobante ya fue revisado"})
	case errors.Is(err, sql.ErrNoRows):
		c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "solicitud no encontrada"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
	}
}
