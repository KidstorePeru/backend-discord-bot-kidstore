package store

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/fortnite"
	"KidStoreStore/src/middleware"
	"KidStoreStore/src/types"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

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

		if customer.KCBalance < req.PriceKC {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": fmt.Sprintf("KC insuficientes: tienes %d KC, necesitas %d KC", customer.KCBalance, req.PriceKC)})
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
		orders, err := db.GetOrdersByCustomer(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo pedidos"})
			return
		}
		if orders == nil { orders = []types.Order{} }
		c.JSON(http.StatusOK, gin.H{"success": true, "orders": orders})
	}
}

// ==================== WORKER ====================

// notifyDiscord es inyectada desde main.go para evitar importación circular
var notifyDiscord func(discordID, status, itemName string, priceKC int, lang string)

// SetDiscordNotifier inyecta la función de notificación desde main.go
func SetDiscordNotifier(fn func(discordID, status, itemName string, priceKC int, lang string)) {
	notifyDiscord = fn
}

// StartOrderWorker procesa pedidos pendientes cada 30 segundos
func StartOrderWorker(ctx context.Context, database *sql.DB) {
	fmt.Println("[Worker] Iniciando cola de envíos...")
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				fmt.Println("[Worker] Detenido.")
				return
			case <-ticker.C:
				inSchedule, reason := db.IsWithinSchedule(database)
				if !inSchedule { fmt.Printf("[Worker] ⏰ Pausado: %s\n", reason); continue }
				processOrders(database)
			}
		}
	}()
}

