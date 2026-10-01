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
	body    atomic.Value // string: si no está vacío, responde 200 con este cuerpo (catálogo roto)
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
		if custom, _ := f.body.Load().(string); custom != "" {
			fmt.Fprint(w, custom)
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

// Un 200 con un catálogo roto (null, sin data, sin entradas, HTML, JSON truncado)
// NUNCA reemplaza la última tienda buena: ni en memoria ni en el respaldo en disco.
func TestFetchShopBody_CatalogoInvalidoNoReemplazaElUltimoBueno(t *testing.T) {
	for name, bad := range map[string]string{
		"null":            `null`,
		"data nula":       `{"status":200,"data":null}`,
		"sin entradas":    `{"status":200,"data":{"date":"x","entries":[]}}`,
		"status de error": `{"status":404,"error":"not found"}`,
		"html":            `<html><body>Bad gateway</body></html>`,
		"truncado":        `{"status":200,"data":{"entries":[{"offerId":"a"`,
		"arreglo":         `[1,2,3]`,
	} {
		t.Run(name, func(t *testing.T) {
			f := withFakeShop(t)
			if _, err := fetchShopBody(context.Background(), "es-419"); err != nil {
				t.Fatal(err)
			}
			expireShopCache()
			f.body.Store(bad)

			body, err := fetchShopBody(context.Background(), "es-419")
			if err != nil {
				t.Fatalf("con un catálogo bueno previo no debe fallar: %v", err)
			}
			if !isStale(t, body) {
				t.Error("debe servirse la última tienda buena marcada _stale")
			}
			if validateShopBody(body) != nil {
				t.Error("lo servido debe ser un catálogo válido")
			}
			// El respaldo en disco sigue siendo el bueno.
			disk, ok := loadShopDiskCache("es-419")
			if !ok || string(disk) != fakeShopBody {
				t.Errorf("el respaldo en disco no debe reemplazarse por un catálogo roto")
			}
			shopCacheMu.RLock()
			mem := string(shopCache["es-419"].body)
			shopCacheMu.RUnlock()
			if mem != fakeShopBody {
				t.Error("la caché en memoria no debe reemplazarse por un catálogo roto")
			}
		})
	}
}

// Sin ninguna respuesta buena previa, un catálogo roto es un error (no se sirve
// ni se guarda nada).
func TestFetchShopBody_CatalogoInvalidoSinRespaldo_DevuelveError(t *testing.T) {
	f := withFakeShop(t)
	f.body.Store(`{"status":200,"data":null}`)
	if _, err := fetchShopBody(context.Background(), "es-419"); err == nil {
		t.Fatal("un catálogo inválido sin respaldo debe devolver error")
	}
	if _, ok := loadShopDiskCache("es-419"); ok {
		t.Error("no debe haberse guardado nada en disco")
	}
}

// Un respaldo en disco dañado se ignora (no se sirve una tienda rota).
func TestLoadShopDiskCache_IgnoraRespaldoDanado(t *testing.T) {
	f := withFakeShop(t)
	if err := os.WriteFile(shopDiskCachePath("en"), []byte(`{"status":200,"data":{"entries":[`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadShopDiskCache("en"); ok {
		t.Error("un respaldo truncado debe ignorarse")
	}
	f.failing.Store(true)
	if _, err := fetchShopBody(context.Background(), "en"); err == nil {
		t.Error("con el proveedor caído y el respaldo dañado debe devolver error, no la tienda rota")
	}
}

// markStale nunca entra en pánico ni marca algo que no sea un objeto JSON.
func TestMarkStale_NoEntraEnPanicoConCuerposRaros(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `"texto"`, `42`, ``, `{`} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("markStale(%q) entró en pánico: %v", body, r)
				}
			}()
			if _, ok := markStale([]byte(body)); ok {
				t.Errorf("markStale(%q) no debe dar ok", body)
			}
		}()
	}
	out, ok := markStale([]byte(fakeShopBody))
	if !ok || !isStale(t, out) {
		t.Error("un catálogo válido debe marcarse _stale")
	}
}

// El respaldo se escribe de forma atómica: no quedan archivos temporales y el
// contenido es exactamente el último catálogo bueno.
func TestSaveShopDiskCache_EscrituraAtomica(t *testing.T) {
	withFakeShop(t)
	if _, err := fetchShopBody(context.Background(), "es-419"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(shopDiskCacheDir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("quedó un temporal: %s", e.Name())
		}
	}
	if disk, ok := loadShopDiskCache("es-419"); !ok || string(disk) != fakeShopBody {
		t.Error("el respaldo debe ser el último catálogo bueno")
	}
}


