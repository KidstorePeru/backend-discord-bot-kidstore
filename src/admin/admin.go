package admin

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"
	"KidStoreStore/src/store"
	"KidStoreStore/src/types"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func HandlerGetAllCustomers(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
		search := c.Query("search")
		customers, total, err := db.GetAllCustomers(database, page, limit, search)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo clientes"})
			return
		}
		if customers == nil { customers = []types.Customer{} }
		c.JSON(http.StatusOK, gin.H{"success": true, "customers": customers, "total": total, "page": page, "limit": limit})
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
		orders, _, _ := db.GetOrdersByCustomer(database, id, 1, 50)
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
			IsAdmin      *bool   `json:"is_admin"`
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

		if err := db.UpdateProfile(database, id, epic, "", nil); err != nil {
			if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
				c.JSON(http.StatusConflict, gin.H{"success": false, "error": "email o usuario Epic ya en uso"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error actualizando cliente"})
			return
		}
		if email != "" {
			if _, err := database.Exec(`UPDATE customers SET email=$1, email_changed_at=NOW(), updated_at=NOW() WHERE id=$2`, email, id); err != nil {
				if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
					c.JSON(http.StatusConflict, gin.H{"success": false, "error": "email o usuario Epic ya en uso"})
					return
				}
				c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error actualizando cliente"})
				return
			}
		}

		// Actualizar balance KC si se especificó
		if req.KCBalance != nil {
			if *req.KCBalance < 0 {
				c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "el balance KC no puede ser negativo"})
				return
			}
			if _, err := database.Exec(`UPDATE customers SET kc_balance=$1, updated_at=NOW() WHERE id=$2`, *req.KCBalance, id); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error actualizando balance"})
				return
			}
		}

		// Actualizar rol admin si se especificó
		if req.IsAdmin != nil {
			if err := db.SetCustomerAdmin(database, id, *req.IsAdmin); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error actualizando rol"})
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

		rechargeID, err := db.RechargeKC(database, customerID, req.AmountKC, req.AmountSoles, req.Note, approvedBy, "manual")
		if err != nil {
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
		discordbot.NotifyRecharge(customer, req.AmountKC, customer.KCBalance, "Manual (admin)")
		// Antes esta recarga solo avisaba por Discord (y solo si el cliente lo
		// tenia vinculado) — un cliente que paga por Yape/Plin sin Discord
		// nunca se enteraba de que ya se le acredito el KC.
		if customer.Email != nil && *customer.Email != "" {
			amountSoles := 0.0
			if req.AmountSoles != nil { amountSoles = *req.AmountSoles }
			productName := "Recarga manual de KC"
			if req.Note != nil && *req.Note != "" { productName = *req.Note }
			voucherURL := fmt.Sprintf("https://www.kidstoreperu.net/dashboard/comprobantes/recarga/%s", rechargeID)
			go store.SendPaymentApprovedEmail(store.GetSMTPConfig(), *customer.Email, productName, amountSoles, req.AmountKC, "Recarga manual", voucherURL, "es")
		}

		c.JSON(http.StatusOK, gin.H{
			"success": true, "message": "KC recargados correctamente",
			"new_balance": customer.KCBalance,
		})
	}
}

func HandlerGetAllOrders(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
		search := c.Query("search")
		status := c.Query("status")
		orders, total, err := db.GetAllOrders(database, page, limit, search, status)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo pedidos"})
			return
		}
		if orders == nil { orders = []types.Order{} }
		c.JSON(http.StatusOK, gin.H{"success": true, "orders": orders, "total": total, "page": page, "limit": limit})
	}
}

func HandlerGetAllPayments(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
		status := c.Query("status")
		payments, total, err := db.GetAllPaymentTransactions(database, page, limit, status)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo pagos"})
			return
		}
		if payments == nil { payments = []types.PaymentTransaction{} }
		c.JSON(http.StatusOK, gin.H{"success": true, "payments": payments, "total": total, "page": page, "limit": limit})
	}
}

// ==================== PRODUCT AVAILABILITY ====================

func HandlerGetProductAvailability(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		items, err := db.GetAllProductAvailability(database)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo disponibilidad"})
			return
		}
		if items == nil { items = []db.ProductAvailability{} }
		c.JSON(http.StatusOK, gin.H{"success": true, "items": items})
	}
}

