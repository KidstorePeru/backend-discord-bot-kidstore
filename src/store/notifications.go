package store

// Centro de notificaciones (campana de la web) y lista de deseos
// («Avísame cuando vuelva»).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"
	"KidStoreStore/src/middleware"
	"KidStoreStore/src/safe"
	"KidStoreStore/src/types"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// requestCustomerID devuelve el cliente autenticado o responde 401.
func requestCustomerID(c *gin.Context) (uuid.UUID, bool) {
	idStr, ok := middleware.GetCustomerID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
		return uuid.Nil, false
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
		return uuid.Nil, false
	}
	return id, true
}

func requestLang(c *gin.Context) string {
	if strings.HasPrefix(strings.ToLower(c.GetHeader("X-Lang")), "en") {
		return "en"
	}
	return "es"
}

// notifyOrderSent deja el aviso de «pedido entregado» en la campana.
func notifyOrderSent(database *sql.DB, order types.Order) {
	db.AddNotification(database, order.CustomerID, db.NotifOrderSent, map[string]any{
		"order_id": order.ID, "item_name": order.ItemName, "item_image": derefString(order.ItemImage),
		"price_kc": order.PriceKC, "epic_username": order.EpicUsername,
	})
}

// ==================== CAMPANA ====================

func HandlerGetNotifications(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerID, ok := requestCustomerID(c)
		if !ok {
			return
		}
		limit, _ := strconv.Atoi(c.Query("limit"))
		items, unread, err := db.ListNotifications(database, customerID, limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudieron cargar las notificaciones"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "notifications": items, "unread": unread})
	}
}

func HandlerGetUnreadNotifications(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerID, ok := requestCustomerID(c)
		if !ok {
			return
		}
		unread, err := db.CountUnreadNotifications(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudieron cargar las notificaciones"})
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"success": true, "unread": unread})
	}
}

func HandlerMarkNotificationsRead(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerID, ok := requestCustomerID(c)
		if !ok {
			return
		}
		if err := db.MarkNotificationsRead(database, customerID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudieron marcar como leídas"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

func HandlerGetNotificationPrefs(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerID, ok := requestCustomerID(c)
		if !ok {
			return
		}
		prefs, err := db.GetNotificationPrefs(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudieron cargar las preferencias"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "prefs": prefs})
	}
}

func HandlerUpdateNotificationPrefs(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerID, ok := requestCustomerID(c)
		if !ok {
			return
		}
		var req struct {
			Email   *bool `json:"email"`
			Discord *bool `json:"discord"`
		}
		if err := c.ShouldBindJSON(&req); err != nil || req.Email == nil || req.Discord == nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "preferencias inválidas"})
			return
		}
		prefs := db.NotificationPrefs{Email: *req.Email, Discord: *req.Discord}
		if err := db.SetNotificationPrefs(database, customerID, prefs); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudieron guardar las preferencias"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "prefs": prefs})
	}
}

// ==================== LISTA DE DESEOS ====================

var wishlistItemIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:\-]{1,150}$`)

// cleanText recorta y quita caracteres de control (el nombre se muestra en la
// web y en correos; igual se escapa al mostrarlo).
func cleanText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

func validImageURL(s string) bool {
	if s == "" {
		return true
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != "" && len(s) <= 500
}

// currentShopAppearances devuelve, para cada objeto que está HOY en la
// tienda, la oferta en la que aparece (si está en varias, la que lo vende
// solo, y si no la más barata). ok=false si la tienda no está disponible o es
// una copia de respaldo (no se avisa ni se marca nada con datos que pueden no
// ser los de hoy).
func currentShopAppearances(ctx context.Context, now time.Time) (map[string]shopEntryView, bool) {
	body, err := fetchShopBody(ctx, "es-419")
	if err != nil || bytes.Contains(body, []byte(`"_stale":true`)) {
		return nil, false
	}
	views, err := parseShopEntries(body)
	if err != nil {
		return nil, false
	}
	out := map[string]shopEntryView{}
	for _, v := range views {
		if !v.OutDate.IsZero() && !now.Before(v.OutDate) {
			continue // la oferta ya salió de la tienda
		}
		if v.InDate.IsZero() {
			y, m, d := now.UTC().Date()
			v.InDate = time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		}
		for _, id := range v.ItemIDs {
			prev, seen := out[id]
			better := !seen ||
				len(v.ItemIDs) < len(prev.ItemIDs) ||
				(len(v.ItemIDs) == len(prev.ItemIDs) && v.FinalPrice < prev.FinalPrice)
			if better {
				out[id] = v
			}
		}
	}
	return out, true
}

type wishlistItemView struct {
	db.WishlistItem
	InShop  bool       `json:"in_shop"`
	PriceKC int        `json:"price_kc,omitempty"`
	OutDate *time.Time `json:"out_date,omitempty"`
}

func HandlerGetWishlist(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerID, ok := requestCustomerID(c)
		if !ok {
			return
		}
		items, err := db.ListWishlistItems(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudo cargar tu lista de deseos"})
			return
		}
		appearances, _ := currentShopAppearances(c.Request.Context(), time.Now())
		out := make([]wishlistItemView, 0, len(items))
		for _, it := range items {
			view := wishlistItemView{WishlistItem: it}
			if a, found := appearances[it.ItemID]; found {
				view.InShop, view.PriceKC = true, a.FinalPrice
				if !a.OutDate.IsZero() {
					out := a.OutDate
					view.OutDate = &out
				}
			}
			out = append(out, view)
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "items": out, "limit": db.WishlistLimit})
	}
}

func HandlerAddWishlistItem(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerID, ok := requestCustomerID(c)
		if !ok {
			return
		}
		var req struct {
			ItemID   string `json:"item_id"`
			Name     string `json:"name"`
			ItemType string `json:"item_type"`
			Image    string `json:"image"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "datos inválidos"})
			return
		}
		item := db.WishlistItem{
			ItemID:   strings.TrimSpace(req.ItemID),
			Name:     cleanText(req.Name, 150),
			ItemType: cleanText(req.ItemType, 60),
			Image:    strings.TrimSpace(req.Image),
		}
		if !wishlistItemIDPattern.MatchString(item.ItemID) || item.Name == "" || !validImageURL(item.Image) {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "datos inválidos"})
			return
		}
		// Si el objeto está en la tienda ahora mismo, esta aparición se da por
		// avisada: el aviso llega la próxima vez que vuelva.
		var inShopSince *time.Time
		if appearances, ok := currentShopAppearances(c.Request.Context(), time.Now()); ok {
			if a, found := appearances[item.ItemID]; found {
				in := a.InDate
				inShopSince = &in
			}
		}
		err := db.AddWishlistItem(database, customerID, item, requestLang(c), inShopSince)
		if errors.Is(err, db.ErrWishlistFull) {
			c.JSON(http.StatusConflict, gin.H{"success": false, "code": "WISHLIST_FULL",
				"error": fmt.Sprintf("tu lista de deseos ya tiene %d objetos", db.WishlistLimit)})
			return
		}
		if err != nil {
			slog.Error("Wishlist: error agregando", "customer", customerID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudo agregar a tu lista de deseos"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

func HandlerRemoveWishlistItem(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerID, ok := requestCustomerID(c)
		if !ok {
			return
		}
		itemID := c.Param("itemId")
		if !wishlistItemIDPattern.MatchString(itemID) {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "datos inválidos"})
			return
		}
		if err := db.RemoveWishlistItem(database, customerID, itemID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudo quitar de tu lista de deseos"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

// ==================== BUSCADOR DE COSMÉTICOS ====================

// cosmeticsSearchURL — variable para que las pruebas usen un servidor simulado.
var cosmeticsSearchURL = "https://fortnite-api.com/v2/cosmetics/br/search/all"

type cosmeticResult struct {
	ItemID   string `json:"item_id"`
	Name     string `json:"name"`
	ItemType string `json:"item_type"`
	Image    string `json:"image"`
	LastSeen string `json:"last_seen,omitempty"`
}

type searchCacheEntry struct {
	results []cosmeticResult
	at      time.Time
}

var (
	searchCacheMu sync.Mutex
	searchCache   = map[string]searchCacheEntry{}
)

// searchCosmetics busca en el catálogo de fortnite-api.com. Solo devuelve
// objetos que alguna vez se vendieron en la tienda (los del Pase de Batalla o
// de eventos nunca vuelven: sería una promesa falsa avisar de ellos).
func searchCosmetics(ctx context.Context, query, lang string) ([]cosmeticResult, error) {
	key := lang + "|" + strings.ToLower(query)
	searchCacheMu.Lock()
	if e, ok := searchCache[key]; ok && time.Since(e.at) < 30*time.Minute {
		searchCacheMu.Unlock()
		return e.results, nil
	}
	searchCacheMu.Unlock()

	apiLang := "es-419"
	if lang == "en" {
		apiLang = "en"
	}
	q := url.Values{"name": {query}, "matchMethod": {"contains"}, "language": {apiLang}, "responseFlags": {"4"}}
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, cosmeticsSearchURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := shopClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return []cosmeticResult{}, nil // fortnite-api responde 404 cuando no hay coincidencias
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fortnite-api.com respondió %d", resp.StatusCode)
	}
	var parsed struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Type struct {
				DisplayValue string `json:"displayValue"`
			} `json:"type"`
			Images struct {
				SmallIcon string `json:"smallIcon"`
				Icon      string `json:"icon"`
			} `json:"images"`
			ShopHistory []string `json:"shopHistory"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("respuesta inesperada: %w", err)
	}
	results := []cosmeticResult{}
	for _, d := range parsed.Data {
		if len(d.ShopHistory) == 0 || !wishlistItemIDPattern.MatchString(d.ID) || d.Name == "" {
			continue
		}
		img := d.Images.SmallIcon
		if img == "" {
			img = d.Images.Icon
		}
		results = append(results, cosmeticResult{
			ItemID: d.ID, Name: d.Name, ItemType: d.Type.DisplayValue, Image: img,
			LastSeen: d.ShopHistory[len(d.ShopHistory)-1],
		})
	}
	// Primero lo que estuvo en la tienda más recientemente.
	sort.SliceStable(results, func(i, j int) bool { return results[i].LastSeen > results[j].LastSeen })
	if len(results) > 24 {
		results = results[:24]
	}
	searchCacheMu.Lock()
	if len(searchCache) > 500 {
		searchCache = map[string]searchCacheEntry{}
	}
	searchCache[key] = searchCacheEntry{results: results, at: time.Now()}
	searchCacheMu.Unlock()
	return results, nil
}

func HandlerSearchCosmetics() gin.HandlerFunc {
	return func(c *gin.Context) {
		query := cleanText(c.Query("q"), 50)
		if len([]rune(query)) < 2 {
			c.JSON(http.StatusOK, gin.H{"success": true, "results": []cosmeticResult{}})
			return
		}
		results, err := searchCosmetics(c.Request.Context(), query, requestLang(c))
		if err != nil {
			slog.Warn("Buscador de cosméticos: error", "error", err)
			c.JSON(http.StatusBadGateway, gin.H{"success": false, "error": "el buscador no está disponible ahora, intenta de nuevo"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "results": results})
	}
}

// ==================== AVISO DIARIO: «VOLVIÓ A LA TIENDA» ====================

// WishlistBackItem es un objeto de la lista de deseos que volvió a la tienda.
type WishlistBackItem struct {
	ItemID  string
	Name    string
	Image   string
	PriceKC int
	OutDate time.Time
}

// Avisos por correo y Discord — variables para poder reemplazarlas en pruebas.
var (
	sendWishlistBackEmail   = SendWishlistBackEmail
	sendWishlistBackDiscord = func(discordID, lang string, items []WishlistBackItem) {
		di := make([]discordbot.WishlistBackItem, 0, len(items))
		for _, it := range items {
			di = append(di, discordbot.WishlistBackItem{Name: it.Name, Image: it.Image, PriceKC: it.PriceKC, OutDate: it.OutDate})
		}
		discordbot.NotifyWishlistBack(discordID, lang, di)
	}
)

// runWishlistNotifier avisa a cada cliente qué objetos de su lista de deseos
// volvieron a la tienda: un aviso en la campana por objeto y, si lo tiene
// activado, UN correo y UN mensaje de Discord con todos juntos. Cada objeto
// se avisa una sola vez por regreso (no cada día que siga en la tienda).
func runWishlistNotifier(database *sql.DB, now time.Time) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	appearances, ok := currentShopAppearances(ctx, now)
	if !ok {
		return 0, nil // tienda no disponible o desactualizada: se intenta en la próxima vuelta
	}
	inDates := make(map[string]time.Time, len(appearances))
	for id, a := range appearances {
		inDates[id] = a.InDate
	}
	matches, err := db.PendingWishlistMatches(database, inDates)
	if err != nil {
		return 0, err
	}

	type customerBatch struct {
		lang  string
		items []WishlistBackItem
	}
	batches := map[uuid.UUID]*customerBatch{}
	var order []uuid.UUID
	for _, m := range matches {
		a := appearances[m.ItemID]
		claimed, err := db.ClaimWishlistNotification(database, m.RowID, a.InDate)
		if err != nil {
			slog.Warn("Wishlist: no se pudo marcar el aviso", "error", err)
			continue
		}
		if !claimed {
			continue
		}
		image := m.Image
		if image == "" {
			image = a.Image
		}
		item := WishlistBackItem{ItemID: m.ItemID, Name: m.Name, Image: image, PriceKC: a.FinalPrice, OutDate: a.OutDate}
		data := map[string]any{"item_id": item.ItemID, "name": item.Name, "image": item.Image, "price_kc": item.PriceKC, "offer_id": a.OfferID}
		if !a.OutDate.IsZero() {
			data["out_date"] = a.OutDate
		}
		db.AddNotification(database, m.CustomerID, db.NotifWishlistBack, data)
		b, exists := batches[m.CustomerID]
		if !exists {
			b = &customerBatch{lang: m.Lang}
			batches[m.CustomerID] = b
			order = append(order, m.CustomerID)
		}
		b.items = append(b.items, item)
	}

	notified := 0
	for _, customerID := range order {
		b := batches[customerID]
		notified += len(b.items)
		customer, err := db.GetCustomerByID(database, customerID)
		if err != nil {
			continue
		}
		prefs, err := db.GetNotificationPrefs(database, customerID)
		if err != nil {
			prefs = db.NotificationPrefs{Email: true, Discord: true}
		}
		if prefs.Email && customer.Email != nil && *customer.Email != "" {
			go sendWishlistBackEmail(smtpConfig, *customer.Email, b.items, b.lang)
		}
		if prefs.Discord && customer.DiscordID != nil && *customer.DiscordID != "" {
			go sendWishlistBackDiscord(*customer.DiscordID, b.lang, b.items)
		}
	}
	if notified > 0 {
		slog.Info("Wishlist: avisos de objetos que volvieron a la tienda", "objetos", notified, "clientes", len(order))
	}
	return notified, nil
}

// StartWishlistNotifier revisa cada 10 minutos si algún objeto de las listas
// de deseos volvió a la tienda (la tienda cambia a las 00:00 UTC, 7:00 p. m.
// en Lima; así el aviso llega a los pocos minutos). Una vez al día borra los
// avisos de la campana con más de 90 días.
func StartWishlistNotifier(database *sql.DB) {
	go func() {
		time.Sleep(2 * time.Minute)
		var lastCleanup time.Time
		for {
			safe.Run("wishlist.notify", func() {
				if _, err := runWishlistNotifier(database, time.Now()); err != nil {
					slog.Error("Wishlist: error revisando la tienda", "error", err)
				}
			})
			if time.Since(lastCleanup) > 24*time.Hour {
				safe.Run("notifications.cleanup", func() {
					if n, err := db.DeleteOldNotifications(database); err == nil && n > 0 {
						slog.Info("Notificaciones viejas borradas", "cantidad", n)
					}
				})
				lastCleanup = time.Now()
			}
			time.Sleep(10 * time.Minute)
		}
	}()
}

// ==================== CORREO ====================

var englishMonths = [...]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

// shopDayLabel: el último día que la oferta está en la tienda, en hora de
// Lima (outDate es el final del día UTC, o sea las 6:59 p. m. en Lima).
func shopDayLabel(t time.Time, es bool) string {
	lima := t.In(time.FixedZone("PET", -5*3600))
	if es {
		return fmt.Sprintf("%d %s", lima.Day(), spanishMonths[lima.Month()-1])
	}
	return fmt.Sprintf("%s %d", englishMonths[lima.Month()-1], lima.Day())
}

func SendWishlistBackEmail(cfg types.EnvConfig, toEmail string, items []WishlistBackItem, lang string) {
	if !hasEmailProvider(cfg) || len(items) == 0 {
		return
	}
	es := lang != "en"
	var subject, intro, eyebrow, btnText, manage string
	if es {
		eyebrow = "Lista de deseos"
		if len(items) == 1 {
			subject = "KidStorePeru — Volvió a la tienda: " + items[0].Name
			intro = "Algo de tu lista de deseos volvió a la tienda de Fortnite. Aprovecha antes de que se vaya."
		} else {
			subject = fmt.Sprintf("KidStorePeru — Volvieron %d objetos de tu lista de deseos", len(items))
			intro = "Varias cosas de tu lista de deseos volvieron a la tienda de Fortnite. Aprovecha antes de que se vayan."
		}
		btnText, manage = "Ver en la tienda", "Administrar mis avisos"
	} else {
		eyebrow = "Wishlist"
		if len(items) == 1 {
			subject = "KidStorePeru — Back in the shop: " + items[0].Name
			intro = "Something from your wishlist is back in the Fortnite shop. Grab it before it leaves."
		} else {
			subject = fmt.Sprintf("KidStorePeru — %d items from your wishlist are back", len(items))
			intro = "Several items from your wishlist are back in the Fortnite shop. Grab them before they leave."
		}
		btnText, manage = "View in the shop", "Manage my alerts"
	}

	body := emailEyebrow(eyebrow) + emailCopy(intro)
	for _, it := range items {
		sub := emailKCIcon(12) + fmt.Sprintf("%d KC", it.PriceKC)
		if !it.OutDate.IsZero() {
			if es {
				sub += " · hasta el " + shopDayLabel(it.OutDate, true)
			} else {
				sub += " · until " + shopDayLabel(it.OutDate, false)
			}
		}
		body += emailItemRow(it.Image, esc(it.Name), sub)
	}
	body += emailButton(btnText, "https://www.kidstoreperu.net/store")
	body += fmt.Sprintf(`<p style="margin:14px 0 0;font-size:11.5px;text-align:center;"><a href="https://www.kidstoreperu.net/notifications" style="color:#6d716f;">%s</a></p>`, manage)

	htmlBody := emailShell(esc(subject), esc(intro), fmtDateEs(), body)
	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: wishlist back send error", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: wishlist back notification sent", "to", toEmail, "items", len(items))
	}
}