// Regresión: solo se validaba la forma general del catálogo. Una respuesta con
// alguna entrada rota (sin offerId, precio basura, contenido que no es una
// lista de objetos…) se guardaba igual y reemplazaba el último respaldo bueno.
func TestFetchShopBody_EntradaMalformadaConservaElUltimoRespaldo(t *testing.T) {
	good := `{"offerId":"v2:/ok","finalPrice":500,"regularPrice":800,"brItems":[{"name":"Bien"}]}`
	for name, bad := range map[string]string{
		"no es objeto":        `"texto"`,
		"entrada null":        `null`,
		"sin offerId":         `{"finalPrice":500,"brItems":[{"name":"X"}]}`,
		"offerId vacío":       `{"offerId":" ","finalPrice":500,"brItems":[{"name":"X"}]}`,
		"precio texto":        `{"offerId":"a","finalPrice":"gratis","brItems":[{"name":"X"}]}`,
		"precio negativo":     `{"offerId":"a","finalPrice":-1,"brItems":[{"name":"X"}]}`,
		"precio decimal":      `{"offerId":"a","finalPrice":1.5,"brItems":[{"name":"X"}]}`,
		"regular inválido":    `{"offerId":"a","finalPrice":500,"regularPrice":null,"brItems":[{"name":"X"}]}`,
		"sin contenido":       `{"offerId":"a","finalPrice":500}`,
		"brItems no es lista": `{"offerId":"a","finalPrice":500,"brItems":{"name":"X"}}`,
		"brItems con null":    `{"offerId":"a","finalPrice":500,"brItems":[null]}`,
		"layout no es objeto": `{"offerId":"a","finalPrice":500,"layout":"x","brItems":[{"name":"X"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"status":200,"data":{"date":"x","entries":[` + good + `,` + bad + `]}}`
			if validateShopBody([]byte(body)) == nil {
				t.Fatal("un catálogo con una entrada malformada debe rechazarse")
			}
			f := withFakeShop(t)
			if _, err := fetchShopBody(context.Background(), "es-419"); err != nil {
				t.Fatal(err)
			}
			expireShopCache()
			f.body.Store(body)
			served, err := fetchShopBody(context.Background(), "es-419")
			if err != nil || !isStale(t, served) {
				t.Fatalf("debe servirse el último catálogo bueno marcado _stale: err=%v", err)
			}
			if disk, ok := loadShopDiskCache("es-419"); !ok || string(disk) != fakeShopBody {
				t.Error("el respaldo en disco no debe reemplazarse")
			}
		})
	}
}

func TestValidateShopBody_AceptaEntradasValidasVariadas(t *testing.T) {
	body := `{"status":200,"data":{"date":"x","entries":[
		{"offerId":"a","finalPrice":0,"brItems":[{"name":"X"}]},
		{"offerId":"b","finalPrice":500,"regularPrice":800,"tracks":[{"title":"T"}],"layout":{"id":"l"}},
		{"offerId":"c","finalPrice":1200,"bundle":{"name":"Lote"},"brItems":[]},
		{"offerId":"d","finalPrice":300,"cars":[{"name":"C"}],"layout":null}
	]}}`
	if err := validateShopBody([]byte(body)); err != nil {
		t.Fatalf("entradas válidas no deben rechazarse: %v", err)
	}
}

// Regresión: al comprar un lote (o un tema musical, auto o instrumento), el
// pedido guardaba solo el nombre y ninguna imagen — los correos de "Pedido
// enviado" mostraban el ícono de KC en vez del producto. La imagen se elige
// igual que en la tienda (offerImages en model.ts).
func TestResolveShopItem_ImagenDeCadaTipoDeOferta(t *testing.T) {
	f := withFakeShop(t)
	f.body.Store(`{"status":200,"data":{"date":"x","entries":[
		{"offerId":"lote","finalPrice":3400,"bundle":{"name":"Lote Madison Beer","image":"https://fortnite-api.com/bundle.png"},
		 "newDisplayAsset":{"renderImages":[{"productTag":"Product.Juno","image":"https://fortnite-api.com/juno.png"},{"productTag":"Product.BR","image":"https://fortnite-api.com/render-br.png"}]},
		 "brItems":[{"name":"Madison","images":{"featured":"https://fortnite-api.com/featured.png"}}]},
		{"offerId":"lote-sin-render","finalPrice":2000,"bundle":{"name":"Lote X","image":"https://fortnite-api.com/bundle-x.png"},"brItems":[{"name":"A","images":{"icon":"https://fortnite-api.com/a.png"}}]},
		{"offerId":"tema","finalPrice":500,"tracks":[{"title":"Poker Face","albumArt":"https://cdn.fortnite-api.com/tracks/poker.jpg"}]},
		{"offerId":"auto","finalPrice":1500,"cars":[{"name":"Dominus GT","images":{"small":"https://fortnite-api.com/car-s.png","large":"https://fortnite-api.com/car-l.png"}}]},
		{"offerId":"instrumento","finalPrice":800,"instruments":[{"name":"Cetro","images":{"large":"https://fortnite-api.com/inst.png"}}]},
		{"offerId":"skin","finalPrice":1200,"brItems":[{"name":"Isaac","images":{"featured":"","icon":"https://fortnite-api.com/isaac.png"}}]}
	]}}`)
	cases := map[string]struct{ name, image string }{
		"lote":            {"Lote Madison Beer", "https://fortnite-api.com/render-br.png"},
		"lote-sin-render": {"Lote X", "https://fortnite-api.com/bundle-x.png"},
		"tema":            {"Poker Face", "https://cdn.fortnite-api.com/tracks/poker.jpg"},
		"auto":            {"Dominus GT", "https://fortnite-api.com/car-l.png"},
		"instrumento":     {"Cetro", "https://fortnite-api.com/inst.png"},
		"skin":            {"Isaac", "https://fortnite-api.com/isaac.png"},
	}
	for id, want := range cases {
		item, err := resolveShopItem(context.Background(), id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if item.Name != want.name || item.Image != want.image {
			t.Errorf("%s: obtuve nombre=%q imagen=%q, se esperaba %q / %q", id, item.Name, item.Image, want.name, want.image)
		}
	}
}
