package store

// Reseñas verificadas y cifras reales de la portada.

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// maskName muestra solo el principio y el final del nombre de Epic
// ("JuanPerez23" → "Ju***23"), para cuidar la privacidad del cliente.
func maskName(name string) string {
	r := []rune(strings.TrimSpace(name))
	switch {
	case len(r) == 0:
		return "Cliente"
	case len(r) <= 3:
		return string(r[:1]) + "***"
	case len(r) <= 5:
		return string(r[:2]) + "***"
	default:
		return string(r[:2]) + "***" + string(r[len(r)-2:])
	}
}

// Aviso al admin de una reseña nueva para moderar (variable para pruebas).
var alertNewReview = discordbot.AlertNewReview

func HandlerReviewableOrders(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerID, ok := requestCustomerID(c)
		if !ok {
			return
		}
		ids, err := db.ReviewableOrders(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudieron cargar tus pedidos"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "order_ids": ids})
	}
}

func HandlerCreateReview(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerID, ok := requestCustomerID(c)
		if !ok {
			return
		}
		var req struct {
			OrderID string `json:"order_id"`
			Rating  int    `json:"rating"`
			Comment string `json:"comment"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "datos inválidos"})
			return
		}
		orderID, err := uuid.Parse(req.OrderID)
		if err != nil || req.Rating < 1 || req.Rating > 5 {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "datos inválidos"})
			return
		}
		customer, err := db.GetCustomerByID(database, customerID)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		comment := cleanText(req.Comment, 500)
		review, err := db.CreateReview(database, customerID, orderID, req.Rating, comment, maskName(customer.EpicUsername), requestLang(c))
		switch {
		case errors.Is(err, db.ErrReviewExists):
			c.JSON(http.StatusConflict, gin.H{"success": false, "code": "REVIEW_EXISTS", "error": "ya dejaste una reseña para este pedido"})
			return
		case errors.Is(err, db.ErrReviewNotAllowed):
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "code": "REVIEW_NOT_ALLOWED", "error": "este pedido no se puede reseñar"})
			return
		case err != nil:
			slog.Error("Reseñas: error guardando", "customer", customerID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudo guardar tu reseña"})
			return
		}
		go alertNewReview(review.Rating, review.ItemName, review.DisplayName, review.Comment)
		c.JSON(http.StatusOK, gin.H{"success": true, "review": review})
	}
}

// ── Públicas (portada) ──

type publicReview struct {
	DisplayName string    `json:"display_name"`
	Rating      int       `json:"rating"`
	Comment     string    `json:"comment"`
	ItemName    string    `json:"item_name"`
	ItemImage   string    `json:"item_image"`
	Reply       *string   `json:"reply,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type cachedPublic struct {
	body gin.H
	at   time.Time
}

var (
	publicCacheMu sync.Mutex
	reviewsCache  cachedPublic
	statsCache    cachedPublic
	publicTTL     = 5 * time.Minute
)

func HandlerPublicReviews(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		publicCacheMu.Lock()
		cached := reviewsCache
		publicCacheMu.Unlock()
		if cached.body != nil && time.Since(cached.at) < publicTTL {
			c.Header("Cache-Control", "public, max-age=300")
			c.JSON(http.StatusOK, cached.body)
			return
		}
		list, summary, err := db.PublicReviews(database, 12)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudieron cargar las reseñas"})
			return
		}
		out := make([]publicReview, 0, len(list))
		for _, r := range list {
			out = append(out, publicReview{DisplayName: r.DisplayName, Rating: r.Rating, Comment: r.Comment,
				ItemName: r.ItemName, ItemImage: r.ItemImage, Reply: r.Reply, CreatedAt: r.CreatedAt})
		}
		body := gin.H{"success": true, "reviews": out, "summary": summary}
		publicCacheMu.Lock()
		reviewsCache = cachedPublic{body: body, at: time.Now()}
		publicCacheMu.Unlock()
		c.Header("Cache-Control", "public, max-age=300")
		c.JSON(http.StatusOK, body)
	}
}

// InvalidatePublicReviews hace que la portada muestre al toque una reseña
// recién aprobada o rechazada.
func InvalidatePublicReviews() {
	publicCacheMu.Lock()
	reviewsCache = cachedPublic{}
	statsCache = cachedPublic{}
	publicCacheMu.Unlock()
}

// HandlerStoreStats: cifras reales de la portada. orders_delivered suma las
// ventas históricas fuera de la web (historicOrders, configurable) y los
// pedidos entregados por la web; shop_items_today es el número de ofertas de
// la tienda de hoy (0 si no está disponible: la portada usa su texto fijo).
func HandlerStoreStats(database *sql.DB, historicOrders int) gin.HandlerFunc {
	return func(c *gin.Context) {
		publicCacheMu.Lock()
		cached := statsCache
		publicCacheMu.Unlock()
		if cached.body != nil && time.Since(cached.at) < publicTTL {
			c.Header("Cache-Control", "public, max-age=300")
			c.JSON(http.StatusOK, cached.body)
			return
		}
		delivered, err := db.CountDeliveredOrders(database)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudieron cargar las cifras"})
			return
		}
		shopItems := 0
		if appearancesBody, err := fetchShopBody(c.Request.Context(), "es-419"); err == nil {
			if views, err := parseShopEntries(appearancesBody); err == nil {
				now := time.Now()
				for _, v := range views {
					if v.OutDate.IsZero() || now.Before(v.OutDate) {
						shopItems++
					}
				}
			}
		}
		_, summary, err := db.PublicReviews(database, 1)
		if err != nil {
			summary = db.ReviewSummary{}
		}
		body := gin.H{"success": true, "orders_delivered": historicOrders + delivered, "shop_items_today": shopItems, "reviews": summary}
		publicCacheMu.Lock()
		statsCache = cachedPublic{body: body, at: time.Now()}
		publicCacheMu.Unlock()
		c.Header("Cache-Control", "public, max-age=300")
		c.JSON(http.StatusOK, body)
	}
}
