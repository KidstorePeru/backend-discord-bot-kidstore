package store

// Pruebas del proxy de la tienda (GET /store/shop → fortnite-api.com): caché
// en memoria, respaldo en disco, UN reintento, y la respuesta marcada
// `_stale` cuando el proveedor cae — en vez de un error — siempre que haya
// habido alguna respuesta buena antes. fortnite-api.com se reemplaza por un
// httptest.Server; ninguna prueba sale a la red real ni usa la base de datos.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

const fakeShopBody = `{"status":200,"data":{"date":"2026-09-29T00:00:00Z","entries":[{"offerId":"v2:/offer-1","finalPrice":800,"brItems":[{"name":"Objeto real","images":{"icon":"https://x/icon.png"}}]}]}}`

// fakeFortniteAPI simula fortnite-api.com. failing controla si responde 503.
type fakeFortniteAPI struct {
	calls   int32
	failing atomic.Bool
}

// withFakeShop redirige el proxy a un servidor falso, con caché en memoria y
// en disco propias de esta prueba (restaura todo al terminar).
func withFakeShop(t *testing.T) *fakeFortniteAPI {
	t.Helper()
	f := &fakeFortniteAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.calls, 1)
		if f.failing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, fakeShopBody)
	}))

	prevURL, prevDir, prevTTL := shopAPIURL, shopDiskCacheDir, shopTTL
	shopCacheMu.Lock()
	prevCache, prevFail := shopCache, shopLastFailure
	shopCache = map[string]*shopCacheEntry{}
	shopLastFailure = map[string]time.Time{}
	shopCacheMu.Unlock()

	shopAPIURL = srv.URL
	shopDiskCacheDir = t.TempDir()
	t.Cleanup(func() {
		srv.Close()
		shopAPIURL, shopDiskCacheDir, shopTTL = prevURL, prevDir, prevTTL
		shopCacheMu.Lock()
		shopCache, shopLastFailure = prevCache, prevFail
		shopCacheMu.Unlock()
	})
	return f
}

func expireShopCache() {
	shopCacheMu.Lock()
	for _, e := range shopCache {
		e.fetchedAt = time.Now().Add(-2 * shopTTL)
	}
	shopCacheMu.Unlock()
}

func forgetShopMemory() {
	shopCacheMu.Lock()
	shopCache = map[string]*shopCacheEntry{}
	shopCacheMu.Unlock()
}

func isStale(t *testing.T, body []byte) bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("la respuesta no es JSON válido: %v", err)
	}
	return string(m["_stale"]) == "true"
}

func TestFetchShopBody_RespuestaFrescaSeCacheaYSeRespaldaEnDisco(t *testing.T) {
	f := withFakeShop(t)

	body, err := fetchShopBody(context.Background(), "es-419")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if isStale(t, body) {
		t.Error("una respuesta fresca del proveedor no debe marcarse _stale")
	}
	if _, err := os.Stat(filepath.Join(shopDiskCacheDir, "shop-es-419.json")); err != nil {
		t.Errorf("la respuesta buena debe respaldarse en disco: %v", err)
	}

	// Dentro del TTL no se vuelve a llamar al proveedor.
	if _, err := fetchShopBody(context.Background(), "es-419"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&f.calls); got != 1 {
		t.Errorf("con la caché fresca no debe volver a pedirse la tienda (llamadas=%d)", got)
	}
}

func TestFetchShopBody_ProveedorCaido_ReintentaUnaVezYSirveLaUltimaBuenaMarcadaStale(t *testing.T) {
	f := withFakeShop(t)
	if _, err := fetchShopBody(context.Background(), "es-419"); err != nil {
		t.Fatal(err)
	}
	expireShopCache()
	f.failing.Store(true)
	before := atomic.LoadInt32(&f.calls)

	body, err := fetchShopBody(context.Background(), "es-419")
	if err != nil {
		t.Fatalf("con una respuesta buena previa NO debe devolver error: %v", err)
	}
	if !isStale(t, body) {
		t.Error("la última tienda buena debe llegar marcada _stale")
	}
	if got := atomic.LoadInt32(&f.calls) - before; got != 2 {
		t.Errorf("debe reintentar exactamente una vez (2 intentos), hubo %d", got)
	}
	// La verificación de precios al crear un pedido sigue funcionando con los datos vencidos.
	item, err := resolveShopItem(context.Background(), "v2:/offer-1")
	if err != nil || item.FinalPrice != 800 || item.Name != "Objeto real" {
		t.Errorf("resolveShopItem con tienda _stale: item=%+v err=%v", item, err)
	}
}

