package store

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/types"
	"database/sql"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ==================== LIBRO DE RECLAMACIONES VIRTUAL ====================
//
// Requisito legal para negocios de e-commerce en Perú (Código de Protección
// y Defensa del Consumidor, Ley N° 29571, y su reglamento). Debe estar
// disponible para CUALQUIER consumidor, sin necesidad de tener una cuenta —
// por eso este handler es público (sin JWT), a diferencia del resto de
// /store que sí requiere sesión.

// HandlerCreateComplaint recibe un reclamo o queja del Libro de Reclamaciones.
func HandlerCreateComplaint(database *sql.DB, cfg types.EnvConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req types.CreateComplaintRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		// Si el consumidor es menor de edad, el reglamento espera el dato de
		// quién presenta el reclamo en su representación (padre/madre/apoderado).
		if req.IsMinor && strings.TrimSpace(req.GuardianName) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "si el consumidor es menor de edad, se requiere el nombre del padre/madre/apoderado"})
			return
		}

		complaint := types.ConsumerComplaint{
			Kind:               req.Kind,
			FullName:           strings.TrimSpace(req.FullName),
			DocumentType:       req.DocumentType,
			DocumentNumber:     strings.TrimSpace(req.DocumentNumber),
			Email:              strings.TrimSpace(strings.ToLower(req.Email)),
			ProductDescription: strings.TrimSpace(req.ProductDescription),
			Detail:             strings.TrimSpace(req.Detail),
			ConsumerRequest:    strings.TrimSpace(req.ConsumerRequest),
			IsMinor:            req.IsMinor,
			AmountInvolved:     req.AmountInvolved,
		}
		if req.Phone != "" {
			p := strings.TrimSpace(req.Phone)
			complaint.Phone = &p
		}
		if req.Address != "" {
			a := strings.TrimSpace(req.Address)
			complaint.Address = &a
		}
		if req.GuardianName != "" {
			g := strings.TrimSpace(req.GuardianName)
			complaint.GuardianName = &g
		}
		if req.OrderID != "" {
			if oid, err := uuid.Parse(req.OrderID); err == nil {
				complaint.OrderID = &oid
			}
		}

		saved, err := db.CreateComplaint(database, complaint, c.ClientIP())
		if err != nil {
			slog.Error("Libro de Reclamaciones: error guardando reclamo", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudo registrar tu reclamo, intenta de nuevo"})
			return
		}

		db.AddAuditLog(database, nil, "COMPLAINT_CREATED", "reclamo "+saved.Reference+" ("+saved.Kind+")", c.ClientIP())

		// El idioma no se autentica (formulario público) — se toma del header
		// que ya usa el resto del sitio para el selector ES/EN.
		lang := c.GetHeader("X-Lang")
		if lang != "en" {
			lang = "es"
		}
		go SendComplaintReceivedEmail(cfg, saved.Email, saved.FullName, saved.Reference, saved.Kind, saved.ProductDescription, saved.Detail, lang)

		c.JSON(http.StatusCreated, gin.H{
			"success": true,
			"complaint": gin.H{
				"reference":  saved.Reference,
				"status":     saved.Status,
				"created_at": saved.CreatedAt,
			},
		})
	}
}

// HandlerGetComplaintStatus permite a un consumidor consultar el estado de su
// reclamo con el código de seguimiento que se le entregó — no expone otros
// reclamos ni requiere cuenta, solo el código exacto (no es enumerable: es un
// valor aleatorio de suficiente entropía).
func HandlerGetComplaintStatus(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		reference := strings.ToUpper(strings.TrimSpace(c.Param("reference")))
		complaint, err := db.GetComplaintByReference(database, reference)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "no se encontró un reclamo con ese código"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"complaint": gin.H{
				"reference":      complaint.Reference,
				"kind":           complaint.Kind,
				"status":         complaint.Status,
				"admin_response": complaint.AdminResponse,
				"responded_at":   complaint.RespondedAt,
				"created_at":     complaint.CreatedAt,
			},
		})
	}
}