func HandlerUpdateProductAvailability(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req db.ProductAvailability
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}
		if req.Timezone == "" { req.Timezone = "America/Lima" }
		if err := db.UpsertProductAvailability(database, req); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error guardando disponibilidad"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "disponibilidad actualizada"})
	}
}

func HandlerCheckProductAvailable(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		productID := c.Param("id")
		available := db.IsProductAvailable(database, productID)
		c.JSON(http.StatusOK, gin.H{"success": true, "available": available, "product_id": productID})
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

		// Revenue stats from payment_transactions
		var totalRevenuePEN, todayRevenuePEN, weekRevenuePEN, monthRevenuePEN sql.NullFloat64
		var totalPayments, approvedPayments, pendingPayments, failedPayments, expiredPayments, fulfilledPayments int
		database.QueryRow(`SELECT COALESCE(SUM(amount_pen),0) FROM payment_transactions WHERE status IN ('approved','fulfilled')`).Scan(&totalRevenuePEN)
		database.QueryRow(`SELECT COALESCE(SUM(amount_pen),0) FROM payment_transactions WHERE status IN ('approved','fulfilled') AND created_at >= CURRENT_DATE`).Scan(&todayRevenuePEN)
		database.QueryRow(`SELECT COALESCE(SUM(amount_pen),0) FROM payment_transactions WHERE status IN ('approved','fulfilled') AND created_at >= CURRENT_DATE - INTERVAL '7 days'`).Scan(&weekRevenuePEN)
		database.QueryRow(`SELECT COALESCE(SUM(amount_pen),0) FROM payment_transactions WHERE status IN ('approved','fulfilled') AND created_at >= CURRENT_DATE - INTERVAL '30 days'`).Scan(&monthRevenuePEN)
		database.QueryRow(`SELECT COUNT(*) FROM payment_transactions`).Scan(&totalPayments)
		database.QueryRow(`SELECT COUNT(*) FROM payment_transactions WHERE status='approved'`).Scan(&approvedPayments)
		database.QueryRow(`SELECT COUNT(*) FROM payment_transactions WHERE status='pending'`).Scan(&pendingPayments)
		database.QueryRow(`SELECT COUNT(*) FROM payment_transactions WHERE status='failed'`).Scan(&failedPayments)
		database.QueryRow(`SELECT COUNT(*) FROM payment_transactions WHERE status='expired'`).Scan(&expiredPayments)
		database.QueryRow(`SELECT COUNT(*) FROM payment_transactions WHERE status='fulfilled'`).Scan(&fulfilledPayments)

		// Gateway breakdown
		type gwStat struct {
			Gateway string  `json:"gateway"`
			Count   int     `json:"count"`
			Total   float64 `json:"total_pen"`
		}
		var gwStats []gwStat
		gwRows, _ := database.Query(`SELECT gateway, COUNT(*), COALESCE(SUM(amount_pen),0) FROM payment_transactions WHERE status IN ('approved','fulfilled') GROUP BY gateway ORDER BY SUM(amount_pen) DESC`)
		if gwRows != nil {
			defer gwRows.Close()
			for gwRows.Next() {
				var g gwStat
				gwRows.Scan(&g.Gateway, &g.Count, &g.Total)
				gwStats = append(gwStats, g)
			}
		}

		// Recent payments (last 5)
		type recentPay struct {
			ProductName string  `json:"product_name"`
			AmountPEN   float64 `json:"amount_pen"`
			Gateway     string  `json:"gateway"`
			Status      string  `json:"status"`
			CreatedAt   string  `json:"created_at"`
		}
		var recentPayments []recentPay
		rpRows, _ := database.Query(`SELECT product_name, amount_pen, gateway, status, created_at FROM payment_transactions ORDER BY created_at DESC LIMIT 5`)
		if rpRows != nil {
			defer rpRows.Close()
			for rpRows.Next() {
				var r recentPay
				rpRows.Scan(&r.ProductName, &r.AmountPEN, &r.Gateway, &r.Status, &r.CreatedAt)
				recentPayments = append(recentPayments, r)
			}
		}

		// New customers this week
		var newCustomersWeek int
		database.QueryRow(`SELECT COUNT(*) FROM customers WHERE is_active=true AND created_at >= CURRENT_DATE - INTERVAL '7 days'`).Scan(&newCustomersWeek)

		slotStats := gin.H{
			"all_time": fetchSlotPeriodStats(database, ""),
			"today":    fetchSlotPeriodStats(database, "CURRENT_DATE"),
			"week":     fetchSlotPeriodStats(database, "CURRENT_DATE - INTERVAL '7 days'"),
			"month":    fetchSlotPeriodStats(database, "CURRENT_DATE - INTERVAL '30 days'"),
			"top_winners": fetchSlotTopWinners(database),
		}

		c.JSON(http.StatusOK, gin.H{
			"success":            true,
			"total_customers":    totalCustomers,
			"new_customers_week": newCustomersWeek,
			"total_orders":       totalOrders,
			"total_sent":         totalSent,
			"total_pending":      totalPending,
			"total_kc_recharged": totalKCRecharged.Int64,
			// Revenue
			"revenue_total_pen":   totalRevenuePEN.Float64,
			"revenue_today_pen":   todayRevenuePEN.Float64,
			"revenue_week_pen":    weekRevenuePEN.Float64,
			"revenue_month_pen":   monthRevenuePEN.Float64,
			// Payment counts
			"total_payments":      totalPayments,
			"approved_payments":   approvedPayments,
			"pending_payments":    pendingPayments,
			"failed_payments":     failedPayments,
			"expired_payments":    expiredPayments,
			"fulfilled_payments":  fulfilledPayments,
			// Breakdowns
			"gateway_stats":      gwStats,
			"recent_payments":    recentPayments,
			// Slot (/slot en Discord) — ganancia/pérdida de la casa
			"slot_stats": slotStats,
		})
	}
}

