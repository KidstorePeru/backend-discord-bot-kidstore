package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"KidStoreStore/src/db"
	"KidStoreStore/src/types"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// wishEntry arma una oferta válida de la tienda con los objetos indicados.
func wishEntry(offerID string, price int, inDate, outDate string, itemIDs ...string) string {
	var items []string
	for _, id := range itemIDs {
		items = append(items, fmt.Sprintf(`{"id":%q,"name":"Nombre %s","images":{"icon":"https://fortnite-api.com/images/%s.png"}}`, id, id, id))
	}
	return fmt.Sprintf(`{"offerId":%q,"finalPrice":%d,"inDate":%q,"outDate":%q,"brItems":[%s]}`,
		offerID, price, inDate, outDate, strings.Join(items, ","))
}

func wishShop(entries ...string) string {
	return `{"status":200,"data":{"date":"2026-10-01T00:00:00Z","entries":[` + strings.Join(entries, ",") + `]}}`
}

type wishlistSends struct {
	emails   chan []WishlistBackItem
	discords chan []WishlistBackItem
}

// stubWishlistSends reemplaza el correo y Discord por canales (nunca se
// manda nada real).
func stubWishlistSends(t *testing.T) wishlistSends {
	t.Helper()
	s := wishlistSends{emails: make(chan []WishlistBackItem, 10), discords: make(chan []WishlistBackItem, 10)}
	prevEmail, prevDiscord := sendWishlistBackEmail, sendWishlistBackDiscord
	sendWishlistBackEmail = func(_ types.EnvConfig, _ string, items []WishlistBackItem, _ string) { s.emails <- items }
	sendWishlistBackDiscord = func(_, _ string, items []WishlistBackItem) { s.discords <- items }
	t.Cleanup(func() { sendWishlistBackEmail, sendWishlistBackDiscord = prevEmail, prevDiscord })
	return s
}

func receive(t *testing.T, ch chan []WishlistBackItem, what string) []WishlistBackItem {
	t.Helper()
	select {
	case items := <-ch:
		return items
	case <-time.After(3 * time.Second):
		t.Fatalf("no llegó el %s", what)
		return nil
	}
}

func assertNothing(t *testing.T, ch chan []WishlistBackItem, what string) {
	t.Helper()
	select {
	case items := <-ch:
		t.Fatalf("no debería mandarse otro %s, llegó %+v", what, items)
	case <-time.After(150 * time.Millisecond):
	}
}

