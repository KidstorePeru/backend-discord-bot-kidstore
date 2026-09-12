package store

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"
	"KidStoreStore/src/fortnite"
	"KidStoreStore/src/middleware"
	"KidStoreStore/src/safe"
	"KidStoreStore/src/types"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ==================== PAYMENT INFO ====================

var paymentInfoJSON string

func SetPaymentInfoJSON(json string) {
	paymentInfoJSON = json
}

func HandlerGetPaymentInfo() gin.HandlerFunc {
	return func(c *gin.Context) {
		if paymentInfoJSON == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "error": "payment info not configured"})
			return
		}
		c.Data(http.StatusOK, "application/json", []byte(paymentInfoJSON))
	}
}

// ==================== EXCHANGE RATES ====================

type ratesCacheEntry struct {
	body      []byte
	fetchedAt time.Time
}

var (
	ratesCacheMu  sync.RWMutex
	ratesCacheVal *ratesCacheEntry
	ratesTTL      = 24 * time.Hour
	ratesClient   = &http.Client{Timeout: 10 * time.Second}
)

var exchangeRateAPIKey string

func SetExchangeRateAPIKey(key string) {
	exchangeRateAPIKey = key
}

// fallbackRates se usa cuando no hay API key configurada o la API falla y
// tampoco hay nada en caché — deja el sitio funcional (aunque con tasas
// desactualizadas) en vez de romper precios/pagos.
var fallbackRates = map[string]float64{"PEN": 1, "USD": 0.27, "EUR": 0.25}

// currentConversionRates devuelve el mapa de conversion (1 PEN = X <divisa>)
// usando el mismo caché de 24h que HandlerGetExchangeRates — la usan tanto
// el endpoint público como los cobros de dLocal Go (que necesitan saber
// cuánto es el precio en la divisa real del cliente).
func currentConversionRates() map[string]float64 {
	ratesCacheMu.RLock()
	cached := ratesCacheVal
	ratesCacheMu.RUnlock()

	if cached != nil && time.Since(cached.fetchedAt) < ratesTTL {
		var parsed struct {
			Rates map[string]float64 `json:"rates"`
		}
		if json.Unmarshal(cached.body, &parsed) == nil && len(parsed.Rates) > 0 {
			return parsed.Rates
		}
	}

	if exchangeRateAPIKey == "" {
		return fallbackRates
	}

	apiURL := fmt.Sprintf("https://v6.exchangerate-api.com/v6/%s/latest/PEN", exchangeRateAPIKey)
	resp, err := ratesClient.Get(apiURL)
	if err != nil {
		if cached != nil {
			var parsed struct{ Rates map[string]float64 `json:"rates"` }
			if json.Unmarshal(cached.body, &parsed) == nil {
				return parsed.Rates
			}
		}
		return fallbackRates
	}
	defer resp.Body.Close()

	var apiResp struct {
		Result          string             `json:"result"`
		ConversionRates map[string]float64 `json:"conversion_rates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil || apiResp.Result != "success" {
		return fallbackRates
	}

	// "rates" trae las ~160 divisas que devuelve la API (1 PEN = X divisa) —
	// se usa para mostrar el precio de referencia en la moneda local del
	// cliente y para cobrar con dLocal Go en esa misma divisa. USD/EUR se
	// mantienen también en el nivel superior por compatibilidad con el
	// código existente que ya los usa directo.
	result := gin.H{
		"USD":       apiResp.ConversionRates["USD"],
		"EUR":       apiResp.ConversionRates["EUR"],
		"rates":     apiResp.ConversionRates,
		"fetchedAt": time.Now().UnixMilli(),
	}
	body, _ := json.Marshal(result)

	ratesCacheMu.Lock()
	ratesCacheVal = &ratesCacheEntry{body: body, fetchedAt: time.Now()}
	ratesCacheMu.Unlock()

	return apiResp.ConversionRates
}

func HandlerGetExchangeRates(c *gin.Context) {
	rates := currentConversionRates()
	ratesCacheMu.RLock()
	cached := ratesCacheVal
	ratesCacheMu.RUnlock()
	if cached != nil {
		c.Data(http.StatusOK, "application/json", cached.body)
		return
	}
	c.JSON(http.StatusOK, gin.H{"USD": rates["USD"], "EUR": rates["EUR"], "rates": rates, "fetchedAt": 0})
}

// ==================== CACHÉ DE TIENDA ====================

type shopCacheEntry struct {
	body      []byte
	fetchedAt time.Time
}

var (
	shopCacheMu sync.RWMutex
	shopCache   = map[string]*shopCacheEntry{}
	shopTTL     = 5 * time.Minute
	shopClient  = &http.Client{Timeout: 10 * time.Second}
)

// fetchShopBody devuelve el JSON crudo de la tienda actual de Fortnite (desde
// caché si sigue fresco, o pidiéndolo a fortnite-api.com si no) — lo usan
// tanto el endpoint público /store/shop como la verificación de precios al
// crear un pedido, para que ambos vean siempre los mismos datos.
func fetchShopBody(ctx context.Context, lang string) ([]byte, error) {
	shopCacheMu.RLock()
	entry, ok := shopCache[lang]
	shopCacheMu.RUnlock()
	if ok && time.Since(entry.fetchedAt) < shopTTL {
		return entry.body, nil
	}

	url := fmt.Sprintf("https://fortnite-api.com/v2/shop?language=%s", lang)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("error preparando request: %w", err)
	}

	resp, err := shopClient.Do(req)
	if err != nil {
		if ok { return entry.body, nil } // stale cache es mejor que nada
		return nil, fmt.Errorf("error obteniendo tienda: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error leyendo respuesta: %w", err)
	}

	shopCacheMu.Lock()
	shopCache[lang] = &shopCacheEntry{body: body, fetchedAt: time.Now()}
	shopCacheMu.Unlock()

	return body, nil
}

func HandlerGetShop(c *gin.Context) {
	lang := c.Query("lang")
	if lang == "" { lang = "es-419" }
	if lang != "es-419" && lang != "en" { lang = "es-419" }

	body, err := fetchShopBody(c.Request.Context(), lang)
	if err != nil {
		// Ruta publica, sin autenticar — nunca devolver el error crudo (podria
		// traer detalles internos de red/infraestructura), solo loguearlo.
		slog.Error("HandlerGetShop: error obteniendo tienda", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudo obtener la tienda, intenta de nuevo"})
		return
	}
	c.Data(http.StatusOK, "application/json", body)
}

// shopItem — lo que realmente sabemos de un item de la tienda, sacado de la
// API de Fortnite, no de lo que mande el cliente.
type shopItem struct {
	Name        string
	Image       string
	FinalPrice  int
}

// resolveShopItem busca un offerId en la tienda actual y devuelve sus datos
// REALES (precio, nombre e imagen). Nunca hay que confiar en lo que manda el
// cliente al crear un pedido — ni el precio, ni el nombre, ni la imagen —
// cualquiera podría interceptar la petición y, además de intentar pagar de
// menos, meter texto/HTML arbitrario en item_name que después se muestra tal
// cual en el correo de confirmación. Esta es la única fuente de verdad.
func resolveShopItem(ctx context.Context, offerID string) (shopItem, error) {
	body, err := fetchShopBody(ctx, "es-419")
	if err != nil {
		return shopItem{}, err
	}
	var parsed struct {
		Data struct {
			Entries []struct {
				OfferID    string `json:"offerId"`
				FinalPrice int    `json:"finalPrice"`
				Bundle     *struct {
					Name string `json:"name"`
				} `json:"bundle"`
				BrItems []struct {
					Name   string `json:"name"`
					Images struct {
						Featured  string `json:"featured"`
						Icon      string `json:"icon"`
						SmallIcon string `json:"smallIcon"`
					} `json:"images"`
				} `json:"brItems"`
				Tracks []struct {
					Title string `json:"title"`
				} `json:"tracks"`
			} `json:"entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return shopItem{}, fmt.Errorf("respuesta de tienda inesperada: %w", err)
	}
	for _, e := range parsed.Data.Entries {
		if e.OfferID != offerID {
			continue
		}
		item := shopItem{FinalPrice: e.FinalPrice}
		switch {
		case e.Bundle != nil:
			item.Name = e.Bundle.Name
		case len(e.BrItems) > 0:
			item.Name = e.BrItems[0].Name
			item.Image = e.BrItems[0].Images.Featured
			if item.Image == "" { item.Image = e.BrItems[0].Images.Icon }
			if item.Image == "" { item.Image = e.BrItems[0].Images.SmallIcon }
		case len(e.Tracks) > 0:
			item.Name = e.Tracks[0].Title
		}
		if item.Name == "" { item.Name = "Item" }
		return item, nil
	}
	return shopItem{}, fmt.Errorf("item no encontrado en la tienda actual")
}