// ==================== SLOT (/slot) — ESTADÍSTICAS DE LA CASA ====================
// Convención: cuando un cliente PIERDE, ese KC queda "ganado" para la casa
// (nunca sale de circulación a su favor). Cuando un cliente GANA, la casa le
// acredita el pago completo (payout_amount, que ya incluye devolverle lo
// apostado) — eso es lo que "pierde" la casa en cada jugada ganadora.

type slotPeriodStats struct {
	Plays      int     `json:"plays"`
	Wagered    int     `json:"wagered"`     // KC total apostado
	Gain       int     `json:"gain"`        // KC ganado por la casa (jugadas perdidas)
	Loss       int     `json:"loss"`        // KC pagado a ganadores (jugadas ganadas)
	Net        int     `json:"net"`         // gain - loss (positivo = a favor de la casa)
	Wins       int     `json:"wins"`
	WinRatePct float64 `json:"win_rate_pct"` // tasa de victoria real observada
}

func fetchSlotPeriodStats(database *sql.DB, since string) slotPeriodStats {
	var s slotPeriodStats
	query := `
		SELECT
			COUNT(*),
			COALESCE(SUM(bet_amount),0),
			COALESCE(SUM(bet_amount) FILTER (WHERE NOT won),0),
			COALESCE(SUM(payout_amount) FILTER (WHERE won),0),
			COALESCE(SUM(CASE WHEN won THEN 1 ELSE 0 END),0)
		FROM slot_plays`
	if since != "" {
		query += ` WHERE created_at >= ` + since
	}
	database.QueryRow(query).Scan(&s.Plays, &s.Wagered, &s.Gain, &s.Loss, &s.Wins)
	s.Net = s.Gain - s.Loss
	if s.Plays > 0 {
		s.WinRatePct = float64(s.Wins) / float64(s.Plays) * 100
	}
	return s
}

