package admin

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"
	"KidStoreStore/src/middleware"
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

// adminActor identifica quién ejecuta una acción de administrador, para que
// el registro de auditoría deje de atribuir todo a la cadena genérica
// "admin" (o a X-Approved-By, un header que manda el propio cliente y que
// cualquiera con acceso de admin podía poner en cualquier valor). Cuando el
// acceso fue por JWT (Método 2 de AdminAuthMiddleware, ej. login con 2FA),
// el middleware ya dejó guardado el customer_id real y verificado — se usa
// para buscar un nombre legible. Cuando el acceso fue por la clave de API
// compartida (Método 1 — el método que usa hoy el panel de administración),
// no existe ninguna identidad individual que verificar: se etiqueta como
// tal en vez de inventar o confiar en un dato que el propio cliente puede
// mandar. Dar responsabilidad individual real a cada admin requiere que el
// panel inicie sesión con una cuenta propia en vez de la clave compartida —
// un cambio más grande que queda fuera de este arreglo puntual.
func adminActor(c *gin.Context, database *sql.DB) string {
	if customerIDStr, ok := middleware.GetCustomerID(c); ok {
		if id, err := uuid.Parse(customerIDStr); err == nil {
			if customer, err := db.GetCustomerByID(database, id); err == nil {
				return customer.EpicUsername
			}
		}
		return customerIDStr
	}
	return "clave de API compartida"
}

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

// maxManualKCAdjustment topa cuánto puede moverse el saldo de un cliente en
// un solo ajuste manual (edición directa en el panel, o /admin/recharge).
// Antes no había ningún tope — un cero de más al escribir el monto (o un
// uso indebido) acreditaba una cantidad arbitraria sin que el sistema lo
// cuestionara. 125 000 KC son 10 veces el paquete más grande que se vende
// hoy (Legend, 12 500 KC) — generoso para una corrección real, pero
// suficiente para frenar un error de tipeo evidente. Si este número no
// encaja con cómo se usa el panel en la práctica, es solo una constante:
// se ajusta acá.
const maxManualKCAdjustment = 125000