// alreadyOwnedResolution es la decisión pura (sin tocar DB ni red) que toma
// processOrder cuando Epic responde ErrAlreadyOwned al intentar enviar un
// regalo — separada del resto de la función para poder probarla con un
// test unitario simple, sin necesitar una base de datos real:
//
//   - wasAlreadyAttempting=false (nunca se había intentado enviar ESTE
//     pedido antes): el receptor obtuvo el ítem por su cuenta, ANTES de
//     este pedido — nuestro pedido nunca lo entregó. Resolución:
//     "refund_not_delivered" (reembolsar, nunca marcar "sent").
//   - wasAlreadyAttempting=true (había un intento de ESTE pedido
//     interrumpido, ver MarkOrderSendAttempted): no se puede confirmar si
//     ese intento interrumpido fue el que entregó el ítem. Resolución:
//     "needs_review" (ni se inventa evidencia de entrega, ni se reembolsa
//     a ciegas arriesgando un reembolso duplicado).
func alreadyOwnedResolution(wasAlreadyAttempting bool) string {
	if wasAlreadyAttempting {
		return "needs_review"
	}
	return "refund_not_delivered"
}

// ==================== CREAR PEDIDO ====================

const maxPendingOrdersPerCustomer = 10

// friendGracePeriod — ver el comentario en processOrder, caso "el receptor
// no es amigo de ningún bot". 10 minutos cubren al menos un ciclo completo
// del aceptador automático de solicitudes de amistad (corre cada 5 min).
const friendGracePeriod = 10 * time.Minute

