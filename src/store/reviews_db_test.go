package store

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"KidStoreStore/src/db"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestMaskName(t *testing.T) {
	cases := map[string]string{"JuanPerez23": "Ju***23", "Ana": "A***", "Pedro": "Pe***", "": "Cliente", "Ñandú_Gamer": "Ña***er"}
	for in, want := range cases {
		if got := maskName(in); got != want {
			t.Errorf("maskName(%q) = %q, se esperaba %q", in, got, want)
		}
	}
}

func deliveredOrder(t *testing.T, customerID uuid.UUID, deliveredAgo time.Duration) uuid.UUID {
	t.Helper()
	o := newShopTestOrder(t, shopTestDB, customerID, 800)
	if _, err := shopTestDB.Exec(`UPDATE orders SET status='sent', updated_at=$2 WHERE id=$1`, o.ID, time.Now().Add(-deliveredAgo)); err != nil {
		t.Fatal(err)
	}
	return o.ID
}

func TestResenas_SoloDePedidosEntregadosYTrasAprobar(t *testing.T) {
	conn := setupShopTestDB(t)
	conn.Exec(`DELETE FROM reviews`)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	t.Cleanup(cleanup)
	otherID, cleanup2 := newShopTestCustomer(t, conn, 0)
	t.Cleanup(cleanup2)
	t.Cleanup(func() { conn.Exec(`DELETE FROM reviews`) })

	sent := deliveredOrder(t, custID, time.Hour)
	old := deliveredOrder(t, custID, 100*24*time.Hour)
	pending := newShopTestOrder(t, conn, custID, 500).ID

	ids, err := db.ReviewableOrders(conn, custID)
	if err != nil || len(ids) != 1 || ids[0] != sent {
		t.Fatalf("pedidos para reseñar = %v (%v), se esperaba solo el entregado hace poco", ids, err)
	}

	for name, tc := range map[string]struct {
		customer, order uuid.UUID
	}{
		"pedido pendiente":        {custID, pending},
		"entregado hace 100 días": {custID, old},
		"pedido de otro cliente":  {otherID, sent},
		"pedido que no existe":    {custID, uuid.New()},
	} {
		if _, err := db.CreateReview(conn, tc.customer, tc.order, 5, "x", "Cl***te", "es"); !errors.Is(err, db.ErrReviewNotAllowed) {
			t.Errorf("%s: se esperaba ErrReviewNotAllowed, obtuve %v", name, err)
		}
	}

	review, err := db.CreateReview(conn, custID, sent, 4, "Llegó en minutos", "Sh***23", "es")
	if err != nil || review.Status != "pending" || review.ItemName != "Item de prueba" {
		t.Fatalf("CreateReview = %+v, %v", review, err)
	}
	if _, err := db.CreateReview(conn, custID, sent, 5, "otra", "Sh***23", "es"); !errors.Is(err, db.ErrReviewExists) {
		t.Errorf("una segunda reseña del mismo pedido debería rechazarse, obtuve %v", err)
	}
	if ids, _ := db.ReviewableOrders(conn, custID); len(ids) != 0 {
		t.Errorf("un pedido reseñado ya no debería ofrecerse, quedan %v", ids)
	}

	// Pendiente: no se publica.
	list, summary, _ := db.PublicReviews(conn, 10)
	if len(list) != 0 || summary.Count != 0 {
		t.Fatalf("una reseña sin aprobar no debe publicarse: %+v %+v", list, summary)
	}
	if err := db.ModerateReview(conn, review.ID, "approved", "¡Gracias por tu compra!"); err != nil {
		t.Fatal(err)
	}
	list, summary, _ = db.PublicReviews(conn, 10)
	if len(list) != 1 || summary.Count != 1 || summary.Average != 4 || list[0].Reply == nil || *list[0].Reply != "¡Gracias por tu compra!" {
		t.Errorf("publicada = %+v, resumen %+v", list, summary)
	}
	if err := db.ModerateReview(conn, uuid.New(), "approved", ""); err == nil {
		t.Error("moderar una reseña inexistente debería fallar")
	}
}

func TestResenas_HandlerYCifras(t *testing.T) {
	conn := setupShopTestDB(t)
	f := withFakeShop(t)
	conn.Exec(`DELETE FROM reviews`)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	t.Cleanup(cleanup)
	t.Cleanup(func() { conn.Exec(`DELETE FROM reviews`); InvalidatePublicReviews() })
	InvalidatePublicReviews()

	alerts := make(chan int, 1)
	prev := alertNewReview
	alertNewReview = func(rating int, _, _, _ string) { alerts <- rating }
	t.Cleanup(func() { alertNewReview = prev })

	orderID := deliveredOrder(t, custID, time.Hour)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("customer_id", custID.String()); c.Next() })
	r.POST("/store/reviews", HandlerCreateReview(conn))
	r.GET("/store/stats", HandlerStoreStats(conn, 10000))

	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/store/reviews", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}
	if w := post(`{"order_id":"` + orderID.String() + `","rating":9}`); w.Code != http.StatusBadRequest {
		t.Errorf("rating 9 debería rechazarse: %d", w.Code)
	}
	w := post(`{"order_id":"` + orderID.String() + `","rating":5,"comment":"  Excelente\u0007 servicio  "}`)
	if w.Code != http.StatusOK {
		t.Fatalf("crear: %d %s", w.Code, w.Body.String())
	}
	var created struct{ Review db.Review }
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.Review.Comment != "Excelente servicio" || !strings.Contains(created.Review.DisplayName, "***") {
		t.Errorf("reseña = %+v", created.Review)
	}
	select {
	case got := <-alerts:
		if got != 5 {
			t.Errorf("aviso con %d estrellas", got)
		}
	case <-time.After(2 * time.Second):
		t.Error("no llegó el aviso al admin")
	}
	if w := post(`{"order_id":"` + orderID.String() + `","rating":5}`); w.Code != http.StatusConflict {
		t.Errorf("reseña duplicada: %d", w.Code)
	}

	// Cifras: base histórica + pedidos entregados por la web; ofertas de hoy.
	var delivered int
	conn.QueryRow(`SELECT COUNT(*) FROM orders WHERE status='sent'`).Scan(&delivered)
	now := time.Now().UTC()
	setShop(f, wishShop(
		wishEntry("v2:/a", 500, now.Add(-time.Hour).Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339), "A"),
		wishEntry("v2:/b", 500, now.Add(-48*time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339), "B"),
	))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/store/stats", nil))
	var stats struct {
		OrdersDelivered int `json:"orders_delivered"`
		ShopItemsToday  int `json:"shop_items_today"`
	}
	json.Unmarshal(w.Body.Bytes(), &stats)
	if stats.OrdersDelivered != 10000+delivered || stats.ShopItemsToday != 1 {
		t.Errorf("cifras = %+v (entregados por la web: %d; solo 1 oferta sigue en la tienda)", stats, delivered)
	}
}
