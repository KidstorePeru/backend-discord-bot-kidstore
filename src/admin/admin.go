package admin

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/types"
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func HandlerGetAllCustomers(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customers, err := db.GetAllCustomers(database)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo clientes"})
			return
		}
		if customers == nil { customers = []types.Customer{} }
		c.JSON(http.StatusOK, gin.H{"success": true, "customers": customers})
	}
}

func HandlerGetCustomer(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id inválido"})
			return
		}
		customer, err := db.GetCustomerByID(database, id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "cliente no encontrado"})
			return
		}
		recharges, _ := db.GetRechargesByCustomer(database, id)
		if recharges == nil { recharges = []types.KCRecharge{} }
		orders, _ := db.GetOrdersByCustomer(database, id)
		if orders == nil { orders = []types.Order{} }
		c.JSON(http.StatusOK, gin.H{
			"success": true, "customer": customer,
			"recharges": recharges, "orders": orders,
		})
	}
}

// HandlerUpdateCustomer — PUT /admin/customers/:id
// Permite editar epic_username, email y kc_balance de un cliente
func HandlerUpdateCustomer(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id inválido"})
			return
		}

		var req struct {
			EpicUsername *string `json:"epic_username"`
			Email        *string `json:"email"`
			KCBalance    *int    `json:"kc_balance"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		// Verificar que el cliente existe
		_, err = db.GetCustomerByID(database, id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "cliente no encontrado"})
			return
		}

		epic := ""
		if req.EpicUsername != nil { epic = strings.TrimSpace(*req.EpicUsername) }
		email := ""
		if req.Email != nil { email = strings.ToLower(strings.TrimSpace(*req.Email)) }

		if err := db.UpdateProfile(database, id, epic, email, ""); err != nil {
			if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
				c.JSON(http.StatusConflict, gin.H{"success": false, "error": "email o usuario Epic ya en uso"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error actualizando cliente"})
			return
		}

		// Actualizar balance KC si se especificó
		if req.KCBalance != nil {
			if _, err := database.Exec(`UPDATE customers SET kc_balance=$1, updated_at=NOW() WHERE id=$2`, *req.KCBalance, id); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error actualizando balance"})
				return
			}
		}

		db.AddAuditLog(database, &id, "ADMIN_CUSTOMER_UPDATED", "cliente actualizado por admin", c.ClientIP())
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "cliente actualizado correctamente"})
	}
}

// HandlerDeleteCustomer — DELETE /admin/customers/:id
// Desactiva (soft delete) o elimina definitivamente un cliente
func HandlerDeleteCustomer(database *sql.DB) gin.HandlerFunc {
    return func(c *gin.Context) {
        id, err := uuid.Parse(c.Param("id"))
        if err != nil {
            c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id inválido"})
            return
        }

        var epicUsername string
        err = database.QueryRow(`SELECT epic_username FROM customers WHERE id=$1`, id).Scan(&epicUsername)
        if err != nil {
            c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "cliente no encontrado"})
            return
        }

        if _, err := database.Exec(`DELETE FROM customers WHERE id=$1`, id); err != nil {
            c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error eliminando cliente"})
            return
        }

        db.AddAuditLog(database, nil, "ADMIN_CUSTOMER_DELETED",
            fmt.Sprintf("cliente %s eliminado permanentemente por admin", epicUsername), c.ClientIP())
        c.JSON(http.StatusOK, gin.H{"success": true, "message": "cliente eliminado correctamente"})
    }
}

func HandlerRechargeKC(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req types.RechargeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}
		customerID, err := uuid.Parse(req.CustomerID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "customer_id inválido"})
			return
		}

		approvedBy := strings.TrimSpace(c.GetHeader("X-Approved-By"))
		if approvedBy == "" { approvedBy = "admin" }

		if err := db.RechargeKC(database, customerID, req.AmountKC, req.AmountSoles, req.Note, approvedBy); err != nil {
			if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "inactive") {
				c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "cliente no encontrado o inactivo"})
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error recargando KC"})
			}
			return
		}

		customer, _ := db.GetCustomerByID(database, customerID)
		db.AddAuditLog(database, &customerID, "KC_RECHARGED",
			fmt.Sprintf("recarga de %d KC por %s", req.AmountKC, approvedBy), c.ClientIP())

		c.JSON(http.StatusOK, gin.H{
			"success": true, "message": "KC recargados correctamente",
			"new_balance": customer.KCBalance,
		})
	}
}

func HandlerGetAllOrders(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		orders, err := db.GetAllOrders(database)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo pedidos"})
			return
		}
		if orders == nil { orders = []types.Order{} }
		c.JSON(http.StatusOK, gin.H{"success": true, "orders": orders})
	}
}

func HandlerGetStats(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var totalCustomers, totalOrders, totalSent, totalPending int
		var totalKCRecharged sql.NullInt64

		database.QueryRow(`SELECT COUNT(*) FROM customers WHERE is_active=true`).Scan(&totalCustomers)
		database.QueryRow(`SELECT COUNT(*) FROM orders`).Scan(&totalOrders)
		database.QueryRow(`SELECT COUNT(*) FROM orders WHERE status='sent'`).Scan(&totalSent)
		database.QueryRow(`SELECT COUNT(*) FROM orders WHERE status='pending'`).Scan(&totalPending)
		database.QueryRow(`SELECT COALESCE(SUM(amount_kc),0) FROM kc_recharges`).Scan(&totalKCRecharged)

		c.JSON(http.StatusOK, gin.H{
			"success":            true,
			"total_customers":    totalCustomers,
			"total_orders":       totalOrders,
			"total_sent":         totalSent,
			"total_pending":      totalPending,
			"total_kc_recharged": totalKCRecharged.Int64,
		})
	}
}

func HandlerGetBotSchedule(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		schedule, err := db.GetBotSchedule(database)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo horario"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "schedule": schedule})
	}
}

func HandlerUpdateBotSchedule(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req types.BotScheduleRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}
		if req.Timezone == "" { req.Timezone = "America/Lima" }
		if err := db.UpdateBotSchedule(database, req.Enabled, req.StartHour, req.EndHour, req.Timezone); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}
		db.AddAuditLog(database, nil, "BOT_SCHEDULE_UPDATED",
			fmt.Sprintf("horario actualizado: enabled=%v %02d:00-%02d:00 %s",
				req.Enabled, req.StartHour, req.EndHour, req.Timezone),
			c.ClientIP())
		schedule, _ := db.GetBotSchedule(database)
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "horario actualizado correctamente", "schedule": schedule})
	}
}