func HandlerCreateOrder(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, err := uuid.Parse(customerIDStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id inválido"})
			return
		}

		var req types.CreateOrderRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		// ── Verificar el item contra la tienda real ──
		// El cliente podría manipular price_kc/price_vbucks/item_name/item_image
		// directamente en la petición (editando la request desde el navegador)
		// para pedir un item caro pagando casi nada — el bot igual gastaría sus
		// VBucks reales enviándolo — o para meter texto/HTML arbitrario en
		// item_name, que después se muestra tal cual en el correo de
		// confirmación. Nunca hay que confiar en nada de esto: se reemplaza
		// todo por los datos reales de fortnite-api.com.
		item, err := resolveShopItem(c.Request.Context(), req.ItemOfferID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "no se pudo verificar el item en la tienda actual: " + err.Error()})
			return
		}
		// 1 VBuck = 1 KC (igual que vbucksToKC en el frontend) — si esa tasa
		// cambia algún día, hay que actualizarla también acá.
		expectedKC := req.PriceVBucks
		if item.FinalPrice != req.PriceVBucks || req.PriceKC != expectedKC {
			slog.Warn("Posible manipulación de precio en pedido bloqueada",
				"customer", customerID, "offerID", req.ItemOfferID,
				"price_kc_reclamado", req.PriceKC, "price_vbucks_reclamado", req.PriceVBucks, "ip", c.ClientIP())
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "el precio del item no coincide con la tienda actual"})
			return
		}
		// Nunca se usa el item_image que mande el cliente — solo el real del
		// catálogo, o vacío (las plantillas ya manejan ese caso con un ícono
		// de reemplazo).
		req.ItemName = item.Name
		req.ItemImage = item.Image

		// ── Verificar horario ──
		inSchedule, scheduleReason := db.IsWithinSchedule(database)
		if !inSchedule {
			schedule, _ := db.GetBotSchedule(database)
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"success": false,
				"error":   fmt.Sprintf("Los bots están fuera de su horario de trabajo (%02d:00 - %02d:00 %s). Por favor intenta durante ese horario.", schedule.StartHour, schedule.EndHour, schedule.Timezone),
				"code": "BOTS_OFFLINE", "start_hour": schedule.StartHour, "end_hour": schedule.EndHour,
				"timezone": schedule.Timezone, "reason": scheduleReason,
			})
			return
		}

		customer, err := db.GetCustomerByID(database, customerID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "cliente no encontrado"})
			return
		}

		// ── Verificar saldo ──
		if customer.KCBalance < req.PriceKC {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": fmt.Sprintf("KC insuficientes: tienes %d KC, necesitas %d KC", customer.KCBalance, req.PriceKC)})
			return
		}

		// ── Límite de pedidos pendientes por cliente (máx. 10) — la cuenta
		// se hace ahora dentro de la misma transacción que descuenta el
		// saldo y crea el pedido (ver DeductKCAndCreateOrder), para que dos
		// compras simultáneas del mismo cliente no puedan las dos pasar la
		// comprobación antes de que cualquiera inserte su pedido. ──
		order, err := db.DeductKCAndCreateOrder(database, customerID, customer.EpicUsername, req, maxPendingOrdersPerCustomer)
		if err != nil {
			if strings.Contains(err.Error(), "insufficient") {
				c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			} else if strings.Contains(err.Error(), "too many pending orders") {
				c.JSON(http.StatusTooManyRequests, gin.H{
					"success": false,
					"error":   fmt.Sprintf("Tienes %d pedidos pendientes. Espera a que se procesen antes de crear nuevos (máximo %d).", maxPendingOrdersPerCustomer, maxPendingOrdersPerCustomer),
					"code":    "TOO_MANY_ORDERS",
				})
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error creando pedido"})
			}
			return
		}

		db.AddAuditLog(database, &customerID, "ORDER_CREATED",
			fmt.Sprintf("pedido %s: %s por %d KC (%d VBucks)", order.ID, req.ItemName, req.PriceKC, req.PriceVBucks), c.ClientIP())

		c.JSON(http.StatusOK, gin.H{"success": true, "order": order, "message": "pedido creado, procesando envío..."})
	}
}

// ==================== MIS PEDIDOS ====================

func HandlerGetMyOrders(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, err := uuid.Parse(customerIDStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id inválido"})
			return
		}
		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
		orders, total, err := db.GetOrdersByCustomer(database, customerID, page, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo pedidos"})
			return
		}
		if orders == nil { orders = []types.Order{} }
		c.JSON(http.StatusOK, gin.H{"success": true, "orders": orders, "total": total, "page": page, "limit": limit})
	}
}

// HandlerGetMyOrderStats devuelve totales calculados directamente en la
// base de datos (no sobre una página de resultados ya recortada) — /perfil
// los usaba mal: mostraba len(orders) de un solo lote (como mucho 100
// pedidos, antes ni eso por el bug del límite) como si fuera el total real
// del cliente. GetCustomerOrderStats ya existía y la usaba el bot de
// Discord, pero nunca se había expuesto por HTTP para el sitio web.
func HandlerGetMyOrderStats(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, err := uuid.Parse(customerIDStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id inválido"})
			return
		}
		total, sent, pending, spentKC, err := db.GetCustomerOrderStats(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo estadísticas"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "total_orders": total, "sent_orders": sent, "pending_orders": pending, "total_spent_kc": spentKC})
	}
}