// HandlerUpdateCustomer — PUT /admin/customers/:id
// Permite editar epic_username, email, kc_balance e is_admin de un cliente.
func HandlerUpdateCustomer(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id inválido"})
			return
		}

		var req struct {
			EpicUsername   *string `json:"epic_username"`
			Email          *string `json:"email" binding:"omitempty,email"`
			KCBalance      *int    `json:"kc_balance"`
			KCBalanceNote  *string `json:"kc_balance_note"`
			IsAdmin        *bool   `json:"is_admin"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		customer, err := db.GetCustomerByID(database, id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "cliente no encontrado"})
			return
		}

		epic := ""
		if req.EpicUsername != nil { epic = strings.TrimSpace(*req.EpicUsername) }
		email := ""
		if req.Email != nil { email = strings.ToLower(strings.TrimSpace(*req.Email)) }
		actor := adminActor(c, database)

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
		if epic != "" || email != "" {
			db.AddAuditLog(database, &id, "ADMIN_CUSTOMER_UPDATED",
				fmt.Sprintf("datos del cliente editados por %s", actor), c.ClientIP())
		}

		// Ajustar balance KC si se especificó — antes esto era un SET directo
		// (UPDATE customers SET kc_balance=$1 ...) que pisaba el número sin
		// dejar ningún rastro de cuánto era antes, cuánto cambió, ni por qué
		// — y era, de hecho, la ÚNICA forma que tenía el panel web de bajar
		// el saldo de alguien (RechargeKC, la otra vía, solo suma). Ahora se
		// calcula la diferencia contra el balance real y se aplica a través
		// de los mismos mecanismos con ledger que ya usa la recarga manual
		// (RechargeKC para subir, DeductKCManual para bajar — el mismo que
		// ya usa el comando /kc remove de Discord), así que cualquier cambio
		// de saldo desde el panel queda siempre en el mismo registro
		// auditable, sin importar por qué puerta se hizo.
		if req.KCBalance != nil {
			if *req.KCBalance < 0 {
				c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "el balance KC no puede ser negativo"})
				return
			}
			delta := *req.KCBalance - customer.KCBalance
			if delta > maxManualKCAdjustment || delta < -maxManualKCAdjustment {
				c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": fmt.Sprintf("el ajuste (%+d KC) supera el máximo permitido de %d KC por operación", delta, maxManualKCAdjustment)})
				return
			}
			note := "ajuste manual desde edición de cliente"
			if req.KCBalanceNote != nil && strings.TrimSpace(*req.KCBalanceNote) != "" {
				note = strings.TrimSpace(*req.KCBalanceNote)
			}
			switch {
			case delta > 0:
				if _, err := db.RechargeKC(database, id, delta, nil, &note, actor, "manual_admin_edit"); err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error actualizando balance"})
					return
				}
			case delta < 0:
				if _, err := db.DeductKCManual(database, id, -delta); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
					return
				}
			}
			if delta != 0 {
				db.AddAuditLog(database, &id, "ADMIN_BALANCE_ADJUSTED",
					fmt.Sprintf("%s ajustó el balance de %d a %d KC (%+d) — nota: %s", actor, customer.KCBalance, *req.KCBalance, delta, note), c.ClientIP())
			}
		}

		// Otorgar/quitar rol admin — se separa del log genérico de arriba a
		// propósito: antes, cambiar is_admin pasaba por el mismo formulario
		// y el mismo log ("cliente actualizado por admin") que cambiar un
		// nombre de usuario, sin ninguna marca que distinguiera "esto fue un
		// cambio de permisos". Si una clave de admin se filtrara alguna vez,
		// así era fácil crear en silencio un segundo acceso permanente que
		// sobrevive aunque se rote la clave filtrada. Ahora queda su propia
		// entrada de auditoría, inconfundible.
		if req.IsAdmin != nil && *req.IsAdmin != customer.IsAdmin {
			if err := db.SetCustomerAdmin(database, id, *req.IsAdmin); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error actualizando rol"})
				return
			}
			action := "ADMIN_PRIVILEGE_REVOKED"
			verb := "quitó"
			if *req.IsAdmin {
				action = "ADMIN_PRIVILEGE_GRANTED"
				verb = "otorgó"
			}
			db.AddAuditLog(database, &id, action,
				fmt.Sprintf("%s %s el rol de administrador a %s", actor, verb, customer.EpicUsername), c.ClientIP())
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "message": "cliente actualizado correctamente"})
	}
}

// HandlerDeleteCustomer — DELETE /admin/customers/:id
//
// Antes esto borraba al cliente por completo con un DELETE directo. Como
// orders.customer_id NO tiene ON DELETE CASCADE (a propósito, para no
// perder historial de pedidos) pero payment_transactions/kc_recharges/
// slot_plays SÍ lo tienen, el resultado real dependía de qué cliente
// fuera: (a) uno con algún pedido → el DELETE fallaba con un error de
// restricción de llave foránea, sin forma de eliminarlo desde el panel; (b)
// uno sin pedidos pero que sí pagó o recargó → el DELETE funcionaba, pero
// borraba para siempre su historial de pagos y recargas — justo el tipo de
// registro financiero que un negocio necesita conservar. Ahora, en vez de
// eliminar, se desactiva (is_active=false) — el mismo mecanismo que ya usa
// la auto-eliminación de cuenta del propio cliente — dejando intacto todo
// su historial de pedidos/pagos/recargas para poder consultarlo después
// (soporte, disputas de pago, contabilidad). No se anonimizan sus datos
// (a diferencia de la auto-eliminación): un admin puede necesitar seguir
// viendo el correo/usuario real de una cuenta que desactivó, por ejemplo
// para investigar fraude.
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

		if err := db.DeactivateCustomerByAdmin(database, id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error desactivando cliente"})
			return
		}

		db.AddAuditLog(database, &id, "ADMIN_CUSTOMER_DEACTIVATED",
			fmt.Sprintf("cliente %s desactivado por %s — historial de pedidos/pagos conservado", epicUsername, adminActor(c, database)), c.ClientIP())
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "cliente desactivado correctamente — su historial se conserva"})
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
		// Antes "quién aprobó" salía de X-Approved-By, un header que manda
		// el propio cliente HTTP (hoy el panel siempre manda el literal fijo
		// "admin-panel") — no identificaba a nadie en particular y, al ser
		// un dato que el cliente controla, tampoco era confiable como
		// registro de auditoría. adminActor usa la identidad real verificada
		// por el middleware cuando el acceso fue por JWT, o etiqueta
		// honestamente "clave de API compartida" cuando no hay forma de
		// saber quién individualmente ejecutó la acción.
		approvedBy := adminActor(c, database)

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

// HandlerResolveOrderReview resuelve a mano un pedido que quedó en 'review'
// (entrega incierta tras una caída durante el envío — ver
// MarkOrderSendAttempted en db.go). El admin debe verificar directamente en
// Epic Games (buscando al receptor y revisando su inventario/historial de
// regalos) si el ítem llegó o no ANTES de elegir una acción: "delivered" si
// sí llegó (deja constancia de que es una confirmación manual, no la
// respuesta automática de Epic), "refund" si no llegó.
func HandlerResolveOrderReview(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "ID inválido"})
			return
		}
		var req struct {
			Action string `json:"action" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || (req.Action != "delivered" && req.Action != "refund") {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "acción inválida, usar: delivered o refund"})
			return
		}
		order, err := db.GetOrderByID(database, id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "pedido no encontrado"})
			return
		}
		if order.Status != "review" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "este pedido no está en revisión"})
			return
		}
		actor := adminActor(c, database)
		if err := db.ResolveReviewOrder(database, id, req.Action, actor); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error resolviendo el pedido: " + err.Error()})
			return
		}
		db.AddAuditLog(database, &order.CustomerID, "ORDER_REVIEW_RESOLVED",
			fmt.Sprintf("pedido %s resuelto manualmente por %s: %s", id, actor, req.Action), c.ClientIP())
		c.JSON(http.StatusOK, gin.H{"success": true})
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
// queja y avisa al consumidor por correo. Por Ley N° 29571 (modificada por
// la Ley N° 31435, vigente desde el 21/05/2022), el plazo máximo de
// respuesta es de 15 días hábiles improrrogables desde su presentación.
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
