package discordbot

import (
	"strings"
	"testing"
	"time"
)

func TestWishlistBackEmbed(t *testing.T) {
	out := time.Date(2026, 10, 4, 23, 59, 59, 0, time.UTC) // 18:59 del 4 en Lima
	es := wishlistBackEmbed("es", []WishlistBackItem{{Name: "Rata *radiactiva*", Image: "https://x/a.png", PriceKC: 1200, OutDate: out}})
	if es.Title != "🔔 ¡Volvió a la tienda!" || !strings.Contains(es.Description, `Rata \*radiactiva\*`) ||
		!strings.Contains(es.Description, "1200 KC (hasta el 4 oct.)") || es.Thumbnail == nil {
		t.Errorf("embed ES = %+v", es)
	}
	en := wishlistBackEmbed("en", []WishlistBackItem{{Name: "A", PriceKC: 1}, {Name: "B", PriceKC: 2}})
	if !strings.Contains(en.Title, "2 items from your wishlist") || !strings.Contains(en.Description, "View in the shop") {
		t.Errorf("embed EN = %+v", en)
	}
	var many []WishlistBackItem
	for i := 0; i < 13; i++ {
		many = append(many, WishlistBackItem{Name: "x", PriceKC: 1})
	}
	if d := wishlistBackEmbed("es", many).Description; !strings.Contains(d, "…y 3 más") {
		t.Errorf("con más de 10 objetos debería resumir el resto: %s", d)
	}
}
