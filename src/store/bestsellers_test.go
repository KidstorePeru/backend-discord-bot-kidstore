package store

// "LO MÁS VENDIDO DE HOY": ranking con nuestras ventas reales, limitado a lo
// que sigue en la tienda de hoy, con ventanas 24 h → 7 d → 30 d y un mínimo
// para mostrar la sección.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"KidStoreStore/src/db"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func rankFrom(byWindow map[time.Duration][]string) func(time.Duration) ([]string, error) {
	return func(w time.Duration) ([]string, error) { return byWindow[w], nil }
}

func shopSet(ids ...string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func TestBestSellers_Logica(t *testing.T) {
	day, week, month := bestSellerWindows[0], bestSellerWindows[1], bestSellerWindows[2]

	t.Run("con suficientes ventas en 24 h usa solo esa ventana, en orden", func(t *testing.T) {
		got, _ := bestSellers(shopSet("a", "b", "c", "d", "z"), rankFrom(map[time.Duration][]string{
			day: {"a", "b", "c", "d"}, week: {"z"},
		}))
		if fmt.Sprint(got) != "[a b c d]" {
			t.Errorf("obtuve %v", got)
		}
	})
	t.Run("descarta lo que ya rotó fuera de la tienda", func(t *testing.T) {
		got, _ := bestSellers(shopSet("a", "c", "d", "e"), rankFrom(map[time.Duration][]string{
			day: {"a", "ya-no-esta", "c", "d", "e"},
		}))
		if fmt.Sprint(got) != "[a c d e]" {
			t.Errorf("obtuve %v", got)
		}
	})
	t.Run("si 24 h no alcanza, completa con 7 d y luego 30 d sin repetir, las de 24 h primero", func(t *testing.T) {
		got, _ := bestSellers(shopSet("a", "b", "c", "d"), rankFrom(map[time.Duration][]string{
			day: {"b"}, week: {"a", "b"}, month: {"c", "d", "a"},
		}))
		if fmt.Sprint(got) != "[b a c d]" {
			t.Errorf("obtuve %v", got)
		}
	})
	t.Run("como máximo 8", func(t *testing.T) {
		ids := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}
		got, _ := bestSellers(shopSet(ids...), rankFrom(map[time.Duration][]string{day: ids}))
		if len(got) != bestSellersMax {
			t.Errorf("esperaba %d, obtuve %d", bestSellersMax, len(got))
		}
	})
	t.Run("si ni en 30 d se llega al mínimo, no se muestra la sección", func(t *testing.T) {
		got, _ := bestSellers(shopSet("a", "b", "c"), rankFrom(map[time.Duration][]string{
			day: {"a"}, week: {"b"}, month: {"c"},
		}))
		if got == nil || len(got) != 0 {
			t.Errorf("esperaba lista vacía (no nil), obtuve %v", got)
		}
	})
}

// insertOrderAt crea un pedido de prueba con estado y antigüedad dados.
func insertOrderAt(t *testing.T, conn *sql.DB, custID uuid.UUID, offerID, status string, ago time.Duration) {
	t.Helper()
	if _, err := conn.Exec(`INSERT INTO orders (id, customer_id, epic_username, item_offer_id, item_name, price_kc, price_vbucks, status, created_at, updated_at)
		VALUES ($1,$2,'tester',$3,'Item',100,100,$4,NOW()-$5::interval,NOW())`,
		uuid.New(), custID, offerID, status, fmt.Sprintf("%d seconds", int64(ago.Seconds()))); err != nil {
		t.Fatalf("insert order: %v", err)
	}
}

func TestGetBestSellingOfferIDs_CuentaVentasRealesEnLaVentana(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	p := "bs-" + uuid.New().String()[:8] + "-"
	A, B, C, D := p+"A", p+"B", p+"C", p+"D"

	insertOrderAt(t, conn, custID, A, "sent", time.Hour)
	insertOrderAt(t, conn, custID, A, "pending", 2*time.Hour)
	insertOrderAt(t, conn, custID, B, "sent", 3*time.Hour)
	insertOrderAt(t, conn, custID, B, "failed", time.Hour)    // no cuenta
	insertOrderAt(t, conn, custID, B, "refunded", time.Hour)  // no cuenta
	insertOrderAt(t, conn, custID, C, "sent", 30*time.Minute) // empata con B, pero más reciente
	insertOrderAt(t, conn, custID, D, "sent", 48*time.Hour)   // fuera de 24 h

	ids, err := db.GetBestSellingOfferIDs(conn, 24*time.Hour, 500)
	if err != nil {
		t.Fatal(err)
	}
	var mine []string
	for _, id := range ids {
		if len(id) > len(p) && id[:len(p)] == p {
			mine = append(mine, id[len(p):])
		}
	}
	if fmt.Sprint(mine) != "[A C B]" {
		t.Errorf("esperaba [A C B] (A: 2 ventas; C y B: 1, C más reciente; D fuera de ventana), obtuve %v", mine)
	}
}

func TestHandlerGetBestSellers_SoloObjetosDeLaTiendaActualYSinCantidades(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()
	withFakeShop(t)

	// La tienda falsa solo tiene "v2:/offer-1" con layout; se le agregan 3 más.
	entries := `{"offerId":"v2:/offer-1","finalPrice":800,"layout":{"id":"s"},"brItems":[{"name":"X"}]}`
	for _, id := range []string{"bsh-2", "bsh-3", "bsh-4"} {
		entries += fmt.Sprintf(`,{"offerId":"%s","finalPrice":100,"layout":{"id":"s"}}`, id)
	}
	entries += `,{"offerId":"bsh-sin-layout","finalPrice":100}`
	body := fmt.Sprintf(`{"status":200,"data":{"date":"2026-09-29T00:00:00Z","entries":[%s]}}`, entries)
	shopCacheMu.Lock()
	shopCache["es-419"] = &shopCacheEntry{body: []byte(body), fetchedAt: time.Now()}
	shopCacheMu.Unlock()
	bestSellersMu.Lock()
	bestSellersCache = nil
	bestSellersMu.Unlock()
	t.Cleanup(func() { conn.Exec(`DELETE FROM orders WHERE customer_id=$1`, custID) })

	for i, id := range []string{"bsh-2", "bsh-2", "bsh-2", "v2:/offer-1", "v2:/offer-1", "bsh-3", "bsh-4", "bsh-sin-layout", "bsh-ya-roto"} {
		insertOrderAt(t, conn, custID, id, "sent", time.Duration(i+1)*time.Minute)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/store/shop/bestsellers", HandlerGetBestSellers(conn))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/store/shop/bestsellers", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("esperaba 200, obtuve %d", w.Code)
	}
	var resp map[string]json.RawMessage
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp) != 1 {
		t.Errorf("la respuesta pública solo debe traer offer_ids (sin cantidades): %s", w.Body.String())
	}
	var ids []string
	json.Unmarshal(resp["offer_ids"], &ids)
	if fmt.Sprint(ids) != "[bsh-2 v2:/offer-1 bsh-3 bsh-4]" {
		t.Errorf("ranking inesperado (sin objetos rotados ni sin layout): %v", ids)
	}
}