// Durante una caída larga, las visitas siguientes NO vuelven a esperar al
// proveedor: reciben la copia _stale al instante hasta que pase el backoff,
// y en cuanto el proveedor vuelve, la tienda deja de estar _stale.
func TestFetchShopBody_DuranteUnaCaida_NoReintentaEnCadaVisitaYSeRecupera(t *testing.T) {
	f := withFakeShop(t)
	if _, err := fetchShopBody(context.Background(), "es-419"); err != nil {
		t.Fatal(err)
	}
	expireShopCache()
	f.failing.Store(true)
	if _, err := fetchShopBody(context.Background(), "es-419"); err != nil { // 1ª visita durante la caída: 2 intentos
		t.Fatal(err)
	}
	callsAfterFirstFailure := atomic.LoadInt32(&f.calls)

	for i := 0; i < 5; i++ {
		body, err := fetchShopBody(context.Background(), "es-419")
		if err != nil || !isStale(t, body) {
			t.Fatalf("visita %d durante la caída: err=%v stale=%v", i, err, err == nil && isStale(t, body))
		}
	}
	if got := atomic.LoadInt32(&f.calls) - callsAfterFirstFailure; got != 0 {
		t.Errorf("dentro del backoff no debe volver a llamarse al proveedor (llamadas extra=%d)", got)
	}

	// Pasado el backoff, con el proveedor de vuelta: tienda fresca otra vez.
	shopCacheMu.Lock()
	shopLastFailure["es-419"] = time.Now().Add(-2 * shopFailBackoff)
	shopCacheMu.Unlock()
	f.failing.Store(false)
	body, err := fetchShopBody(context.Background(), "es-419")
	if err != nil || isStale(t, body) {
		t.Fatalf("al volver el proveedor la tienda debe dejar de estar _stale: err=%v", err)
	}
}

func TestFetchShopBody_TrasReiniciarConElProveedorCaido_UsaElRespaldoEnDisco(t *testing.T) {
	f := withFakeShop(t)
	if _, err := fetchShopBody(context.Background(), "en"); err != nil {
		t.Fatal(err)
	}
	forgetShopMemory() // simula un reinicio del proceso: solo queda el archivo
	f.failing.Store(true)

	body, err := fetchShopBody(context.Background(), "en")
	if err != nil {
		t.Fatalf("con el respaldo en disco no debe devolver error: %v", err)
	}
	if !isStale(t, body) {
		t.Error("el respaldo en disco debe llegar marcado _stale")
	}
}

func TestFetchShopBody_SinNingunaRespuestaBuena_DevuelveError(t *testing.T) {
	f := withFakeShop(t)
	f.failing.Store(true)
	if _, err := fetchShopBody(context.Background(), "es-419"); err == nil {
		t.Fatal("sin caché ni respaldo, un proveedor caído debe devolver error")
	}
}

func TestHandlerGetShop_CabecerasYErrorGenerico(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f := withFakeShop(t)
	r := gin.New()
	r.GET("/store/shop", HandlerGetShop)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/store/shop?lang=es-419", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("esperaba 200, obtuve %d", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "public, s-maxage=600, stale-while-revalidate=1800" {
		t.Errorf("Cache-Control inesperado: %q", got)
	}

	// Idioma no permitido → se usa es-419 (nunca se reenvía texto arbitrario al proveedor).
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/store/shop?lang=../../etc", nil))
	if w.Code != http.StatusOK {
		t.Errorf("un idioma inválido debe caer en es-419, obtuve %d", w.Code)
	}

	// Sin ninguna tienda buena: 500 con un mensaje genérico (sin detalles internos).
	forgetShopMemory()
	shopDiskCacheDir = t.TempDir()
	shopCacheMu.Lock()
	shopLastFailure = map[string]time.Time{}
	shopCacheMu.Unlock()
	f.failing.Store(true)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/store/shop?lang=en", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("esperaba 500, obtuve %d", w.Code)
	}
	if body := w.Body.String(); body != `{"error":"no se pudo obtener la tienda, intenta de nuevo","success":false}` {
		t.Errorf("el error debe ser genérico, obtuve %s", body)
	}
}