// HandlerGetMyRechargeStats — mismo problema, mismo remedio que
// HandlerGetMyOrderStats pero para recargas: el dashboard sumaba amount_pen
// de TODOS los pagos por pasarela sin filtrar por estado (incluía
// pendientes y fallidos como si fueran plata realmente cobrada) y además
// solo sobre los últimos 50 pagos cargados. GetCustomerRechargeStats
// calcula ambos números correctamente sobre todo el historial.
func HandlerGetMyRechargeStats(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, err := uuid.Parse(customerIDStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id inválido"})
			return
		}
		totalKC, totalPEN, pendingPayments, err := db.GetCustomerRechargeStats(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo estadísticas"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "total_kc_recharged": totalKC, "total_pen_recharged": totalPEN, "pending_payments": pendingPayments})
	}
}

// ==================== COMPROBANTE DE PEDIDO ====================

// HandlerOrderVoucher devuelve los datos para la página de comprobante de un
// pedido — solo si ya se entregó ("sent") y le pertenece al cliente autenticado.
func HandlerOrderVoucher(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, err := uuid.Parse(customerIDStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id inválido"})
			return
		}
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id inválido"})
			return
		}
		order, err := db.GetOrderByID(database, id)
		if err != nil || order.CustomerID != customerID {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "comprobante no encontrado"})
			return
		}
		if order.Status != "sent" {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "este pedido todavía no tiene comprobante"})
			return
		}
		customer, err := db.GetCustomerByID(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo cliente"})
			return
		}
		itemImage := ""
		if order.ItemImage != nil { itemImage = *order.ItemImage }
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"voucher": gin.H{
				"type":          "order",
				"reference":     strings.ToUpper(id.String()[:8]),
				"customer_name": customer.EpicUsername,
				"item_name":     order.ItemName,
				"item_image":    itemImage,
				"epic_username": order.EpicUsername,
				"price_kc":      order.PriceKC,
				"price_vbucks":  order.PriceVBucks,
				"status":        order.Status,
				"created_at":    order.CreatedAt,
				// delivery_confirmed: true si Epic Games confirmó el envío con
				// su propia respuesta (evidencia guardada server-side). No se
				// expone el JSON crudo al cliente, solo la confirmación —
				// el detalle técnico queda para uso interno en disputas de pago.
				"delivery_confirmed": order.DeliveryEvidence != nil,
				"delivered_at":       order.UpdatedAt,
			},
		})
	}
}

// ==================== WORKER ====================

var encryptionKey string

func SetEncryptionKey(key string) {
	encryptionKey = key
}

func StartOrderWorker(ctx context.Context, database *sql.DB) {
	slog.Info("Worker: Iniciando cola de envíos")
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				slog.Info("Worker: Detenido")
				return
			case <-ticker.C:
				inSchedule, reason := db.IsWithinSchedule(database)
				if !inSchedule { slog.Info("Worker: pausado", "reason", reason); continue }
				safe.Run("StartOrderWorker.processOrders", func() { processOrders(database) })
			}
		}
	}()
}

func processOrders(database *sql.DB) {
	// ClaimPendingOrders (no GetPendingOrders) — reclama los pedidos de forma
	// atómica (FOR UPDATE SKIP LOCKED) para que, si local y producción llegan
	// a correr al mismo tiempo contra la misma base compartida, nunca puedan
	// tomar y enviar el mismo pedido dos veces.
	orders, err := db.ClaimPendingOrders(database)
	if err != nil || len(orders) == 0 { return }

	accounts, err := db.GetActiveGameAccounts(database, encryptionKey)
	if err != nil || len(accounts) == 0 {
		slog.Warn("Worker: no hay cuentas bot activas disponibles")
		discordbot.AlertNoActiveBots(len(orders))
		noBotsMsg := "Sin cuentas bot activas disponibles."
		for _, order := range orders {
			failOrderAndRefund(database, order, noBotsMsg, "No había cuentas disponibles para procesar tu pedido en ese momento.")
		}
		return
	}

	for _, order := range orders {
		o := order
		// Un panic al procesar UN pedido (ej. datos raros del cliente) no
		// debe frenar el resto del lote de este mismo ciclo.
		safe.Run("processOrders.processOrder", func() { processOrder(database, o, accounts) })
	}
}