func processOrders(database *sql.DB) {
	orders, err := db.GetPendingOrders(database)
	if err != nil || len(orders) == 0 { return }

	accounts, err := db.GetActiveGameAccounts(database)
	if err != nil || len(accounts) == 0 {
		fmt.Println("[Worker] No hay cuentas bot activas disponibles")
		noBotsMsg := "Sin cuentas bot activas disponibles. El admin debe vincular o reactivar una cuenta."
		for _, order := range orders {
			db.UpdateOrderStatus(database, order.ID, "failed", nil, &noBotsMsg)
			db.RefundOrder(database, order.ID)
			db.AddAuditLog(database, &order.CustomerID, "ORDER_FAILED", fmt.Sprintf("pedido %s: %s — KC reembolsados", order.ID, noBotsMsg), "worker")
			sendDiscordNotification(database, order, "refunded")
		}
		return
	}

	for _, order := range orders {
		var selectedAccount *types.GameAccount
		for i := range accounts {
			if accounts[i].RemainingGifts > 0 { selectedAccount = &accounts[i]; break }
		}
		if selectedAccount == nil {
			fmt.Println("[Worker] Sin slots de regalo en ninguna cuenta bot")
			noSlotsMsg := "Todas las cuentas bot han agotado sus envíos del día. Los gifts se resetean diariamente."
			for _, o := range orders {
				if o.Status == "pending" {
					db.UpdateOrderStatus(database, o.ID, "failed", nil, &noSlotsMsg)
					db.RefundOrder(database, o.ID)
					db.AddAuditLog(database, &o.CustomerID, "ORDER_FAILED", fmt.Sprintf("pedido %s: %s — KC reembolsados", o.ID, noSlotsMsg), "worker")
					sendDiscordNotification(database, o, "refunded")
				}
			}
			break
		}

		db.UpdateOrderStatus(database, order.ID, "processing", nil, nil)

		receiverAccountID, err := fortnite.GetReceiverAccountID(database, *selectedAccount, order.EpicUsername)
		if err != nil {
			errMsg := fmt.Sprintf("no se encontró el usuario Epic '%s': %s", order.EpicUsername, err.Error())
			fmt.Printf("[Worker] ❌ %s\n", errMsg)
			db.UpdateOrderStatus(database, order.ID, "failed", nil, &errMsg)
			db.RefundOrder(database, order.ID)
			db.AddAuditLog(database, &order.CustomerID, "ORDER_FAILED", fmt.Sprintf("pedido %s: %s", order.ID, errMsg), "worker")
			sendDiscordNotification(database, order, "refunded")
			continue
		}

		isFriend, friendSince, err := fortnite.CheckFriendship(database, *selectedAccount, receiverAccountID)
		if err != nil || !isFriend {
			errMsg := fmt.Sprintf("el usuario '%s' no tiene agregado al bot '%s' como amigo", order.EpicUsername, selectedAccount.DisplayName)
			fmt.Printf("[Worker] ❌ %s\n", errMsg)
			db.UpdateOrderStatus(database, order.ID, "failed", nil, &errMsg)
			db.RefundOrder(database, order.ID)
			db.AddAuditLog(database, &order.CustomerID, "ORDER_FAILED", fmt.Sprintf("pedido %s: %s", order.ID, errMsg), "worker")
			sendDiscordNotification(database, order, "refunded")
			continue
		}

		hoursAsFriend := time.Since(friendSince).Hours()
		if hoursAsFriend < 48 {
			remaining := 48 - hoursAsFriend
			errMsg := fmt.Sprintf("amigos hace %.1f horas — faltan %.1f horas para poder recibir regalos", hoursAsFriend, remaining)
			fmt.Printf("[Worker] ⏳ pedido %s: %s\n", order.ID, errMsg)
			db.UpdateOrderStatus(database, order.ID, "pending", nil, nil)
			continue
		}

		message := "¡Gracias por tu compra en KidStorePeru! 🎮"
		err = fortnite.SendGift(database, *selectedAccount, receiverAccountID,
			order.ItemOfferID, order.PriceVBucks, order.ItemName, message)

		if err != nil {
			errMsg := err.Error()
			fmt.Printf("[Worker] ❌ Error enviando gift pedido %s: %s\n", order.ID, errMsg)
			if strings.Contains(errMsg, "token") || strings.Contains(errMsg, "401") ||
				strings.Contains(errMsg, "403") || strings.Contains(errMsg, "unauthorized") ||
				strings.Contains(errMsg, "deactivated") {
				fmt.Printf("[Worker] 🔒 Token inválido para bot %s — marcando como inactivo\n", selectedAccount.DisplayName)
				db.DeactivateGameAccount(database, selectedAccount.ID)
				selectedAccount.RemainingGifts = 0
			}
			if refundErr := db.RefundOrder(database, order.ID); refundErr != nil {
				fmt.Printf("[Worker] ⚠️ Error reembolsando pedido %s: %s\n", order.ID, refundErr)
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
				fmt.Printf("[Worker] ⚠️ Error descontando pavos del bot %s: %s\n", selectedAccount.DisplayName, err)
			} else {
				fmt.Printf("[Worker] 💰 Descontados %d pavos del bot %s\n", order.PriceVBucks, selectedAccount.DisplayName)
			}
		}

		db.AddAuditLog(database, &order.CustomerID, "ORDER_SENT",
			fmt.Sprintf("pedido %s enviado por bot %s → %s", order.ID, selectedAccount.DisplayName, order.EpicUsername), "worker")

		sendDiscordNotification(database, order, "sent")

		fmt.Printf("[Worker] ✅ Pedido %s enviado: bot %s → %s (%s)\n",
			order.ID, selectedAccount.DisplayName, order.EpicUsername, order.ItemName)
	}
}

// sendDiscordNotification envía un DM al cliente si tiene Discord vinculado
func sendDiscordNotification(database *sql.DB, order types.Order, status string) {
	if notifyDiscord == nil { return }
	customer, err := db.GetCustomerByID(database, order.CustomerID)
	if err != nil || customer.DiscordID == nil || *customer.DiscordID == "" { return }
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

// ── Import requerido por discordgo (usado indirectamente) ──
var _ = (*discordgo.Session)(nil)
