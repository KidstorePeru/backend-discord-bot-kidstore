package store

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/discord"
	"KidStoreStore/src/fortnite"
	"KidStoreStore/src/middleware"
	"KidStoreStore/src/types"
	"context"
	"database/sql"
	"encoding/json"
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

func HandlerGetExchangeRates(c *gin.Context) {
	ratesCacheMu.RLock()
	cached := ratesCacheVal
	ratesCacheMu.RUnlock()

	if cached != nil && time.Since(cached.fetchedAt) < ratesTTL {
		c.Data(http.StatusOK, "application/json", cached.body)
		return
	}

	if exchangeRateAPIKey == "" {
		c.JSON(http.StatusOK, gin.H{"USD": 0.27, "EUR": 0.25, "fetchedAt": 0})
		return
	}

	apiURL := fmt.Sprintf("https://v6.exchangerate-api.com/v6/%s/latest/PEN", exchangeRateAPIKey)
	resp, err := ratesClient.Get(apiURL)
	if err != nil {
		if cached != nil {
			c.Data(http.StatusOK, "application/json", cached.body)
			return
		}
		c.JSON(http.StatusOK, gin.H{"USD": 0.27, "EUR": 0.25, "fetchedAt": 0})
		return
	}
	defer resp.Body.Close()

	var apiResp struct {
		Result          string             `json:"result"`
		ConversionRates map[string]float64 `json:"conversion_rates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil || apiResp.Result != "success" {
		if cached != nil {
			c.Data(http.StatusOK, "application/json", cached.body)
			return
		}
		c.JSON(http.StatusOK, gin.H{"USD": 0.27, "EUR": 0.25, "fetchedAt": 0})
		return
	}

	result := gin.H{
		"USD":       apiResp.ConversionRates["USD"],
		"EUR":       apiResp.ConversionRates["EUR"],
		"fetchedAt": time.Now().UnixMilli(),
	}
	body, _ := json.Marshal(result)

	ratesCacheMu.Lock()
	ratesCacheVal = &ratesCacheEntry{body: body, fetchedAt: time.Now()}
	ratesCacheMu.Unlock()

	c.Data(http.StatusOK, "application/json", body)
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

func HandlerGetShop(c *gin.Context) {
	lang := c.Query("lang")
	if lang == "" { lang = "es-419" }
	if lang != "es-419" && lang != "en" { lang = "es-419" }

	shopCacheMu.RLock()
	entry, ok := shopCache[lang]
	shopCacheMu.RUnlock()
	if ok && time.Since(entry.fetchedAt) < shopTTL {
		c.Data(http.StatusOK, "application/json", entry.body)
		return
	}

	url := fmt.Sprintf("https://fortnite-api.com/v2/shop?language=%s", lang)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error preparando request"})
		return
	}

	resp, err := shopClient.Do(req)
	if err != nil {
		shopCacheMu.RLock()
		stale, hasStale := shopCache[lang]
		shopCacheMu.RUnlock()
		if hasStale { c.Data(http.StatusOK, "application/json", stale.body); return }
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo tienda"})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error leyendo respuesta"})
		return
	}

	shopCacheMu.Lock()
	shopCache[lang] = &shopCacheEntry{body: body, fetchedAt: time.Now()}
	shopCacheMu.Unlock()

	c.Data(http.StatusOK, "application/json", body)
}

// ==================== CREAR PEDIDO ====================

const maxPendingOrdersPerCustomer = 10

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

		// ── Límite de pedidos pendientes por cliente (máx. 10) ──
		pendingCount, err := db.CountPendingOrdersByCustomer(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error verificando pedidos"})
			return
		}
		if pendingCount >= maxPendingOrdersPerCustomer {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"success": false,
				"error":   fmt.Sprintf("Tienes %d pedidos pendientes. Espera a que se procesen antes de crear nuevos (máximo %d).", pendingCount, maxPendingOrdersPerCustomer),
				"code":    "TOO_MANY_ORDERS",
			})
			return
		}

		order, err := db.DeductKCAndCreateOrder(database, customerID, customer.EpicUsername, req)
		if err != nil {
			if strings.Contains(err.Error(), "insufficient") {
				c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
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

// ==================== WORKER ====================

var notifyDiscord func(discordID, status, itemName string, priceKC int, lang string)
var encryptionKey string

func SetDiscordNotifier(fn func(discordID, status, itemName string, priceKC int, lang string)) {
	notifyDiscord = fn
}

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
				processOrders(database)
			}
		}
	}()
}

func processOrders(database *sql.DB) {
	orders, err := db.GetPendingOrders(database)
	if err != nil || len(orders) == 0 { return }

	accounts, err := db.GetActiveGameAccounts(database, encryptionKey)
	if err != nil || len(accounts) == 0 {
		slog.Warn("Worker: no hay cuentas bot activas disponibles")
		noBotsMsg := "Sin cuentas bot activas disponibles."
		for _, order := range orders {
			db.UpdateOrderStatus(database, order.ID, "failed", nil, &noBotsMsg)
			db.RefundOrder(database, order.ID)
			db.AddAuditLog(database, &order.CustomerID, "ORDER_FAILED",
				fmt.Sprintf("pedido %s: %s — KC reembolsados", order.ID, noBotsMsg), "worker")
			sendDiscordNotification(database, order, "refunded")
		}
		return
	}

	for _, order := range orders {
		// Buscar cuenta con slots disponibles
		var selectedAccount *types.GameAccount
		for i := range accounts {
			if accounts[i].RemainingGifts > 0 { selectedAccount = &accounts[i]; break }
		}

		// Sin slots: solo procesar el pedido actual y salir del bucle
		if selectedAccount == nil {
			slog.Warn("Worker: sin slots de regalo en ninguna cuenta bot")
			noSlotsMsg := "Todas las cuentas bot han agotado sus envíos del día. Los gifts se resetean diariamente."
			db.UpdateOrderStatus(database, order.ID, "failed", nil, &noSlotsMsg)
			db.RefundOrder(database, order.ID)
			db.AddAuditLog(database, &order.CustomerID, "ORDER_FAILED",
				fmt.Sprintf("pedido %s: %s — KC reembolsados", order.ID, noSlotsMsg), "worker")
			sendDiscordNotification(database, order, "refunded")
			break
		}

		db.UpdateOrderStatus(database, order.ID, "processing", nil, nil)

		receiverAccountID, err := fortnite.GetReceiverAccountID(database, *selectedAccount, order.EpicUsername)
		if err != nil {
			errMsg := fmt.Sprintf("no se encontró el usuario Epic '%s': %s", order.EpicUsername, err.Error())
			slog.Error("Worker: error", "msg", errMsg)
			db.UpdateOrderStatus(database, order.ID, "failed", nil, &errMsg)
			db.RefundOrder(database, order.ID)
			db.AddAuditLog(database, &order.CustomerID, "ORDER_FAILED",
				fmt.Sprintf("pedido %s: %s", order.ID, errMsg), "worker")
			sendDiscordNotification(database, order, "refunded")
			continue
		}

		isFriend, friendSince, err := fortnite.CheckFriendship(database, *selectedAccount, receiverAccountID)
		if err != nil || !isFriend {
			errMsg := fmt.Sprintf("el usuario '%s' no tiene agregado al bot '%s' como amigo", order.EpicUsername, selectedAccount.DisplayName)
			slog.Error("Worker: error", "msg", errMsg)
			db.UpdateOrderStatus(database, order.ID, "failed", nil, &errMsg)
			db.RefundOrder(database, order.ID)
			db.AddAuditLog(database, &order.CustomerID, "ORDER_FAILED",
				fmt.Sprintf("pedido %s: %s", order.ID, errMsg), "worker")
			sendDiscordNotification(database, order, "refunded")
			continue
		}

		hoursAsFriend := time.Since(friendSince).Hours()
		if hoursAsFriend < 48 {
			remaining := 48 - hoursAsFriend
			errMsg := fmt.Sprintf("amigos hace %.1f horas — faltan %.1f horas para poder recibir regalos", hoursAsFriend, remaining)
			slog.Info("Worker: pedido pendiente por amistad reciente", "orderID", order.ID, "msg", errMsg)
			// Solo actualizar el mensaje de error, mantener pending — NO notificar
			db.UpdateOrderStatus(database, order.ID, "pending", nil, nil)
			continue
		}

		message := "¡Gracias por tu compra en KidStorePeru! 🎮"
		err = fortnite.SendGift(database, *selectedAccount, receiverAccountID,
			order.ItemOfferID, order.PriceVBucks, order.ItemName, message)

		if err != nil {
			errMsg := err.Error()
			errLower := strings.ToLower(errMsg)
			slog.Error("Worker: error enviando gift", "orderID", order.ID, "msg", errMsg)

			// Token/auth errors → deactivate bot
			if strings.Contains(errLower, "token") || strings.Contains(errLower, "401") ||
				strings.Contains(errLower, "403") || strings.Contains(errLower, "unauthorized") ||
				strings.Contains(errLower, "deactivated") {
				slog.Warn("Worker: token invalido, marcando bot como inactivo", "bot", selectedAccount.DisplayName)
				db.DeactivateGameAccount(database, selectedAccount.ID)
				selectedAccount.RemainingGifts = 0
			}

			// Network/transient errors → keep pending for retry next cycle
			isNetworkError := strings.Contains(errLower, "timeout") ||
				strings.Contains(errLower, "connection refused") ||
				strings.Contains(errLower, "no such host") ||
				strings.Contains(errLower, "eof") ||
				strings.Contains(errLower, "temporarily unavailable")
			if isNetworkError {
				slog.Warn("Worker: error de red transitorio, reintentando en siguiente ciclo", "orderID", order.ID)
				db.UpdateOrderStatus(database, order.ID, "pending", nil, nil)
				continue
			}

			// Permanent error → fail & refund
			if refundErr := db.RefundOrder(database, order.ID); refundErr != nil {
				slog.Warn("Worker: error reembolsando pedido", "orderID", order.ID, "error", refundErr)
			}
			db.UpdateOrderStatus(database, order.ID, "failed", nil, &errMsg)
			db.AddAuditLog(database, &order.CustomerID, "ORDER_FAILED",
				fmt.Sprintf("pedido %s falló: %s — KC reembolsados", order.ID, errMsg), "worker")
			sendDiscordNotification(database, order, "refunded")
			continue
		}

		accountID := selectedAccount.ID
		db.UpdateOrderStatus(database, order.ID, "sent", &accountID, nil)
		db.UpdateRemainingGifts(database, selectedAccount.ID, selectedAccount.RemainingGifts-1)
		selectedAccount.RemainingGifts--

		if order.PriceVBucks > 0 {
			if err := db.DeductBotVbucks(database, selectedAccount.ID, order.PriceVBucks); err != nil {
				slog.Warn("Worker: error descontando pavos del bot", "bot", selectedAccount.DisplayName, "error", err)
			} else {
				slog.Info("Worker: pavos descontados", "vbucks", order.PriceVBucks, "bot", selectedAccount.DisplayName)
			}
		}

		db.AddAuditLog(database, &order.CustomerID, "ORDER_SENT",
			fmt.Sprintf("pedido %s enviado por bot %s → %s", order.ID, selectedAccount.DisplayName, order.EpicUsername), "worker")

		sendDiscordNotification(database, order, "sent")

		// Send gift log embed to public Discord channel
		if customer, custErr := db.GetCustomerByID(database, order.CustomerID); custErr == nil {
			go discord.SendGiftLogEmbed(order.EpicUsername, order.ItemName, order.PriceKC, customer.KCBalance)
		}

		// Send order sent email notification
		if customer, err := db.GetCustomerByID(database, order.CustomerID); err == nil && customer.Email != nil && *customer.Email != "" {
			emailLang := "es"
			if customer.DiscordID != nil {
				if dl, err := db.GetDiscordLang(database, *customer.DiscordID); err == nil && dl != "" { emailLang = dl }
			}
			go SendOrderSentEmail(smtpConfig, *customer.Email, order.EpicUsername, order.ItemName, order.PriceKC, emailLang)
		}

		slog.Info("Worker: pedido enviado", "orderID", order.ID, "bot", selectedAccount.DisplayName, "recipient", order.EpicUsername, "item", order.ItemName)
	}
}

// sendDiscordNotification envía UNA sola notificación por pedido.
// Usa go para no bloquear el worker, pero la lógica de deduplicación
// está garantizada porque se llama en un único punto por cada caso.
func sendDiscordNotification(database *sql.DB, order types.Order, status string) {
	if notifyDiscord == nil {
		slog.Debug("Discord notifier not configured, skipping notification", "orderID", order.ID, "status", status)
		return
	}
	customer, err := db.GetCustomerByID(database, order.CustomerID)
	if err != nil {
		slog.Warn("Discord notify: customer not found", "orderID", order.ID, "customerID", order.CustomerID)
		return
	}
	if customer.DiscordID == nil || *customer.DiscordID == "" {
		slog.Debug("Discord notify: customer has no Discord linked", "orderID", order.ID, "user", order.EpicUsername)
		return
	}
	lang, _ := db.GetDiscordLang(database, *customer.DiscordID)
	if lang == "" { lang = "es" }
	go notifyDiscord(*customer.DiscordID, status, order.ItemName, order.PriceKC, lang)
}

// ==================== PARSE SHOP RESPONSE ====================

func ParseShopEntry(data []byte, offerID string) (int, error) {
	var resp struct {
		Data struct {
			Entries []struct {
				OfferId    string `json:"offerId"`
				FinalPrice int    `json:"finalPrice"`
			} `json:"entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil { return 0, err }
	for _, e := range resp.Data.Entries {
		if e.OfferId == offerID { return e.FinalPrice, nil }
	}
	return 0, fmt.Errorf("offer %s not found in shop", offerID)
}