// fetchSlotTopWinners lista a los clientes que más KC neto le han ganado a
// la casa jugando /slot (pagado - apostado), para detectar rachas de suerte
// o comportamiento sospechoso.
func fetchSlotTopWinners(database *sql.DB) []gin.H {
	rows, err := database.Query(`
		SELECT c.epic_username, c.id,
			COUNT(*) as plays,
			COALESCE(SUM(sp.payout_amount) FILTER (WHERE sp.won),0) - COALESCE(SUM(sp.bet_amount),0) as net_kc
		FROM slot_plays sp
		JOIN customers c ON c.id = sp.customer_id
		GROUP BY c.id, c.epic_username
		HAVING COALESCE(SUM(sp.payout_amount) FILTER (WHERE sp.won),0) - COALESCE(SUM(sp.bet_amount),0) > 0
		ORDER BY net_kc DESC
		LIMIT 5`)
	if err != nil {
		return []gin.H{}
	}
	defer rows.Close()
	result := []gin.H{}
	for rows.Next() {
		var epicUsername string
		var customerID uuid.UUID
		var plays, netKC int
		if err := rows.Scan(&epicUsername, &customerID, &plays, &netKC); err == nil {
			result = append(result, gin.H{
				"epic_username": epicUsername,
				"customer_id":   customerID,
				"plays":         plays,
				"net_kc":        netKC,
			})
		}
	}
	return result
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

// ==================== PAYMENT ADMIN ACTIONS ====================

func HandlerUpdatePayment(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "ID inválido"})
			return
		}

		var req struct {
			Status string `json:"status" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		validStatuses := map[string]bool{"approved": true, "failed": true, "expired": true, "fulfilled": true}
		if !validStatuses[req.Status] {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "Status inválido. Usar: approved, failed, expired, fulfilled"})
			return
		}

		// Verify payment exists
		payment, err := db.GetPaymentByID(database, id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "Pago no encontrado"})
			return
		}

		if req.Status == "approved" || req.Status == "fulfilled" {
			// "Aprobar" debe acreditar el KC de verdad (mismo camino que un
			// webhook real: email, notificación de Discord, todo) — antes esto
			// solo le cambiaba la etiqueta al pago sin darle el KC al cliente,
			// lo que dejaba pagos marcados "aprobados" que en realidad nunca se
			// acreditaron.
			if err := store.ProcessApprovedPayment(database, id); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
				return
			}
			if req.Status == "fulfilled" {
				if err := db.AdminUpdatePaymentStatus(database, id, "fulfilled"); err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
					return
				}
			}
		} else {
			if err := db.AdminUpdatePaymentStatus(database, id, req.Status); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
				return
			}
		}

		db.AddAuditLog(database, nil, "ADMIN_PAYMENT_UPDATED",
			fmt.Sprintf("payment %s: %s → %s (%s)", id.String()[:8], payment.Status, req.Status, payment.ProductName),
			c.ClientIP())

		c.JSON(http.StatusOK, gin.H{"success": true, "message": fmt.Sprintf("Pago actualizado a %s", req.Status)})
	}
}

func HandlerDeletePayment(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "ID inválido"})
			return
		}

		payment, err := db.GetPaymentByID(database, id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "Pago no encontrado"})
			return
		}

		if err := db.DeletePayment(database, id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": err.Error()})
			return
		}

		db.AddAuditLog(database, nil, "ADMIN_PAYMENT_DELETED",
			fmt.Sprintf("payment %s deleted (%s, %s, S/%.2f)", id.String()[:8], payment.ProductName, payment.Status, payment.AmountPEN),
			c.ClientIP())

		c.JSON(http.StatusOK, gin.H{"success": true, "message": "Pago eliminado"})
	}
}

// HandlerAdminCheck returns whether the authenticated user is an admin.
func HandlerAdminCheck(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"success": true, "is_admin": true})
	}
}

// ==================== LIBRO DE RECLAMACIONES (ADMIN) ====================

func HandlerGetAllComplaints(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
		complaints, total, err := db.GetAllComplaints(database, page, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo reclamos"})
			return
		}
		if complaints == nil { complaints = []types.ConsumerComplaint{} }
		c.JSON(http.StatusOK, gin.H{"success": true, "complaints": complaints, "total": total, "page": page, "limit": limit})
	}
}

// HandlerRespondComplaint registra la respuesta del negocio a un reclamo o
// queja y avisa al consumidor por correo. Por el reglamento de INDECOPI, el
// plazo máximo de respuesta es de 30 días calendario desde su presentación.
func HandlerRespondComplaint(database *sql.DB, cfg types.EnvConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "ID inválido"})
			return
		}
		var req struct {
			Response string `json:"response" binding:"required,min=3,max=3000"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}
		complaint, err := db.GetComplaintByID(database, id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "reclamo no encontrado"})
			return
		}
		if err := db.RespondToComplaint(database, id, req.Response); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error guardando la respuesta"})
			return
		}
		db.AddAuditLog(database, nil, "COMPLAINT_RESPONDED", "reclamo "+complaint.Reference+" respondido", c.ClientIP())
		go store.SendComplaintRespondedEmail(cfg, complaint.Email, complaint.FullName, complaint.Reference, req.Response, "es")
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "respuesta enviada"})
	}
}

func HandlerCloseComplaint(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "ID inválido"})
			return
		}
		if err := db.CloseComplaint(database, id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error cerrando el reclamo"})
			return
		}
		db.AddAuditLog(database, nil, "COMPLAINT_CLOSED", id.String(), c.ClientIP())
		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}