// failOrderAndRefund centraliza lo que antes estaba repetido (con
// variaciones inconsistentes) en cada punto donde un pedido falla: marca el
// pedido fallido, intenta reembolsar el KC, y SOLO afirma en el audit log y
// en el correo al cliente que "ya se reembolsó" cuando el reembolso
// realmente se confirmó. Antes, algunas ramas ni siquiera revisaban el
// error de RefundOrder y notificaban éxito igual — si el reembolso fallaba
// (ej. un error transitorio de DB), el pedido quedaba 'failed' sin su KC
// devuelto, pero el cliente recibía un correo diciendo "ya te devolvimos el
// KC completo". El status del pedido queda como señal honesta de si el
// reembolso se completó: RefundOrder deja el pedido en 'refunded' recién
// cuando de verdad se ejecuta — si falla, el pedido se queda en 'failed'
// (RefundOrder no vuelve a rechazarlo por eso, así que es seguro
// reintentarlo más tarde — ver RetryFailedRefunds).
func failOrderAndRefund(database *sql.DB, order types.Order, internalReason, customerReason string) {
	db.UpdateOrderStatus(database, order.ID, "failed", nil, &internalReason)
	refundErr := db.RefundOrder(database, order.ID)
	refunded := refundErr == nil

	if refunded {
		db.AddAuditLog(database, &order.CustomerID, "ORDER_FAILED",
			fmt.Sprintf("pedido %s: %s — KC reembolsados", order.ID, internalReason), "worker")
	} else {
		slog.Error("Worker: no se pudo reembolsar el pedido, queda pendiente de reintento", "orderID", order.ID, "error", refundErr)
		db.AddAuditLog(database, &order.CustomerID, "ORDER_FAILED",
			fmt.Sprintf("pedido %s: %s — REEMBOLSO FALLÓ (%s), pendiente de reintento automático", order.ID, internalReason, refundErr), "worker")
	}

	notifyOrderFailed(database, order, customerReason, refunded)
}

// RetryFailedRefunds reintenta el reembolso de pedidos que quedaron en
// 'failed' sin que su devolución de KC se haya confirmado nunca (ver
// failOrderAndRefund) — la red de recuperación para el caso que el punto 11
// pedía explícitamente: "permite recuperar devoluciones pendientes". Se
// llama periódicamente desde main.go. Seguro de reintentar cualquier
// cantidad de veces: RefundOrder por sí mismo rechaza reembolsar un pedido
// que ya esté 'refunded' (o 'sent'), así que nunca duplica el KC devuelto.
func RetryFailedRefunds(database *sql.DB) {
	orders, err := db.GetOrdersPendingRefund(database)
	if err != nil {
		slog.Error("RetryFailedRefunds: error listando pedidos", "error", err)
		return
	}
	for _, order := range orders {
		o := order
		safe.Run("RetryFailedRefunds.order", func() {
			if err := db.RefundOrder(database, o.ID); err != nil {
				slog.Warn("RetryFailedRefunds: reembolso sigue fallando, se reintentará más tarde", "orderID", o.ID, "error", err)
				return
			}
			slog.Info("RetryFailedRefunds: reembolso pendiente recuperado", "orderID", o.ID, "customer", o.CustomerID)
			db.AddAuditLog(database, &o.CustomerID, "ORDER_REFUND_RECOVERED",
				fmt.Sprintf("pedido %s: reembolso pendiente se completó en un reintento automático", o.ID), "worker")
		})
	}
}

// notifyOrderFailed avisa por correo que un pedido no se pudo completar —
// "refunded" refleja si el KC de verdad ya se devolvió (nunca se afirma sin
// confirmarlo primero, ver failOrderAndRefund). "reason" debe ser un texto
// ya pensado para el cliente, no el error técnico crudo.
func notifyOrderFailed(database *sql.DB, order types.Order, reason string, refunded bool) {
	customer, err := db.GetCustomerByID(database, order.CustomerID)
	if err != nil {
		return
	}
	if customer.Email != nil && *customer.Email != "" {
		itemImage := ""
		if order.ItemImage != nil { itemImage = *order.ItemImage }
		go SendOrderFailedEmail(smtpConfig, *customer.Email, order.EpicUsername, order.ItemName, itemImage, order.PriceKC, reason, refunded, "es")
	}
}