func countNotifications(t *testing.T, customerID uuid.UUID, kind string) int {
	t.Helper()
	var n int
	if err := shopTestDB.QueryRow(`SELECT COUNT(*) FROM notifications WHERE customer_id=$1 AND kind=$2`, customerID, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func setShop(f *fakeFortniteAPI, body string) {
	f.body.Store(body)
	expireShopCache()
}

func TestListaDeDeseos_AvisaUnaVezPorRegreso(t *testing.T) {
	conn := setupShopTestDB(t)
	f := withFakeShop(t)
	sends := stubWishlistSends(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	t.Cleanup(cleanup)
	if _, err := conn.Exec(`UPDATE customers SET discord_id=$2 WHERE id=$1`, custID, "disc-"+custID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	// Día 1: la skin no está en la tienda; el cliente la agrega a su lista.
	setShop(f, wishShop(wishEntry("v2:/otra", 500, "2026-10-01T00:00:00Z", "2026-10-02T23:59:59.999Z", "Otro_Item")))
	if err := db.AddWishlistItem(conn, custID, db.WishlistItem{ItemID: "Character_Rata", Name: "Rata radiactiva", Image: "https://fortnite-api.com/rata.png"}, "es", nil); err != nil {
		t.Fatal(err)
	}
	if n, err := runWishlistNotifier(conn, now); err != nil || n != 0 {
		t.Fatalf("sin la skin en la tienda no hay aviso: n=%d err=%v", n, err)
	}

	// Día 2: vuelve (sola y también dentro de un lote): un aviso, con la oferta suelta.
	setShop(f, wishShop(
		wishEntry("v2:/lote", 2000, "2026-10-02T00:00:00Z", "2026-10-04T23:59:59.999Z", "Character_Rata", "Pickaxe_Rata"),
		wishEntry("v2:/rata", 1200, "2026-10-02T00:00:00Z", "2026-10-04T23:59:59.999Z", "Character_Rata"),
	))
	day2 := now.Add(24 * time.Hour)
	if n, err := runWishlistNotifier(conn, day2); err != nil || n != 1 {
		t.Fatalf("debería avisar 1 objeto: n=%d err=%v", n, err)
	}
	items := receive(t, sends.emails, "correo")
	if len(items) != 1 || items[0].Name != "Rata radiactiva" || items[0].PriceKC != 1200 {
		t.Errorf("correo con %+v (se esperaba la oferta suelta de 1200 KC)", items)
	}
	receive(t, sends.discords, "mensaje de Discord")
	if got := countNotifications(t, custID, db.NotifWishlistBack); got != 1 {
		t.Errorf("avisos en la campana = %d, se esperaba 1", got)
	}

	// Día 3: sigue en la tienda (misma aparición): no se repite.
	if n, _ := runWishlistNotifier(conn, day2.Add(24*time.Hour)); n != 0 {
		t.Errorf("no debe avisar otra vez mientras siga en la tienda, n=%d", n)
	}
	assertNothing(t, sends.emails, "correo")

	// Semanas después vuelve otra vez (nueva fecha de entrada): avisa de nuevo.
	setShop(f, wishShop(wishEntry("v2:/rata2", 1200, "2026-11-10T00:00:00Z", "2026-11-11T23:59:59.999Z", "Character_Rata")))
	if n, _ := runWishlistNotifier(conn, time.Date(2026, 11, 10, 5, 0, 0, 0, time.UTC)); n != 1 {
		t.Errorf("un nuevo regreso debe avisarse, n=%d", n)
	}
	receive(t, sends.emails, "correo del segundo regreso")
	receive(t, sends.discords, "Discord del segundo regreso")
}

func TestListaDeDeseos_RespetaPreferenciasYTiendaDesactualizada(t *testing.T) {
	conn := setupShopTestDB(t)
	f := withFakeShop(t)
	sends := stubWishlistSends(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	t.Cleanup(cleanup)
	if err := db.SetNotificationPrefs(conn, custID, db.NotificationPrefs{Email: false, Discord: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddWishlistItem(conn, custID, db.WishlistItem{ItemID: "Glider_X", Name: "Planeador"}, "en", nil); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	setShop(f, wishShop(wishEntry("v2:/g", 800, "2026-10-01T00:00:00Z", "2026-10-01T23:59:59.999Z", "Glider_X")))

	// Con el proveedor caído la tienda llega marcada _stale: no se avisa nada.
	if _, err := fetchShopBody(context.Background(), "es-419"); err != nil {
		t.Fatal(err)
	}
	expireShopCache()
	f.failing.Store(true)
	if n, _ := runWishlistNotifier(conn, now); n != 0 {
		t.Fatalf("con datos desactualizados no se avisa, n=%d", n)
	}
	f.failing.Store(false)
	shopCacheMu.Lock()
	shopLastFailure = map[string]time.Time{}
	shopCacheMu.Unlock()

	if n, _ := runWishlistNotifier(conn, now); n != 1 {
		t.Fatalf("n=%d, se esperaba 1", n)
	}
	assertNothing(t, sends.emails, "correo (el cliente lo desactivó)")
	assertNothing(t, sends.discords, "Discord (el cliente no lo tiene vinculado)")
	if got := countNotifications(t, custID, db.NotifWishlistBack); got != 1 {
		t.Errorf("la campana siempre recibe el aviso, hubo %d", got)
	}
}

func wishlistRequest(t *testing.T, h gin.HandlerFunc, method, path string, customerID uuid.UUID, body any) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("customer_id", customerID.String()); c.Next() })
	r.Handle(method, "/store/wishlist", h)
	r.Handle(method, "/store/wishlist/:itemId", h)
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Lang", "en")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestListaDeDeseos_Handlers(t *testing.T) {
	conn := setupShopTestDB(t)
	f := withFakeShop(t)
	sends := stubWishlistSends(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	t.Cleanup(cleanup)
	now := time.Now().UTC()
	in, out := now.Add(-time.Hour).Format(time.RFC3339), now.Add(24*time.Hour).Format(time.RFC3339)
	setShop(f, wishShop(wishEntry("v2:/hoy", 1500, in, out, "Character_Hoy")))

	// Agregar algo que está en la tienda ahora: no genera aviso por esta aparición.
	w := wishlistRequest(t, HandlerAddWishlistItem(conn), http.MethodPost, "/store/wishlist", custID,
		map[string]string{"item_id": "Character_Hoy", "name": "  Skin de hoy  ", "item_type": "Outfit", "image": "https://fortnite-api.com/x.png"})
	if w.Code != http.StatusOK {
		t.Fatalf("agregar: %d %s", w.Code, w.Body.String())
	}
	if n, _ := runWishlistNotifier(conn, now); n != 0 {
		t.Errorf("no debe avisar de algo que el cliente agregó mientras estaba en la tienda, n=%d", n)
	}
	assertNothing(t, sends.emails, "correo")

	// La lista muestra que está en la tienda, con su precio.
	w = wishlistRequest(t, HandlerGetWishlist(conn), http.MethodGet, "/store/wishlist", custID, nil)
	var got struct {
		Items []struct {
			ItemID  string `json:"item_id"`
			Name    string `json:"name"`
			InShop  bool   `json:"in_shop"`
			PriceKC int    `json:"price_kc"`
		} `json:"items"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Items) != 1 || !got.Items[0].InShop || got.Items[0].PriceKC != 1500 || got.Items[0].Name != "Skin de hoy" {
		t.Errorf("lista = %s", w.Body.String())
	}

	// Datos inválidos se rechazan.
	for _, bad := range []map[string]string{
		{"item_id": "../../etc", "name": "x"},
		{"item_id": "Character_Ok", "name": ""},
		{"item_id": "Character_Ok", "name": "x", "image": "javascript:alert(1)"},
		{"item_id": "Character_Ok", "name": "x", "image": "http://inseguro.com/a.png"},
	} {
		if w := wishlistRequest(t, HandlerAddWishlistItem(conn), http.MethodPost, "/store/wishlist", custID, bad); w.Code != http.StatusBadRequest {
			t.Errorf("%v debería rechazarse, obtuve %d", bad, w.Code)
		}
	}

	// Límite de 30 objetos.
	for i := 1; i < db.WishlistLimit; i++ {
		if err := db.AddWishlistItem(conn, custID, db.WishlistItem{ItemID: fmt.Sprintf("Item_%d", i), Name: "x"}, "es", nil); err != nil {
			t.Fatal(err)
		}
	}
	w = wishlistRequest(t, HandlerAddWishlistItem(conn), http.MethodPost, "/store/wishlist", custID, map[string]string{"item_id": "Item_extra", "name": "x"})
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "WISHLIST_FULL") {
		t.Errorf("el objeto 31 debería rechazarse: %d %s", w.Code, w.Body.String())
	}
	// Volver a agregar uno que ya está no cuenta como nuevo.
	if err := db.AddWishlistItem(conn, custID, db.WishlistItem{ItemID: "Item_1", Name: "x"}, "es", nil); err != nil {
		t.Errorf("re-agregar un objeto existente con la lista llena debería funcionar: %v", err)
	}

	// Quitar.
	if w := wishlistRequest(t, HandlerRemoveWishlistItem(conn), http.MethodDelete, "/store/wishlist/Character_Hoy", custID, nil); w.Code != http.StatusOK {
		t.Fatalf("quitar: %d", w.Code)
	}
	items, _ := db.ListWishlistItems(conn, custID)
	for _, it := range items {
		if it.ItemID == "Character_Hoy" {
			t.Error("el objeto debería haberse quitado")
		}
	}
}

func TestNotificaciones_CampanaYEventos(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	t.Cleanup(cleanup)

	order := types.Order{ID: uuid.New(), CustomerID: custID, EpicUsername: "Receptor", ItemName: "Lote Madison Beer", PriceKC: 3400}
	notifyOrderSent(conn, order)
	notifyOrderFailed(conn, order, "motivo", true)
	db.AddNotification(conn, custID, db.NotifKCCredited, map[string]any{"amount_kc": 2400, "method": "yape"})

	list, unread, err := db.ListNotifications(conn, custID, 10)
	if err != nil || len(list) != 3 || unread != 3 {
		t.Fatalf("lista=%d unread=%d err=%v", len(list), unread, err)
	}
	kinds := map[string]bool{}
	for _, n := range list {
		kinds[n.Kind] = true
	}
	if !kinds[db.NotifOrderSent] || !kinds[db.NotifOrderFailed] || !kinds[db.NotifKCCredited] {
		t.Errorf("tipos = %v", kinds)
	}
	if err := db.MarkNotificationsRead(conn, custID); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.CountUnreadNotifications(conn, custID); n != 0 {
		t.Errorf("sin leer después de marcar = %d", n)
	}

	// Los avisos de más de 90 días se borran.
	conn.Exec(`UPDATE notifications SET created_at = NOW() - INTERVAL '91 days' WHERE customer_id=$1 AND kind=$2`, custID, db.NotifKCCredited)
	if _, err := db.DeleteOldNotifications(conn); err != nil {
		t.Fatal(err)
	}
	if list, _, _ := db.ListNotifications(conn, custID, 10); len(list) != 2 {
		t.Errorf("después de limpiar quedan %d, se esperaban 2", len(list))
	}
}