// processOrder intenta enviar un pedido probando cada bot disponible en orden.
// Si un bot falla por gift_limit_reached o token inválido, pasa al siguiente bot
// en el mismo ciclo sin esperar 30 segundos.
func processOrder(database *sql.DB, order types.Order, accounts []types.GameAccount) {
	// Verificar que al menos un bot tiene slots
	hasSlots := false
	for i := range accounts {
		if accounts[i].RemainingGifts > 0 { hasSlots = true; break }
	}
	if !hasSlots {
		noSlotsMsg := "Todas las cuentas bot han agotado sus envíos del día. Los gifts se resetean diariamente."
		slog.Warn("Worker: sin slots en ningún bot", "orderID", order.ID)
		discordbot.AlertNoActiveBots(1)
		db.UpdateOrderStatus(database, order.ID, "pending", nil, &noSlotsMsg)
		return
	}

	// (El pedido ya quedó marcado "processing" al reclamarlo en ClaimPendingOrders.)

	// Obtener el Epic account ID del receptor (igual para todos los bots, basta con uno)
	var receiverAccountID string
	for i := range accounts {
		if accounts[i].RemainingGifts <= 0 { continue }
		id, err := fortnite.GetReceiverAccountID(database, accounts[i], order.EpicUsername)
		if err != nil {
			errMsg := fmt.Sprintf("no se encontró el usuario Epic '%s': %s", order.EpicUsername, err.Error())
			slog.Error("Worker: usuario no encontrado", "orderID", order.ID, "msg", errMsg)
			failOrderAndRefund(database, order, errMsg, fmt.Sprintf("No pudimos encontrar la cuenta de Epic Games '%s'. Verifica que el usuario esté bien escrito.", order.EpicUsername))
			return
		}
		receiverAccountID = id
		break
	}

	// Defensive: receiverAccountID must be set at this point (loop above either sets
	// it or returns early on error). Guard against unexpected empty string.
	if receiverAccountID == "" {
		noSlotsMsg := "no se pudo resolver el ID de la cuenta Epic del receptor"
		slog.Error("Worker: receiverAccountID vacío inesperadamente", "orderID", order.ID)
		db.UpdateOrderStatus(database, order.ID, "pending", nil, &noSlotsMsg)
		return
	}

	// Contadores para determinar el resultado final si todos los bots fallan
	activeBots := 0
	notFriendBots := 0
	anyGiftLimit := false
	insufficientFundsBots := 0

	// ── Loop interno: probar cada bot en orden ──
	for i := range accounts {
		bot := &accounts[i]
		if bot.RemainingGifts <= 0 { continue }
		// No intentar con un bot que no tiene suficientes V-Bucks reales para
		// pagar este item — sin esto, Epic rechazaría la compra y el pedido
		// se marcaría como fallido de inmediato en vez de probar el siguiente
		// bot que sí podría tener fondos. bot.VBucks viene del sync automático
		// contra la API real de Epic (ver healthcheck.go), así que es confiable.
		if bot.VBucks < order.PriceVBucks {
			slog.Info("Worker: bot sin V-Bucks suficientes, probando siguiente",
				"bot", bot.DisplayName, "tiene", bot.VBucks, "necesita", order.PriceVBucks)
			insufficientFundsBots++
			continue
		}
		activeBots++

		// Verificar amistad con este bot
		isFriend, friendSince, err := fortnite.CheckFriendship(database, *bot, receiverAccountID)
		if err != nil || !isFriend {
			slog.Info("Worker: usuario no es amigo del bot, probando siguiente",
				"bot", bot.DisplayName, "user", order.EpicUsername)
			notFriendBots++
			continue // probar siguiente bot
		}

		// Verificar 48h de amistad
		hoursAsFriend := time.Since(friendSince).Hours()
		if hoursAsFriend < 48 {
			slog.Info("Worker: amistad reciente con este bot, probando siguiente",
				"bot", bot.DisplayName, "hours", hoursAsFriend)
			continue // probar siguiente bot
		}

		// Intentar enviar el regalo. Se marca "intento en curso" ANTES de
		// llamar a Epic (persistido en la base, no solo en memoria) — si el
		// proceso se cae justo durante esta llamada, la próxima vez que se
		// reclame este pedido (ClaimPendingOrders) sabremos que hubo un
		// intento real interrumpido, no solo que Epic dice "ya lo tiene" por
		// una compra o regalo de otra persona ajena a nosotros. Ver el
		// comentario completo en MarkOrderSendAttempted (db.go).
		wasAlreadyAttempting, attemptErr := db.MarkOrderSendAttempted(database, order.ID, true)
		if attemptErr != nil {
			slog.Error("Worker: no se pudo registrar el intento de envío, se reintenta en el próximo ciclo", "orderID", order.ID, "error", attemptErr)
			return
		}

		message := "¡Gracias por tu compra en KidStorePeru! 🎮"
		var evidence string
		evidence, err = fortnite.SendGift(database, *bot, receiverAccountID,
			order.ItemOfferID, order.PriceVBucks, order.ItemName, message)

		// Cualquier resultado a partir de acá es un desenlace CONOCIDO para
		// este intento (éxito, ya-lo-tenía, o un error identificado) — se
		// limpia la marca para que no quede prendida de más y confunda al
		// siguiente bot que se pruebe en este mismo ciclo.
		db.MarkOrderSendAttempted(database, order.ID, false)

		if errors.Is(err, fortnite.ErrAlreadyOwned) {
			if alreadyOwnedResolution(wasAlreadyAttempting) == "needs_review" {
				// Había un intento de envío de ESTE pedido interrumpido antes de
				// esta llamada (el proceso se cayó a mitad de camino la vez
				// anterior) — es razonable pensar que ese intento sí llegó a
				// entregar el ítem, pero no hay ninguna evidencia real de Epic
				// confirmando ESE envío puntual, así que no se inventa una. Se
				// deja en revisión manual: ni se marca entregado sin pruebas, ni
				// se reembolsa a ciegas (podría estar duplicando un reembolso si
				// en realidad sí se entregó). No se vuelve a descontar
				// V-Bucks/slot del bot: si el intento original ya los descontó,
				// hacerlo de nuevo los dejaría mal contados.
				reviewNote := fmt.Sprintf("Epic reportó que el receptor ya tiene el ítem, pero un intento de envío anterior de este mismo pedido se había interrumpido — no se puede confirmar si fue ese intento el que lo entregó. bot=%s receiver=%s", bot.DisplayName, receiverAccountID)
				if markErr := db.MarkOrderNeedsReview(database, order.ID, reviewNote); markErr != nil {
					slog.Error("Worker: error dejando pedido en revisión", "orderID", order.ID, "error", markErr)
				}
				discordbot.AlertOrderNeedsReview(order.ID.String(), order.EpicUsername, order.ItemName)
				db.AddAuditLog(database, &order.CustomerID, "ORDER_NEEDS_REVIEW", reviewNote, "worker")
				slog.Warn("Worker: pedido en revisión manual — entrega incierta tras una caída", "orderID", order.ID, "bot", bot.DisplayName)
				return
			}
			// Primer y único intento de envío de este pedido: si Epic dice que
			// el receptor ya tiene el ítem, es porque lo obtuvo por su cuenta
			// (lo compró él mismo, o se lo regaló otra persona) ANTES de este
			// pedido — nuestro pedido nunca lo entregó. No corresponde marcarlo
			// "sent" solo porque el cliente posee el artículo.
			refundMsg := fmt.Sprintf("el receptor '%s' ya posee este ítem (adquirido por su cuenta, no por este pedido) — Epic no permite regalarlo de nuevo", order.EpicUsername)
			failOrderAndRefund(database, order, refundMsg, "El destinatario ya tiene este ítem en su cuenta de Fortnite (obtenido por su cuenta), por lo que Epic Games no permite regalarlo de nuevo. Se reembolsó tu KC.")
			return
		}

		if err == nil {
			// ── Éxito ──
			accountID := bot.ID
			// Guardamos la respuesta cruda de Epic como evidencia de entrega:
			// si algún día hay una disputa de pago/contracargo, esta es la
			// prueba de que el ítem sí se entregó a la cuenta correcta.
			if markErr := db.MarkOrderDelivered(database, order.ID, accountID, evidence); markErr != nil {
				slog.Warn("Worker: error guardando evidencia de entrega", "orderID", order.ID, "error", markErr)
				db.UpdateOrderStatus(database, order.ID, "sent", &accountID, nil)
			}
			// Resta atómica en SQL, no un SET absoluto calculado en memoria —
			// evita que dos instancias del worker procesando la misma cuenta
			// bot casi al mismo tiempo pisen el contador real de regalos
			// (ver el comentario de DecrementRemainingGifts en db.go).
			if decErr := db.DecrementRemainingGifts(database, bot.ID); decErr != nil {
				slog.Warn("Worker: error descontando regalo restante del bot", "bot", bot.DisplayName, "error", decErr)
			}
			bot.RemainingGifts--

			if order.PriceVBucks > 0 {
				if deductErr := db.DeductBotVbucks(database, bot.ID, order.PriceVBucks); deductErr != nil {
					slog.Warn("Worker: error descontando pavos del bot", "bot", bot.DisplayName, "error", deductErr)
				} else {
					// Reflejar el descuento también en memoria (igual que ya se
					// hace con RemainingGifts) — accounts se reutiliza para el
					// resto de pedidos de este mismo ciclo de processOrders, así
					// que si no se actualiza acá, un segundo pedido caro de este
					// mismo ciclo podría creer que el bot todavía tiene fondos
					// que en la base de datos ya se gastaron.
					bot.VBucks -= order.PriceVBucks
					slog.Info("Worker: pavos descontados", "vbucks", order.PriceVBucks, "bot", bot.DisplayName)
				}
			}

			db.AddAuditLog(database, &order.CustomerID, "ORDER_SENT",
				fmt.Sprintf("pedido %s enviado por bot %s → %s", order.ID, bot.DisplayName, order.EpicUsername), "worker")

			if customer, custErr := db.GetCustomerByID(database, order.CustomerID); custErr == nil {
				if customer.Email != nil && *customer.Email != "" {
					itemImage := ""
					if order.ItemImage != nil { itemImage = *order.ItemImage }
					go SendOrderSentEmail(smtpConfig, *customer.Email, order.EpicUsername, order.ItemName, itemImage, order.ID.String(), order.PriceKC, "es")
				}
				discordbot.NotifyPurchase(customer, order.EpicUsername, order.ItemName, order.ItemImage, order.PriceKC, order.PriceVBucks)
			}

			slog.Info("Worker: pedido enviado", "orderID", order.ID, "bot", bot.DisplayName,
				"recipient", order.EpicUsername, "item", order.ItemName)
			return
		}

		// ── Error al enviar gift ──
		errMsg := err.Error()
		errLower := strings.ToLower(errMsg)
		slog.Error("Worker: error enviando gift", "orderID", order.ID, "bot", bot.DisplayName, "msg", errMsg)

		// Token/auth → desactivar bot y probar el siguiente
		if strings.Contains(errLower, "token") || strings.Contains(errLower, "401") ||
			strings.Contains(errLower, "403") || strings.Contains(errLower, "unauthorized") ||
			strings.Contains(errLower, "deactivated") {
			slog.Warn("Worker: token invalido, marcando bot como inactivo", "bot", bot.DisplayName)
			db.DeactivateGameAccount(database, bot.ID)
			discordbot.AlertBotDeactivated(bot.ID, bot.DisplayName, "error de autenticación al intentar enviar un regalo: "+errMsg)
			bot.RemainingGifts = 0
			continue // probar siguiente bot
		}

		// Fondos insuficientes en Epic (aunque nuestro contador decía que sí
		// alcanzaba — ej. otro pedido gastó el saldo justo antes) → tratar
		// igual que "sin V-Bucks": no reintentar este bot, probar el
		// siguiente, y avisar para que se recargue pronto.
		if strings.Contains(errLower, "insufficient") || strings.Contains(errLower, "currency") {
			slog.Warn("Worker: fondos insuficientes en Epic pese al chequeo previo, probando siguiente bot",
				"bot", bot.DisplayName, "orderID", order.ID, "msg", errMsg)
			db.UpdateBotVbucks(database, bot.ID, 0)
			discordbot.CheckVBucksAlert(bot.ID, bot.DisplayName, 0)
			bot.VBucks = 0
			continue // probar siguiente bot
		}

		// Gift limit → marcar bot sin slots y probar el siguiente inmediatamente
		if strings.Contains(errLower, "gift_limit_reached") {
			slog.Warn("Worker: límite de gifts alcanzado, probando siguiente bot",
				"bot", bot.DisplayName, "orderID", order.ID)
			db.UpdateRemainingGifts(database, bot.ID, 0)
			discordbot.AlertNoGiftSlots(bot.ID, bot.DisplayName)
			bot.RemainingGifts = 0
			anyGiftLimit = true
			continue // probar siguiente bot
		}

		// Error de red transitorio → mantener pending, dejar de intentar
		isNetworkError := strings.Contains(errLower, "timeout") ||
			strings.Contains(errLower, "connection refused") ||
			strings.Contains(errLower, "no such host") ||
			strings.Contains(errLower, "eof") ||
			strings.Contains(errLower, "temporarily unavailable")
		if isNetworkError {
			slog.Warn("Worker: error de red transitorio, reintentando en siguiente ciclo", "orderID", order.ID)
			db.UpdateOrderStatus(database, order.ID, "pending", nil, nil)
			return
		}

		// Error permanente → fallar y reembolsar
		// Al cliente no se le manda el error técnico crudo de Epic, solo un
		// motivo genérico y entendible.
		failOrderAndRefund(database, order, errMsg, "Ocurrió un error técnico al procesar el envío.")
		return
	}

	// Todos los bots probados sin éxito — determinar resultado final
	if anyGiftLimit {
		// Algún bot alcanzó el límite diario → mantener pending (se resetea al día siguiente)
		noSlotsMsg := "Todas las cuentas bot han agotado sus envíos del día. Los gifts se resetean diariamente."
		slog.Warn("Worker: todos los bots agotaron límite de gifts", "orderID", order.ID)
		db.UpdateOrderStatus(database, order.ID, "pending", nil, &noSlotsMsg)
	} else if activeBots > 0 && notFriendBots == activeBots {
		// El receptor no es amigo de ningún bot. El flujo pensado es
		// "agrega al bot, después compra" — pero nada impide comprar
		// primero, y este mismo chequeo (sin distinción) también se
		// cumple para un cliente que compró hace 10 segundos y todavía
		// no tuvo tiempo de agregar al bot en Epic Games, o cuyo pedido
		// de amistad ya está mandado pero el aceptador automático
		// (corre cada 5 min, ver StartFriendRequestAcceptor en main.go)
		// todavía no pasó por él. Antes esto se reembolsaba de inmediato
		// (el worker corre cada 30s) sin darle a un cliente legítimo
		// ninguna chance real de agregar al bot a tiempo. Ahora se da un
		// margen de gracia: recién se reembolsa si el pedido lleva más
		// de friendGracePeriod sin encontrar amistad con ningún bot —
		// tiempo de sobra para que pase al menos un ciclo del aceptador
		// automático, incluso si el cliente todavía no había agregado a
		// nadie en el momento de comprar.
		if time.Since(order.CreatedAt) < friendGracePeriod {
			pendingMsg := "Esperando a que agregues alguno de nuestros bots como amigo en Fortnite."
			slog.Info("Worker: usuario aún no es amigo de ningún bot, dentro del margen de gracia", "orderID", order.ID, "edad", time.Since(order.CreatedAt))
			db.UpdateOrderStatus(database, order.ID, "pending", nil, &pendingMsg)
			return
		}
		errMsg := fmt.Sprintf("el usuario '%s' no está en la lista de amigos de ningún bot disponible", order.EpicUsername)
		slog.Error("Worker: usuario no es amigo de ningún bot tras el margen de gracia", "orderID", order.ID)
		failOrderAndRefund(database, order, errMsg, "Tu cuenta de Epic Games no es amiga de ninguno de nuestros bots todavía. Agrega alguno desde la página de Bots y vuelve a intentar tu compra.")
	} else if activeBots == 0 && insufficientFundsBots > 0 {
		// Ningún bot con slots tenía V-Bucks suficientes para este pedido en
		// particular — no es que no haya bots, es que ninguno tiene fondos
		// para ESTE monto. Se avisa (con el mismo cooldown que "sin bots
		// disponibles", para no saturar si hay varios pedidos caros en cola)
		// y se mantiene "pending" para reintentar en cuanto se recarguen.
		noFundsMsg := "Ningún bot tiene V-Bucks suficientes para este pedido en este momento."
		slog.Warn("Worker: ningún bot con fondos suficientes", "orderID", order.ID, "necesita", order.PriceVBucks)
		discordbot.AlertNoActiveBots(1)
		db.UpdateOrderStatus(database, order.ID, "pending", nil, &noFundsMsg)
	} else {
		// Otro motivo (ej: amistad reciente en todos los bots) → mantener pending
		slog.Warn("Worker: ningún bot pudo enviar el regalo en este ciclo, reintentando", "orderID", order.ID)
		db.UpdateOrderStatus(database, order.ID, "pending", nil, nil)
	}
}
