package store

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"
	"KidStoreStore/src/fortnite"
	"KidStoreStore/src/middleware"
	"KidStoreStore/src/safe"
	"KidStoreStore/src/types"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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

// fallbackRates se usa cuando no hay API key configurada o la API falla y
// tampoco hay nada en caché — deja el sitio funcional (aunque con tasas
// desactualizadas) en vez de romper precios/pagos.
var fallbackRates = map[string]float64{"PEN": 1, "USD": 0.27, "EUR": 0.25}

// currentConversionRates devuelve el mapa de conversion (1 PEN = X <divisa>)
// usando el mismo caché de 24h que HandlerGetExchangeRates — la usan tanto
// el endpoint público como los cobros de dLocal Go (que necesitan saber
// cuánto es el precio en la divisa real del cliente).
func currentConversionRates() map[string]float64 {
	ratesCacheMu.RLock()
	cached := ratesCacheVal
	ratesCacheMu.RUnlock()

	if cached != nil && time.Since(cached.fetchedAt) < ratesTTL {
		var parsed struct {
			Rates map[string]float64 `json:"rates"`
		}
		if json.Unmarshal(cached.body, &parsed) == nil && len(parsed.Rates) > 0 {
			return parsed.Rates
		}
	}

	if exchangeRateAPIKey == "" {
		return fallbackRates
	}

	apiURL := fmt.Sprintf("https://v6.exchangerate-api.com/v6/%s/latest/PEN", exchangeRateAPIKey)
	resp, err := ratesClient.Get(apiURL)
	if err != nil {
		if cached != nil {
			var parsed struct{ Rates map[string]float64 `json:"rates"` }
			if json.Unmarshal(cached.body, &parsed) == nil {
				return parsed.Rates
			}
		}
		return fallbackRates
	}
	defer resp.Body.Close()

	var apiResp struct {
		Result          string             `json:"result"`
		ConversionRates map[string]float64 `json:"conversion_rates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil || apiResp.Result != "success" {
		return fallbackRates
	}

	// "rates" trae las ~160 divisas que devuelve la API (1 PEN = X divisa) —
	// se usa para mostrar el precio de referencia en la moneda local del
	// cliente y para cobrar con dLocal Go en esa misma divisa. USD/EUR se
	// mantienen también en el nivel superior por compatibilidad con el
	// código existente que ya los usa directo.
	result := gin.H{
		"USD":       apiResp.ConversionRates["USD"],
		"EUR":       apiResp.ConversionRates["EUR"],
		"rates":     apiResp.ConversionRates,
		"fetchedAt": time.Now().UnixMilli(),
	}
	body, _ := json.Marshal(result)

	ratesCacheMu.Lock()
	ratesCacheVal = &ratesCacheEntry{body: body, fetchedAt: time.Now()}
	ratesCacheMu.Unlock()

	return apiResp.ConversionRates
}

func HandlerGetExchangeRates(c *gin.Context) {
	rates := currentConversionRates()
	ratesCacheMu.RLock()
	cached := ratesCacheVal
	ratesCacheMu.RUnlock()
	if cached != nil {
		c.Data(http.StatusOK, "application/json", cached.body)
		return
	}
	c.JSON(http.StatusOK, gin.H{"USD": rates["USD"], "EUR": rates["EUR"], "rates": rates, "fetchedAt": 0})
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

	// shopFailBackoff: tras un fallo del proveedor, durante este tiempo se sirve
	// directamente la copia _stale sin volver a intentarlo. Sin esto, mientras
	// fortnite-api.com siguiera caído CADA visita esperaría dos intentos
	// fallidos (hasta 10 s si el proveedor no responde) antes de recibir la
	// copia de respaldo.
	shopFailBackoff = 60 * time.Second
	shopLastFailure = map[string]time.Time{} // protegido por shopCacheMu
)

// shopDiskCacheDir guarda la última respuesta BUENA de cada idioma en un
// archivo temporal — sobrevive a un reinicio del proceso, a diferencia de la
// caché en memoria de arriba. Nunca se lee como fuente primaria (siempre se
// prefiere una respuesta fresca del proveedor); solo respalda la caché en
// memoria cuando el proceso acaba de arrancar y fortnite-api.com falla antes
// de que haya podido rellenarla.
var shopDiskCacheDir = filepath.Join(os.TempDir(), "kidstore-shop-cache")

func shopDiskCachePath(lang string) string {
	// lang ya está restringido a "es-419"/"en" por el llamador — sin
	// caracteres que puedan escapar el directorio.
	return filepath.Join(shopDiskCacheDir, "shop-"+lang+".json")
}

// validateShopBody comprueba que una respuesta de la tienda sea un catálogo
// usable ANTES de guardarla o servirla: un objeto JSON con status 200, "data"
// no nulo y al menos una entrada. fortnite-api.com a veces responde 200 con
// {"status":200,"data":null}, una página HTML de error o un cuerpo truncado;
// sin esta comprobación eso reemplazaba el último catálogo bueno (en memoria y
// en disco) y la tienda quedaba vacía hasta la siguiente respuesta correcta.
func validateShopBody(body []byte) error {
	var parsed struct {
		Status *int `json:"status"`
		Data   *struct {
			Entries []json.RawMessage `json:"entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("catálogo con JSON inválido: %w", err)
	}
	if parsed.Status != nil && *parsed.Status != http.StatusOK {
		return fmt.Errorf("catálogo con status %d", *parsed.Status)
	}
	if parsed.Data == nil {
		return errors.New("catálogo sin data")
	}
	if len(parsed.Data.Entries) == 0 {
		return errors.New("catálogo sin entradas")
	}
	// Cada entrada se valida por separado: una respuesta con entradas rotas
	// (truncada, con otro esquema o con precios basura) indica que algo falló
	// del lado del proveedor, así que el catálogo COMPLETO se descarta y se
	// sigue sirviendo el último respaldo válido — nunca se guarda a medias.
	for i, raw := range parsed.Data.Entries {
		if err := validateShopEntry(raw); err != nil {
			return fmt.Errorf("catálogo con la entrada %d inválida: %w", i, err)
		}
	}
	return nil
}

// shopEntryContentKeys: listas de contenido de una oferta (al menos una no vacía).
var shopEntryContentKeys = []string{"brItems", "tracks", "instruments", "cars", "legoKits"}

// validateShopEntry comprueba lo mínimo que la tienda necesita de una oferta:
// un objeto con offerId, precios enteros no negativos y algún contenido
// (objetos, o un bundle). Los campos opcionales, si vienen, deben tener el tipo
// correcto (layout y bundle objetos).
func validateShopEntry(raw json.RawMessage) error {
	var entry map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entry); err != nil || entry == nil {
		return errors.New("no es un objeto")
	}
	var offerID string
	if err := json.Unmarshal(entry["offerId"], &offerID); err != nil || strings.TrimSpace(offerID) == "" {
		return errors.New("sin offerId")
	}
	for _, key := range []string{"finalPrice", "regularPrice"} {
		v, ok := entry[key]
		if !ok && key == "regularPrice" {
			continue // opcional: si falta, la tienda usa finalPrice
		}
		var price float64
		if err := json.Unmarshal(v, &price); err != nil || string(v) == "null" || price < 0 || price != float64(int64(price)) {
			return fmt.Errorf("%s inválido", key)
		}
	}
	for _, key := range []string{"layout", "bundle"} {
		if v, ok := entry[key]; ok && string(v) != "null" {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(v, &obj); err != nil {
				return fmt.Errorf("%s no es un objeto", key)
			}
		}
	}
	hasContent := false
	for _, key := range shopEntryContentKeys {
		v, ok := entry[key]
		if !ok || string(v) == "null" {
			continue
		}
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(v, &items); err != nil {
			return fmt.Errorf("%s no es una lista de objetos", key)
		}
		for _, it := range items {
			if it == nil {
				return fmt.Errorf("%s contiene un elemento vacío", key)
			}
		}
		if len(items) > 0 {
			hasContent = true
		}
	}
	if !hasContent {
		if v, ok := entry["bundle"]; !ok || string(v) == "null" {
			return errors.New("sin contenido")
		}
	}
	return nil
}

// loadShopDiskCache lee el respaldo en disco y lo descarta si está dañado
// (archivo truncado, editado a mano o de un formato viejo): un respaldo
// inválido es peor que ninguno.
func loadShopDiskCache(lang string) ([]byte, bool) {
	body, err := os.ReadFile(shopDiskCachePath(lang))
	if err != nil || len(body) == 0 {
		return nil, false
	}
	if err := validateShopBody(body); err != nil {
		slog.Warn("respaldo en disco de la tienda inválido, se ignora", "lang", lang, "error", err)
		return nil, false
	}
	return body, true
}

// saveShopDiskCache escribe el respaldo de forma atómica: primero a un archivo
// temporal y después se renombra encima del anterior. Si el proceso se cae a
// mitad de la escritura, el respaldo anterior queda intacto en vez de truncado.
func saveShopDiskCache(lang string, body []byte) {
	if err := os.MkdirAll(shopDiskCacheDir, 0o755); err != nil {
		slog.Warn("no se pudo crear el directorio de caché de la tienda", "error", err)
		return
	}
	tmp, err := os.CreateTemp(shopDiskCacheDir, "shop-"+lang+"-*.tmp")
	if err != nil {
		slog.Warn("no se pudo escribir la caché en disco de la tienda", "error", err)
		return
	}
	_, werr := tmp.Write(body)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		slog.Warn("no se pudo escribir la caché en disco de la tienda", "error", errors.Join(werr, cerr))
		return
	}
	if err := os.Rename(tmp.Name(), shopDiskCachePath(lang)); err != nil {
		os.Remove(tmp.Name())
		slog.Warn("no se pudo reemplazar la caché en disco de la tienda", "error", err)
	}
}

// markStale marca el JSON de la tienda como `_stale: true` — el frontend lo
// usa para avisar que estos datos no son la respuesta en vivo del proveedor
// (ver useShopData en el frontend). Devuelve ok=false si el cuerpo no es un
// objeto JSON: nunca se sirve como respaldo algo que no se pudo marcar (se
// vería como una tienda "en vivo"). Antes, un cuerpo "null" dejaba el mapa en
// nil sin error y la asignación de abajo tumbaba el proceso (panic).
func markStale(body []byte) ([]byte, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return nil, false
	}
	m["_stale"] = json.RawMessage("true")
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return out, true
}

// shopAPIURL apunta a la tienda real de fortnite-api.com — variable (no un
// literal embebido) para que las pruebas puedan redirigirla a un
// httptest.Server, igual que nowPaymentsBaseURL.
var shopAPIURL = "https://fortnite-api.com/v2/shop"

// staleShopBody devuelve la última tienda buena marcada _stale: la de memoria
// (aunque esté vencida) o, si el proceso acaba de arrancar, la del archivo en
// disco de la vez anterior.
func staleShopBody(lang string, entry *shopCacheEntry, inMemory bool) ([]byte, bool) {
	if inMemory {
		if stale, ok := markStale(entry.body); ok {
			return stale, true
		}
	}
	if diskBody, hit := loadShopDiskCache(lang); hit {
		return markStale(diskBody)
	}
	return nil, false
}

// requestShopOnce hace UN intento de pedirle la tienda a fortnite-api.com —
// sin caché ni reintento, eso lo maneja fetchShopBody.
func requestShopOnce(ctx context.Context, lang string) ([]byte, error) {
	url := fmt.Sprintf("%s?language=%s", shopAPIURL, lang)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("error preparando request: %w", err)
	}
	// cache: "no-store" del lado del cliente — este proceso es la única
	// caché real (memoria + disco, con su propio TTL); no hace falta (ni
	// conviene) que nada intermedio guarde también su copia.
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := shopClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error obteniendo tienda: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fortnite-api.com respondió %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error leyendo respuesta: %w", err)
	}
	// Un 200 con un catálogo inválido cuenta como fallo: se reintenta y, si
	// sigue mal, se sirve la última tienda buena — nunca reemplaza la caché.
	if err := validateShopBody(body); err != nil {
		return nil, err
	}
	return body, nil
}

// fetchShopBody devuelve el JSON crudo de la tienda actual de Fortnite (desde
// caché si sigue fresco, o pidiéndolo a fortnite-api.com si no) — lo usan
// tanto el endpoint público /store/shop como la verificación de precios al
// crear un pedido, para que ambos vean siempre los mismos datos.
//
// Si fortnite-api.com falla: reintenta UNA vez antes de rendirse, y si sigue
// fallando devuelve la última respuesta buena que haya (memoria, o si el
// proceso acaba de arrancar, el archivo temporal de la vez anterior) marcada
// `_stale`, en vez de un error — el catálogo sigue mostrándose, aunque
// desactualizado. Solo devuelve error si el proveedor falla Y nunca hubo
// ninguna respuesta buena.
func fetchShopBody(ctx context.Context, lang string) ([]byte, error) {
	shopCacheMu.RLock()
	entry, ok := shopCache[lang]
	lastFail, failed := shopLastFailure[lang]
	shopCacheMu.RUnlock()
	if ok && time.Since(entry.fetchedAt) < shopTTL {
		return entry.body, nil
	}
	if failed && time.Since(lastFail) < shopFailBackoff {
		if stale, hit := staleShopBody(lang, entry, ok); hit {
			return stale, nil
		}
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	body, err := requestShopOnce(reqCtx, lang)
	if err != nil {
		slog.Warn("HandlerGetShop: primer intento falló, reintentando una vez", "lang", lang, "error", err)
		body, err = requestShopOnce(reqCtx, lang)
	}
	if err != nil {
		shopCacheMu.Lock()
		shopLastFailure[lang] = time.Now()
		shopCacheMu.Unlock()
		if stale, hit := staleShopBody(lang, entry, ok); hit {
			return stale, nil
		}
		return nil, fmt.Errorf("error obteniendo tienda: %w", err)
	}

	shopCacheMu.Lock()
	shopCache[lang] = &shopCacheEntry{body: body, fetchedAt: time.Now()}
	delete(shopLastFailure, lang)
	shopCacheMu.Unlock()
	saveShopDiskCache(lang, body)

	return body, nil
}

func HandlerGetShop(c *gin.Context) {
	lang := c.Query("lang")
	if lang == "" { lang = "es-419" }
	if lang != "es-419" && lang != "en" { lang = "es-419" }

	body, err := fetchShopBody(c.Request.Context(), lang)
	if err != nil {
		// Ruta publica, sin autenticar — nunca devolver el error crudo (podria
		// traer detalles internos de red/infraestructura), solo loguearlo.
		slog.Error("HandlerGetShop: error obteniendo tienda", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudo obtener la tienda, intenta de nuevo"})
		return
	}
	// El cliente ya guarda su propia caché de 10 min (ver useShopData en el
	// frontend) y "Actualizar" la salta con cache:"no-store" — estas
	// cabeceras son para cualquier intermediario (CDN/navegador) entre medio:
	// puede servir esta respuesta hasta 10 min, y mientras revalida en
	// segundo plano puede seguir sirviendo la vieja hasta 30 min más.
	c.Header("Cache-Control", "public, s-maxage=600, stale-while-revalidate=1800")
	c.Data(http.StatusOK, "application/json", body)
}

// ==================== LO MÁS VENDIDO DE HOY ====================

// La tienda oficial tiene una sección "LO MÁS VENDIDO DE HOY" que fortnite-api
// no trae. Acá se arma con NUESTRAS ventas reales: los objetos que más se
// pidieron, limitados a los que siguen en la tienda de hoy (algo que ya rotó no
// se puede comprar). Si en 24 h no hay suficientes, se amplía la ventana a 7 y
// luego a 30 días; si ni así se llega al mínimo, no se muestra la sección (una
// fila casi vacía se ve peor que no tenerla).
var (
	bestSellerWindows = []time.Duration{24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour}
	bestSellersMin    = 4
	bestSellersMax    = 8
	bestSellersTTL    = 5 * time.Minute

	bestSellersMu    sync.Mutex
	bestSellersCache []string
	bestSellersAt    time.Time
	bestSellersShop  string // fecha de la tienda con la que se calculó (cambia al rotar)
)

// currentShopOfferIDs: offerIds que la tienda de hoy MUESTRA (con layout) y la
// fecha de la rotación.
func currentShopOfferIDs(ctx context.Context) (map[string]bool, string, error) {
	body, err := fetchShopBody(ctx, "es-419")
	if err != nil {
		return nil, "", err
	}
	var parsed struct {
		Data struct {
			Date    string `json:"date"`
			Entries []struct {
				OfferID string          `json:"offerId"`
				Layout  json.RawMessage `json:"layout"`
			} `json:"entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, "", err
	}
	ids := make(map[string]bool, len(parsed.Data.Entries))
	for _, e := range parsed.Data.Entries {
		if len(e.Layout) > 0 && string(e.Layout) != "null" {
			ids[e.OfferID] = true
		}
	}
	return ids, parsed.Data.Date, nil
}

// bestSellers calcula el ranking. rank(window) devuelve los más vendidos de esa
// ventana — inyectable para las pruebas.
func bestSellers(inShop map[string]bool, rank func(time.Duration) ([]string, error)) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, w := range bestSellerWindows {
		ids, err := rank(w)
		if err != nil {
			return nil, err
		}
		// Se agregan en orden: primero lo vendido en 24 h, y solo si hace falta,
		// lo de ventanas más amplias detrás.
		for _, id := range ids {
			if len(out) >= bestSellersMax {
				break
			}
			if inShop[id] && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		if len(out) >= bestSellersMin {
			break
		}
	}
	if len(out) < bestSellersMin {
		return []string{}, nil
	}
	return out, nil
}

// HandlerGetBestSellers — GET /store/shop/bestsellers → {"offer_ids": [...]}.
// Público: solo devuelve ids en orden, nunca cuántas unidades se vendieron.
func HandlerGetBestSellers(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		inShop, shopDate, err := currentShopOfferIDs(c.Request.Context())
		if err != nil {
			slog.Error("HandlerGetBestSellers: no se pudo leer la tienda", "error", err)
			c.JSON(http.StatusOK, gin.H{"offer_ids": []string{}})
			return
		}

		bestSellersMu.Lock()
		defer bestSellersMu.Unlock()
		if bestSellersCache != nil && bestSellersShop == shopDate && time.Since(bestSellersAt) < bestSellersTTL {
			c.JSON(http.StatusOK, gin.H{"offer_ids": bestSellersCache})
			return
		}
		ids, err := bestSellers(inShop, func(w time.Duration) ([]string, error) {
			// Se piden más de los que se muestran: parte puede haber rotado fuera.
			return db.GetBestSellingOfferIDs(database, w, 60)
		})
		if err != nil {
			// Sin ranking no se rompe la tienda: simplemente no aparece la sección.
			slog.Error("HandlerGetBestSellers: error consultando ventas", "error", err)
			c.JSON(http.StatusOK, gin.H{"offer_ids": []string{}})
			return
		}
		bestSellersCache, bestSellersAt, bestSellersShop = ids, time.Now(), shopDate
		c.Header("Cache-Control", "public, max-age=300")
		c.JSON(http.StatusOK, gin.H{"offer_ids": ids})
	}
}

// shopItem — lo que realmente sabemos de un item de la tienda, sacado de la
// API de Fortnite, no de lo que mande el cliente.
type shopItem struct {
	Name        string
	Image       string
	FinalPrice  int
}

// resolveShopItem busca un offerId en la tienda actual y devuelve sus datos
// REALES (precio, nombre e imagen). Nunca hay que confiar en lo que manda el
// cliente al crear un pedido — ni el precio, ni el nombre, ni la imagen —
// cualquiera podría interceptar la petición y, además de intentar pagar de
// menos, meter texto/HTML arbitrario en item_name que después se muestra tal
// cual en el correo de confirmación. Esta es la única fuente de verdad.
func resolveShopItem(ctx context.Context, offerID string) (shopItem, error) {
	body, err := fetchShopBody(ctx, "es-419")
	if err != nil {
		return shopItem{}, err
	}
	type itemImages struct {
		Featured  string `json:"featured"`
		Icon      string `json:"icon"`
		SmallIcon string `json:"smallIcon"`
		Large     string `json:"large"`
		Small     string `json:"small"`
	}
	type namedItem struct {
		Name   string     `json:"name"`
		Images itemImages `json:"images"`
	}
	var parsed struct {
		Data struct {
			Entries []struct {
				OfferID    string `json:"offerId"`
				FinalPrice int    `json:"finalPrice"`
				Bundle     *struct {
					Name  string `json:"name"`
					Image string `json:"image"`
				} `json:"bundle"`
				NewDisplayAsset *struct {
					RenderImages []struct {
						ProductTag string `json:"productTag"`
						Image      string `json:"image"`
					} `json:"renderImages"`
				} `json:"newDisplayAsset"`
				BrItems []namedItem `json:"brItems"`
				Tracks  []struct {
					Title    string `json:"title"`
					AlbumArt string `json:"albumArt"`
				} `json:"tracks"`
				Cars        []namedItem `json:"cars"`
				Instruments []namedItem `json:"instruments"`
				LegoKits    []namedItem `json:"legoKits"`
			} `json:"entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return shopItem{}, fmt.Errorf("respuesta de tienda inesperada: %w", err)
	}
	first := func(vals ...string) string {
		for _, v := range vals {
			if v != "" {
				return v
			}
		}
		return ""
	}
	for _, e := range parsed.Data.Entries {
		if e.OfferID != offerID {
			continue
		}
		item := shopItem{FinalPrice: e.FinalPrice}

		// Nombre: el del lote, o el del primer objeto de la oferta.
		switch {
		case e.Bundle != nil && e.Bundle.Name != "":
			item.Name = e.Bundle.Name
		case len(e.BrItems) > 0:
			item.Name = e.BrItems[0].Name
		case len(e.Tracks) > 0:
			item.Name = e.Tracks[0].Title
		case len(e.Cars) > 0:
			item.Name = e.Cars[0].Name
		case len(e.Instruments) > 0:
			item.Name = e.Instruments[0].Name
		case len(e.LegoKits) > 0:
			item.Name = e.LegoKits[0].Name
		}
		if item.Name == "" { item.Name = "Item" }

		// Imagen: la misma que muestra la tienda (offerImages en model.ts) —
		// portada del tema musical, render de la oferta (primero los de Battle
		// Royale), imagen del lote y, por último, la del primer objeto. Antes
		// los lotes, temas, autos e instrumentos quedaban sin imagen y los
		// correos mostraban el ícono de KC en vez del producto.
		if len(e.Tracks) > 0 && e.Tracks[0].AlbumArt != "" {
			item.Image = e.Tracks[0].AlbumArt
		}
		if item.Image == "" && e.NewDisplayAsset != nil {
			for _, br := range []bool{true, false} {
				for _, r := range e.NewDisplayAsset.RenderImages {
					if r.Image != "" && (!br || r.ProductTag == "Product.BR") {
						item.Image = r.Image
						break
					}
				}
				if item.Image != "" { break }
			}
		}
		if item.Image == "" && e.Bundle != nil { item.Image = e.Bundle.Image }
		if item.Image == "" && len(e.BrItems) > 0 {
			im := e.BrItems[0].Images
			item.Image = first(im.Featured, im.Icon, im.SmallIcon)
		}
		for _, list := range [][]namedItem{e.Cars, e.Instruments, e.LegoKits} {
			if item.Image == "" && len(list) > 0 {
				im := list[0].Images
				item.Image = first(im.Large, im.Small, im.Featured, im.Icon)
			}
		}
		return item, nil
	}
	return shopItem{}, fmt.Errorf("item no encontrado en la tienda actual")
}

// alreadyOwnedResolution es la decisión pura (sin tocar DB ni red) que toma
// processOrder cuando Epic responde ErrAlreadyOwned al intentar enviar un
// regalo — separada del resto de la función para poder probarla con un
// test unitario simple, sin necesitar una base de datos real:
//
//   - wasAlreadyAttempting=false (nunca se había intentado enviar ESTE
//     pedido antes): el receptor obtuvo el ítem por su cuenta, ANTES de
//     este pedido — nuestro pedido nunca lo entregó. Resolución:
//     "refund_not_delivered" (reembolsar, nunca marcar "sent").
//   - wasAlreadyAttempting=true (había un intento de ESTE pedido
//     interrumpido, ver MarkOrderSendAttempted): no se puede confirmar si
//     ese intento interrumpido fue el que entregó el ítem. Resolución:
//     "needs_review" (ni se inventa evidencia de entrega, ni se reembolsa
//     a ciegas arriesgando un reembolso duplicado).
func alreadyOwnedResolution(wasAlreadyAttempting bool) string {
	if wasAlreadyAttempting {
		return "needs_review"
	}
	return "refund_not_delivered"
}

// ==================== CREAR PEDIDO ====================

const maxPendingOrdersPerCustomer = 10

// friendGracePeriod — ver el comentario en processOrder, caso "el receptor
// no es amigo de ningún bot". 10 minutos cubren al menos un ciclo completo
// del aceptador automático de solicitudes de amistad (corre cada 5 min).
const friendGracePeriod = 10 * time.Minute

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

		// ── Verificar el item contra la tienda real ──
		// El cliente podría manipular price_kc/price_vbucks/item_name/item_image
		// directamente en la petición (editando la request desde el navegador)
		// para pedir un item caro pagando casi nada — el bot igual gastaría sus
		// VBucks reales enviándolo — o para meter texto/HTML arbitrario en
		// item_name, que después se muestra tal cual en el correo de
		// confirmación. Nunca hay que confiar en nada de esto: se reemplaza
		// todo por los datos reales de fortnite-api.com.
		item, err := resolveShopItem(c.Request.Context(), req.ItemOfferID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "no se pudo verificar el item en la tienda actual: " + err.Error()})
			return
		}
		// 1 VBuck = 1 KC (igual que vbucksToKC en el frontend) — si esa tasa
		// cambia algún día, hay que actualizarla también acá.
		expectedKC := req.PriceVBucks
		if item.FinalPrice != req.PriceVBucks || req.PriceKC != expectedKC {
			slog.Warn("Posible manipulación de precio en pedido bloqueada",
				"customer", customerID, "offerID", req.ItemOfferID,
				"price_kc_reclamado", req.PriceKC, "price_vbucks_reclamado", req.PriceVBucks, "ip", c.ClientIP())
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "el precio del item no coincide con la tienda actual"})
			return
		}
		// Nunca se usa el item_image que mande el cliente — solo el real del
		// catálogo, o vacío (las plantillas ya manejan ese caso con un ícono
		// de reemplazo).
		req.ItemName = item.Name
		req.ItemImage = item.Image

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

		// ── Límite de pedidos pendientes por cliente (máx. 10) — la cuenta
		// se hace ahora dentro de la misma transacción que descuenta el
		// saldo y crea el pedido (ver DeductKCAndCreateOrder), para que dos
		// compras simultáneas del mismo cliente no puedan las dos pasar la
		// comprobación antes de que cualquiera inserte su pedido. ──
		order, err := db.DeductKCAndCreateOrder(database, customerID, customer.EpicUsername, req, maxPendingOrdersPerCustomer)
		if err != nil {
			if strings.Contains(err.Error(), "insufficient") {
				c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			} else if strings.Contains(err.Error(), "too many pending orders") {
				c.JSON(http.StatusTooManyRequests, gin.H{
					"success": false,
					"error":   fmt.Sprintf("Tienes %d pedidos pendientes. Espera a que se procesen antes de crear nuevos (máximo %d).", maxPendingOrdersPerCustomer, maxPendingOrdersPerCustomer),
					"code":    "TOO_MANY_ORDERS",
				})
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

// HandlerGetMyOrderStats devuelve totales calculados directamente en la
// base de datos (no sobre una página de resultados ya recortada) — /perfil
// los usaba mal: mostraba len(orders) de un solo lote (como mucho 100
// pedidos, antes ni eso por el bug del límite) como si fuera el total real
// del cliente. GetCustomerOrderStats ya existía y la usaba el bot de
// Discord, pero nunca se había expuesto por HTTP para el sitio web.
func HandlerGetMyOrderStats(database *sql.DB) gin.HandlerFunc {
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
		total, sent, pending, spentKC, err := db.GetCustomerOrderStats(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo estadísticas"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "total_orders": total, "sent_orders": sent, "pending_orders": pending, "total_spent_kc": spentKC})
	}
}

// HandlerGetMyRechargeStats — mismo problema, mismo remedio que
// HandlerGetMyOrderStats pero para recargas: el dashboard sumaba amount_pen
// de TODOS los pagos por pasarela sin filtrar por estado (incluía
// pendientes y fallidos como si fueran plata realmente cobrada) y además
// solo sobre los últimos 50 pagos cargados. GetCustomerRechargeStats
// calcula ambos números correctamente sobre todo el historial.
func HandlerGetMyRechargeStats(database *sql.DB) gin.HandlerFunc {
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
		totalKC, totalPEN, pendingPayments, err := db.GetCustomerRechargeStats(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo estadísticas"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "total_kc_recharged": totalKC, "total_pen_recharged": totalPEN, "pending_payments": pendingPayments})
	}
}

// ==================== COMPROBANTE DE PEDIDO ====================

// HandlerOrderVoucher devuelve los datos para la página de comprobante de un
// pedido — solo si ya se entregó ("sent") y le pertenece al cliente autenticado.
func HandlerOrderVoucher(database *sql.DB) gin.HandlerFunc {
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
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id inválido"})
			return
		}
		order, err := db.GetOrderByID(database, id)
		if err != nil || order.CustomerID != customerID {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "comprobante no encontrado"})
			return
		}
		if order.Status != "sent" {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "este pedido todavía no tiene comprobante"})
			return
		}
		customer, err := db.GetCustomerByID(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo cliente"})
			return
		}
		itemImage := ""
		if order.ItemImage != nil { itemImage = *order.ItemImage }
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"voucher": gin.H{
				"type":          "order",
				"reference":     strings.ToUpper(id.String()[:8]),
				"customer_name": customer.EpicUsername,
				"item_name":     order.ItemName,
				"item_image":    itemImage,
				"epic_username": order.EpicUsername,
				"price_kc":      order.PriceKC,
				"price_vbucks":  order.PriceVBucks,
				"status":        order.Status,
				"created_at":    order.CreatedAt,
				// delivery_confirmed: true si Epic Games confirmó el envío con
				// su propia respuesta (evidencia guardada server-side). No se
				// expone el JSON crudo al cliente, solo la confirmación —
				// el detalle técnico queda para uso interno en disputas de pago.
				"delivery_confirmed": order.DeliveryEvidence != nil,
				"delivered_at":       order.UpdatedAt,
			},
		})
	}
}

// ==================== WORKER ====================

var encryptionKey string

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
				safe.Run("StartOrderWorker.processOrders", func() { processOrders(database) })
			}
		}
	}()
}

func processOrders(database *sql.DB) {
	// Antes que nada: entregas REALES cuya evidencia no se pudo guardar en un
	// ciclo anterior (ver persistDelivery) — se guardan sin volver a enviar nada.
	recoverUnpersistedDeliveries(database)

	// ClaimPendingOrders (no GetPendingOrders) — reclama los pedidos de forma
	// atómica (FOR UPDATE SKIP LOCKED) para que, si local y producción llegan
	// a correr al mismo tiempo contra la misma base compartida, nunca puedan
	// tomar y enviar el mismo pedido dos veces.
	orders, err := db.ClaimPendingOrders(database)
	if err != nil || len(orders) == 0 { return }
	// Un pedido reclamado que YA se entregó (su evidencia sigue esperando a
	// guardarse) no vuelve a pasar por el envío ni, sobre todo, por el
	// reembolso de "sin bots activos" de abajo.
	orders = withoutUnpersistedDeliveries(database, orders)
	if len(orders) == 0 { return }

	accounts, err := db.GetActiveGameAccounts(database, encryptionKey)
	if err != nil || len(accounts) == 0 {
		slog.Warn("Worker: no hay cuentas bot activas disponibles")
		discordbot.AlertNoActiveBots(len(orders))
		noBotsMsg := "Sin cuentas bot activas disponibles."
		for _, order := range orders {
			failOrderAndRefund(database, order, noBotsMsg, "No había cuentas disponibles para procesar tu pedido en ese momento.")
		}
		return
	}

	for _, order := range orders {
		o := order
		// Un panic al procesar UN pedido (ej. datos raros del cliente) no
		// debe frenar el resto del lote de este mismo ciclo.
		safe.Run("processOrders.processOrder", func() { processOrder(database, o, accounts) })
	}
}

// failOrderAndRefund centraliza lo que antes estaba repetido (con
// variaciones inconsistentes) en cada punto donde un pedido falla: marca el
// pedido fallido, intenta reembolsar el KC, y SOLO afirma en el audit log y
// en el correo al cliente que "ya se reembolsó" cuando el reembolso
// realmente se confirmó. Antes, algunas ramas ni siquiera revisaban el
// error de RefundOrder y notificaban éxito igual — si el reembolso fallaba
// (ej. un error transitorio de DB), el pedido quedaba 'failed' sin su KC
// devuelto, pero el cliente recibía un correo diciendo "ya te devolvimos el
// KC completo". El status del pedido queda como señal honesta de si el
// reembolso se completó: RefundOrder deja el pedido en 'refunded' recién
// cuando de verdad se ejecuta — si falla, el pedido se queda en 'failed'
// (RefundOrder no vuelve a rechazarlo por eso, así que es seguro
// reintentarlo más tarde — ver RetryFailedRefunds).
//
// customerReason NUNCA debe afirmar por su cuenta si el reembolso se
// completó ("se reembolsó tu KC") — eso lo decide esta función según el
// resultado real de RefundOrder, y ya queda reflejado aparte en el correo
// (SendOrderFailedEmail cambia intro/asunto según refunded). Si el texto
// del motivo también lo afirmara a ciegas, el correo podría contradecirse a
// sí mismo (intro: "reembolso en proceso" / motivo: "ya se reembolsó").
//
// Antes de reembolsar, comprueba si queda un intento de envío sin resolver
// (send_attempted, ver MarkOrderSendAttempted) — puede venir de ESTE mismo
// ciclo o de uno anterior que se cayó a mitad de camino o perdió la
// respuesta de Epic (fortnite.ErrRequestUncertain). Si lo hay, NO es seguro
// concluir "no se entregó": ese intento sin resolver podría haber
// completado la entrega en Epic sin que quede registro acá, y reembolsar
// en ese caso arriesgaría duplicar el pago. Esta comprobación y el paso a
// 'review' se hacen en UNA sola transacción atómica (ResolveOrderReview) —
// nunca en dos escrituras separadas, para que un fallo o una caída del
// proceso entre "decidir" y "guardar" no pueda dejar la marca limpia sin
// que 'review' quedara persistido. Este chequeo protege TODOS los puntos
// del worker que llaman a esta función, no solo el que compara contra
// ErrAlreadyOwned.
//
// Además, la transición a 'failed' es idempotente: MarkOrderFailedIfNotTerminal
// solo se aplica si el pedido no está YA en un estado definitivo
// (refunded/sent/review) — así, si failOrderAndRefund se llama más de una
// vez para el mismo pedido (reintento, condición de carrera, un reinicio a
// mitad de un reembolso previo), una llamada posterior nunca puede pisar
// 'refunded' de vuelta a 'failed' y disparar un segundo reembolso real.
func failOrderAndRefund(database *sql.DB, order types.Order, internalReason, customerReason string) {
	reviewNote := fmt.Sprintf("%s — pero había un intento de envío sin resolver; no se puede confirmar si se entregó, así que no se reembolsó automáticamente", internalReason)
	wasAttempting, resolveErr := db.ResolveOrderReview(database, order.ID, reviewNote)
	if errors.Is(resolveErr, db.ErrOrderHasDeliveryEvidence) {
		// Otro trabajador (o una recuperación) ya registró la entrega: un fallo
		// atrasado no la revierte ni la reembolsa.
		slog.Warn("failOrderAndRefund: el pedido ya tiene evidencia de entrega, no se reembolsa", "orderID", order.ID)
		return
	}
	if resolveErr != nil {
		slog.Error("failOrderAndRefund: no se pudo resolver el intento de envío, se reintenta en el próximo ciclo", "orderID", order.ID, "error", resolveErr)
		return
	}
	if wasAttempting {
		discordbot.AlertOrderNeedsReview(order.ID.String(), order.EpicUsername, order.ItemName)
		db.AddAuditLog(database, &order.CustomerID, "ORDER_NEEDS_REVIEW", reviewNote, "worker")
		slog.Warn("failOrderAndRefund: pedido en revisión manual en vez de reembolso automático — entrega incierta", "orderID", order.ID)
		return
	}

	applied, applyErr := db.MarkOrderFailedIfNotTerminal(database, order.ID, internalReason)
	if applyErr != nil {
		slog.Error("failOrderAndRefund: error marcando el pedido como fallido, se reintenta en el próximo ciclo", "orderID", order.ID, "error", applyErr)
		return
	}
	if !applied {
		// El pedido ya estaba en un estado definitivo (refunded/sent/review)
		// — nada que hacer. Esto es justo lo que evita un segundo reembolso
		// si failOrderAndRefund se llama de nuevo para el mismo pedido.
		slog.Info("failOrderAndRefund: el pedido ya estaba en un estado definitivo, no se repite la acción", "orderID", order.ID)
		return
	}

	refundErr := db.RefundOrder(database, order.ID)
	refunded := refundErr == nil

	if refunded {
		db.AddAuditLog(database, &order.CustomerID, "ORDER_FAILED",
			fmt.Sprintf("pedido %s: %s — KC reembolsados", order.ID, internalReason), "worker")
	} else {
		slog.Error("Worker: no se pudo reembolsar el pedido, queda pendiente de reintento", "orderID", order.ID, "error", refundErr)
		db.AddAuditLog(database, &order.CustomerID, "ORDER_FAILED",
			fmt.Sprintf("pedido %s: %s — REEMBOLSO FALLÓ (%s), pendiente de reintento automático", order.ID, internalReason, refundErr), "worker")
	}

	notifyOrderFailed(database, order, customerReason, refunded)
}

// RetryFailedRefunds reintenta el reembolso de pedidos que quedaron en
// 'failed' sin que su devolución de KC se haya confirmado nunca (ver
// failOrderAndRefund) — la red de recuperación para el caso que el punto 11
// pedía explícitamente: "permite recuperar devoluciones pendientes". Se
// llama periódicamente desde main.go. Seguro de reintentar cualquier
// cantidad de veces: RefundOrder por sí mismo rechaza reembolsar un pedido
// que ya esté 'refunded' (o 'sent'), así que nunca duplica el KC devuelto.
func RetryFailedRefunds(database *sql.DB) {
	orders, err := db.GetOrdersPendingRefund(database)
	if err != nil {
		slog.Error("RetryFailedRefunds: error listando pedidos", "error", err)
		return
	}
	for _, order := range orders {
		o := order
		safe.Run("RetryFailedRefunds.order", func() {
			if err := db.RefundOrder(database, o.ID); err != nil {
				slog.Warn("RetryFailedRefunds: reembolso sigue fallando, se reintentará más tarde", "orderID", o.ID, "error", err)
				return
			}
			slog.Info("RetryFailedRefunds: reembolso pendiente recuperado", "orderID", o.ID, "customer", o.CustomerID)
			db.AddAuditLog(database, &o.CustomerID, "ORDER_REFUND_RECOVERED",
				fmt.Sprintf("pedido %s: reembolso pendiente se completó en un reintento automático", o.ID), "worker")
		})
	}
}

// notifyOrderFailed avisa por correo que un pedido no se pudo completar —
// "refunded" refleja si el KC de verdad ya se devolvió (nunca se afirma sin
// confirmarlo primero, ver failOrderAndRefund). "reason" debe ser un texto
// ya pensado para el cliente, no el error técnico crudo.
func notifyOrderFailed(database *sql.DB, order types.Order, reason string, refunded bool) {
	customer, err := db.GetCustomerByID(database, order.CustomerID)
	if err != nil {
		return
	}
	if customer.Email != nil && *customer.Email != "" {
		itemImage := ""
		if order.ItemImage != nil { itemImage = *order.ItemImage }
		go SendOrderFailedEmail(smtpConfig, *customer.Email, order.EpicUsername, order.ItemName, itemImage, order.PriceKC, reason, refunded, "es")
	}
}

// ── Entregas confirmadas por Epic cuya escritura en la base falló ──
//
// Antes, si MarkOrderDelivered fallaba (base caída un instante, conexión
// cortada), el worker marcaba el pedido 'sent' con UpdateOrderStatus: sin
// evidencia de entrega (nada que mostrar en una disputa de pago) y con
// send_attempted todavía en true. Y si esa segunda escritura también fallaba,
// el pedido quedaba 'processing' sin rastro de que el regalo SÍ se entregó.
//
// Ahora: se reintenta unas veces; si sigue fallando, la evidencia se guarda en
// memoria (y en los logs, completa) y el pedido NO se marca 'sent' sin ella.
// Cada ciclo del worker reintenta guardarla (recoverUnpersistedDeliveries) y un
// pedido reclamado con evidencia pendiente nunca se reenvía ni se reembolsa
// (withoutUnpersistedDeliveries). Si el proceso se reinicia antes de lograrlo,
// send_attempted sigue en true: al reclamarlo, Epic responde "ya lo tiene" y el
// pedido pasa a revisión manual — nunca se reembolsa a ciegas. Los cupos y
// V-Bucks del bot se descuentan una sola vez, en el envío real.

type unpersistedDelivery struct {
	OrderID      uuid.UUID `json:"order_id"`
	CustomerID   uuid.UUID `json:"customer_id"`
	EpicUsername string    `json:"epic_username"`
	ItemName     string    `json:"item_name"`
	BotID        uuid.UUID `json:"bot_id"`
	Evidence     string    `json:"evidence"`
	CapturedAt   time.Time `json:"captured_at"`
}

func (d unpersistedDelivery) order() types.Order {
	return types.Order{ID: d.OrderID, CustomerID: d.CustomerID, EpicUsername: d.EpicUsername, ItemName: d.ItemName}
}

var (
	unpersistedDeliveriesMu sync.Mutex
	unpersistedDeliveries   = map[uuid.UUID]unpersistedDelivery{}
	// Esperas entre reintentos inmediatos de MarkOrderDelivered (variable para las pruebas).
	deliveryPersistRetryDelays = []time.Duration{0, 500 * time.Millisecond, 2 * time.Second}
	// Diario en disco de entregas confirmadas por Epic que no se pudieron guardar
	// en la base: sobrevive a un reinicio del proceso y lo comparten los procesos
	// de la misma máquina. DELIVERY_JOURNAL_DIR permite apuntarlo a un volumen
	// persistente (en Railway el disco del contenedor se pierde al redesplegar;
	// ahí el respaldo final es la revisión manual + la evidencia en los logs).
	deliveryJournalDir = defaultDeliveryJournalDir()
)

func defaultDeliveryJournalDir() string {
	if dir := strings.TrimSpace(os.Getenv("DELIVERY_JOURNAL_DIR")); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), "kidstore-delivery-journal")
}

func deliveryJournalPath(id uuid.UUID) string {
	return filepath.Join(deliveryJournalDir, id.String()+".json")
}

// writeDeliveryJournal guarda la entrega pendiente de forma atómica (archivo
// temporal + fsync + rename): nunca queda un archivo a medias.
func writeDeliveryJournal(d unpersistedDelivery) error {
	if err := os.MkdirAll(deliveryJournalDir, 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(d)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(deliveryJournalDir, d.OrderID.String()+"-*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(body)
	serr := tmp.Sync()
	cerr := tmp.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), deliveryJournalPath(d.OrderID)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func removeDeliveryJournal(id uuid.UUID) {
	if err := os.Remove(deliveryJournalPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("Worker: no se pudo borrar la entrada del diario de entregas", "orderID", id, "error", err)
	}
}

// loadDeliveryJournal incorpora las entregas pendientes del diario (de antes
// de un reinicio, o de otro proceso de la misma máquina). Una entrada ilegible
// se aparta como .bad para revisión manual en vez de reintentarse para siempre.
func loadDeliveryJournal() {
	files, err := filepath.Glob(filepath.Join(deliveryJournalDir, "*.json"))
	if err != nil {
		return
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var d unpersistedDelivery
		if json.Unmarshal(body, &d) != nil || d.OrderID == uuid.Nil || !db.ValidDeliveryEvidence(d.Evidence) {
			slog.Error("Worker: entrada del diario de entregas ilegible, se aparta para revisión manual", "file", f)
			os.Rename(f, f+".bad")
			continue
		}
		unpersistedDeliveriesMu.Lock()
		if _, known := unpersistedDeliveries[d.OrderID]; !known {
			unpersistedDeliveries[d.OrderID] = d
		}
		unpersistedDeliveriesMu.Unlock()
	}
}

func forgetUnpersistedDelivery(id uuid.UUID) {
	unpersistedDeliveriesMu.Lock()
	delete(unpersistedDeliveries, id)
	unpersistedDeliveriesMu.Unlock()
	removeDeliveryJournal(id)
}

// sendInconclusive deja el pedido en revisión manual cuando no se puede saber
// si el regalo llegó (Epic no respondió de forma concluyente, o un intento
// anterior quedó interrumpido): NUNCA se reenvía el regalo a ciegas (podría
// duplicarse) ni se reembolsa a ciegas (podría haberse entregado). La escritura
// no aplica si otro trabajador ya registró la entrega.
func sendInconclusive(database *sql.DB, order types.Order, note string) {
	if err := db.SetOrderReviewOrClear(database, order.ID, true, note); err != nil {
		slog.Error("Worker: no se pudo pasar el pedido a revisión, se reintenta en el próximo ciclo", "orderID", order.ID, "error", err)
		return
	}
	discordbot.AlertOrderNeedsReview(order.ID.String(), order.EpicUsername, order.ItemName)
	db.AddAuditLog(database, &order.CustomerID, "ORDER_NEEDS_REVIEW", note, "worker")
	slog.Warn("Worker: pedido en revisión manual — resultado de envío no concluyente", "orderID", order.ID)
}

// persistDelivery guarda la entrega (estado 'sent' + evidencia) con reintentos.
// Devuelve false si no se pudo: la entrega queda pendiente (memoria + diario en
// disco) y nunca se marca 'sent' sin evidencia.
func persistDelivery(database *sql.DB, order types.Order, botID uuid.UUID, evidence string) bool {
	var err error
	for _, delay := range deliveryPersistRetryDelays {
		if delay > 0 { time.Sleep(delay) }
		if err = db.MarkOrderDelivered(database, order.ID, botID, evidence); err == nil {
			return true
		}
		if errors.Is(err, db.ErrOrderAlreadyRefunded) {
			reportDeliveredButRefunded(database, order, evidence)
			return false
		}
		if errors.Is(err, db.ErrInvalidDeliveryEvidence) {
			// No debería pasar (SendGift ya valida la respuesta de Epic), pero si
			// pasa no se registra una entrega sin prueba: revisión manual.
			sendInconclusive(database, order, fmt.Sprintf("Epic respondió al envío con una evidencia vacía o inválida — no se registra como entregado ni se reembolsa; confirmar en Epic si el ítem llegó. bot=%s", botID))
			return false
		}
	}
	d := unpersistedDelivery{
		OrderID: order.ID, CustomerID: order.CustomerID, EpicUsername: order.EpicUsername, ItemName: order.ItemName,
		BotID: botID, Evidence: evidence, CapturedAt: time.Now().UTC(),
	}
	unpersistedDeliveriesMu.Lock()
	unpersistedDeliveries[order.ID] = d
	unpersistedDeliveriesMu.Unlock()
	journalErr := writeDeliveryJournal(d)
	// La evidencia completa va al log: es el último respaldo si se pierden la
	// memoria y el diario (redespliegue) — el pedido termina en revisión manual.
	slog.Error("Worker: entrega sin persistir — Epic CONFIRMÓ el regalo pero no se pudo guardar; se reintenta sin reenviar",
		"orderID", order.ID, "bot", botID, "recipient", order.EpicUsername, "item", order.ItemName,
		"evidence", evidence, "error", err, "journalError", journalErr)
	discordbot.AlertDeliveryNotPersisted(order.ID.String(), order.EpicUsername, order.ItemName)
	db.AddAuditLog(database, &order.CustomerID, "ORDER_DELIVERY_UNPERSISTED",
		fmt.Sprintf("pedido %s entregado por el bot %s pero no se pudo guardar la entrega (%v); se reintenta sin reenviar", order.ID, botID, err), "worker")
	return false
}

// retryUnpersistedDelivery vuelve a intentar guardar una entrega pendiente.
// Devuelve true si el pedido tenía una entrega pendiente (guardada o no ahora).
// Es segura entre procesos: MarkOrderDelivered es idempotente y nunca pisa un
// pedido reembolsado, y el diario se borra solo cuando la entrega quedó resuelta.
func retryUnpersistedDelivery(database *sql.DB, orderID uuid.UUID) bool {
	unpersistedDeliveriesMu.Lock()
	pending, ok := unpersistedDeliveries[orderID]
	unpersistedDeliveriesMu.Unlock()
	if !ok {
		return false
	}
	err := db.MarkOrderDelivered(database, orderID, pending.BotID, pending.Evidence)
	switch {
	case err == nil:
		slog.Info("Worker: entrega pendiente guardada (sin reenviar el regalo)", "orderID", orderID)
		db.AddAuditLog(database, &pending.CustomerID, "ORDER_DELIVERY_RECOVERED",
			fmt.Sprintf("pedido %s: se guardó la evidencia de una entrega que había fallado al persistir", orderID), "worker")
	case errors.Is(err, db.ErrOrderAlreadyRefunded):
		reportDeliveredButRefunded(database, pending.order(), pending.Evidence)
	case errors.Is(err, db.ErrInvalidDeliveryEvidence):
		sendInconclusive(database, pending.order(), "la evidencia pendiente de guardar resultó inválida — confirmar en Epic si el ítem llegó")
	default:
		slog.Warn("Worker: la entrega pendiente sigue sin poder guardarse, se reintenta en el próximo ciclo", "orderID", orderID, "error", err)
		return true
	}
	forgetUnpersistedDelivery(orderID)
	return true
}

func recoverUnpersistedDeliveries(database *sql.DB) {
	loadDeliveryJournal()
	unpersistedDeliveriesMu.Lock()
	ids := make([]uuid.UUID, 0, len(unpersistedDeliveries))
	for id := range unpersistedDeliveries {
		ids = append(ids, id)
	}
	unpersistedDeliveriesMu.Unlock()
	for _, id := range ids {
		retryUnpersistedDelivery(database, id)
	}
}

func withoutUnpersistedDeliveries(database *sql.DB, orders []types.Order) []types.Order {
	kept := orders[:0]
	for _, o := range orders {
		if retryUnpersistedDelivery(database, o.ID) {
			continue
		}
		kept = append(kept, o)
	}
	return kept
}

// reportDeliveredButRefunded: Epic entregó el regalo pero el pedido ya estaba
// reembolsado — no se toca el pedido (no se "des-reembolsa" solo); se avisa.
func reportDeliveredButRefunded(database *sql.DB, order types.Order, evidence string) {
	slog.Error("Worker: el regalo se entregó pero el pedido ya estaba reembolsado — requiere revisión",
		"orderID", order.ID, "recipient", order.EpicUsername, "evidence", evidence)
	discordbot.AlertOrderNeedsReview(order.ID.String(), order.EpicUsername, order.ItemName)
	db.AddAuditLog(database, &order.CustomerID, "ORDER_DELIVERED_AFTER_REFUND",
		fmt.Sprintf("pedido %s: Epic confirmó la entrega pero el pedido ya estaba reembolsado", order.ID), "worker")
}

// processOrder intenta enviar un pedido probando cada bot disponible en orden.
// Si un bot falla por gift_limit_reached o token inválido, pasa al siguiente bot
// en el mismo ciclo sin esperar 30 segundos.
func processOrder(database *sql.DB, order types.Order, accounts []types.GameAccount) {
	// Un intento de envío anterior de ESTE pedido quedó sin resolver (el proceso
	// se cayó durante la llamada a Epic, o su respuesta se perdió): no se vuelve a
	// llamar a Epic para "ver qué pasa" — eso sería reenviar a ciegas. Revisión.
	if attempted, err := db.OrderSendAttempted(database, order.ID); err != nil {
		slog.Error("Worker: no se pudo leer el estado de envío del pedido, se reintenta en el próximo ciclo", "orderID", order.ID, "error", err)
		db.UpdateOrderStatus(database, order.ID, "pending", nil, nil)
		return
	} else if attempted {
		sendInconclusive(database, order, "un intento de envío anterior de este pedido quedó interrumpido sin respuesta de Epic — no se reenvía ni se reembolsa automáticamente; confirmar en Epic si el ítem llegó")
		return
	}

	// Verificar que al menos un bot tiene slots
	hasSlots := false
	for i := range accounts {
		if accounts[i].RemainingGifts > 0 { hasSlots = true; break }
	}
	if !hasSlots {
		noSlotsMsg := "Todas las cuentas bot han agotado sus envíos del día. Los gifts se resetean diariamente."
		slog.Warn("Worker: sin slots en ningún bot", "orderID", order.ID)
		discordbot.AlertNoActiveBots(1)
		db.UpdateOrderStatus(database, order.ID, "pending", nil, &noSlotsMsg)
		return
	}

	// (El pedido ya quedó marcado "processing" al reclamarlo en ClaimPendingOrders.)

	// Obtener el Epic account ID del receptor (igual para todos los bots, basta con uno).
	// Antes, el primer error de CUALQUIER bot (token vencido, límite de
	// solicitudes, Epic caído, un fallo de red) se trataba igual que un 404
	// real de Epic: cancelaba el pedido de inmediato diciéndole al cliente
	// que su usuario podía estar mal escrito, sin siquiera probar otro bot.
	// Ahora se prueban todos los bots disponibles, y solo un
	// fortnite.ErrEpicUserNotFound (404 real de Epic) se toma como
	// confirmación de que el usuario no existe — cualquier otro fallo es
	// temporal y no dice nada sobre si el usuario existe o no.
	var receiverAccountID string
	userConfirmedNotFound := false
	for i := range accounts {
		if accounts[i].RemainingGifts <= 0 { continue }
		id, err := fortnite.GetReceiverAccountID(database, accounts[i], order.EpicUsername)
		if err != nil {
			if errors.Is(err, fortnite.ErrEpicUserNotFound) {
				// Concluyente: no depende de qué bot preguntó, así que no hace
				// falta seguir probando los demás.
				userConfirmedNotFound = true
				break
			}
			slog.Warn("Worker: no se pudo verificar el usuario Epic con este bot, probando el siguiente",
				"orderID", order.ID, "bot", accounts[i].DisplayName, "error", err)
			continue
		}
		receiverAccountID = id
		break
	}

	if userConfirmedNotFound {
		errMsg := fmt.Sprintf("Epic confirmó (404) que el usuario '%s' no existe", order.EpicUsername)
		slog.Error("Worker: usuario Epic no encontrado, confirmado por Epic", "orderID", order.ID)
		failOrderAndRefund(database, order, errMsg, fmt.Sprintf("No pudimos encontrar la cuenta de Epic Games '%s'. Verifica que el usuario esté bien escrito.", order.EpicUsername))
		return
	}

	if receiverAccountID == "" {
		// Ningún bot pudo confirmar NI descartar al usuario — todos los
		// intentos fallaron por motivos temporales (token, límite de
		// solicitudes, Epic caído, red). Nunca se cancela el pedido ni se le
		// dice al cliente que su usuario está mal escrito por algo que no se
		// pudo verificar: se deja pendiente para que el próximo ciclo del
		// worker lo reintente.
		pendingMsg := "No pudimos verificar tu usuario de Epic por un problema temporal, reintentando automáticamente."
		slog.Warn("Worker: no se pudo resolver el usuario Epic con ningún bot disponible (fallos temporales), se reintenta", "orderID", order.ID)
		db.UpdateOrderStatus(database, order.ID, "pending", nil, &pendingMsg)
		return
	}

	// Contadores para determinar el resultado final si todos los bots fallan.
	// friendBots/notFriendBots/friendCheckErrorBots se calculan para TODOS
	// los bots de la cuenta, SIN IMPORTAR si tienen fondos NI cupos — antes
	// la amistad recién se comprobaba después de pasar el chequeo de
	// V-Bucks (y, más atrás todavía, tras el de RemainingGifts), con un
	// "continue" que saltaba el bot ANTES de llegar a esa comprobación. Eso
	// dejó dos variantes del mismo bug real: un bot que sí era amigo del
	// cliente pero (a) se había quedado sin V-Bucks, o (b) ya había agotado
	// sus regalos del día, nunca sumaba a "es amigo" — si el resto de los
	// bots disponibles resultaban no ser amigos, se concluía que el
	// cliente "no agregó a ningún bot" y se cancelaba el pedido, aunque en
	// realidad sí lo había agregado (a uno que en ese momento no podía
	// enviar por otro motivo). Ahora la amistad es un hecho sobre la
	// relación bot↔cliente que se comprueba primero, independiente de
	// cuántos V-Bucks o cupos tenga el bot en este momento.
	friendBots := 0
	notFriendBots := 0
	friendCheckErrorBots := 0
	fundedBots := 0
	anyGiftLimit := false
	insufficientFundsBots := 0
	// noSlotsFriendBots cuenta bots CONFIRMADOS amigos que en este momento
	// no tienen cupo (RemainingGifts<=0, se resetea a diario) — necesario
	// para poder reportar "esperando que se liberen cupos" en vez de cruzar
	// ese caso con "sin fondos" o, peor, con "no es amigo de ningún bot".
	noSlotsFriendBots := 0

	// ── Loop interno: probar cada bot en orden ──
	for i := range accounts {
		bot := &accounts[i]

		// Verificar amistad con este bot — err != nil significa que no se
		// pudo verificar (nunca que se confirmó que no son amigos). Se hace
		// ANTES de cualquier chequeo de cupos/fondos a propósito (ver
		// comentario arriba): es un hecho sobre la relación bot↔cliente,
		// no sobre la disponibilidad del bot en este ciclo puntual.
		isFriend, friendSince, err := fortnite.CheckFriendship(database, *bot, receiverAccountID)
		switch {
		case err != nil:
			slog.Warn("Worker: no se pudo verificar la amistad con este bot, probando el siguiente",
				"bot", bot.DisplayName, "user", order.EpicUsername, "error", err)
			friendCheckErrorBots++
		case !isFriend:
			slog.Info("Worker: usuario confirmado que no es amigo del bot, probando siguiente",
				"bot", bot.DisplayName, "user", order.EpicUsername)
			notFriendBots++
		default:
			friendBots++
		}

		// Un bot sin cupo hoy no puede enviar nada este ciclo (se resetea
		// diariamente) — pero su amistad YA quedó contada arriba, así que
		// un bot amigo sin cupo nunca puede hacer que se concluya "no es
		// amigo de ningún bot".
		if bot.RemainingGifts <= 0 {
			if err == nil && isFriend {
				noSlotsFriendBots++
			}
			continue
		}

		// No intentar con un bot que no tiene suficientes V-Bucks reales para
		// pagar este item — sin esto, Epic rechazaría la compra y el pedido
		// se marcaría como fallido de inmediato en vez de probar el siguiente
		// bot que sí podría tener fondos. bot.VBucks viene del sync automático
		// contra la API real de Epic (ver healthcheck.go), así que es confiable.
		if bot.VBucks < order.PriceVBucks {
			slog.Info("Worker: bot sin V-Bucks suficientes, probando siguiente",
				"bot", bot.DisplayName, "tiene", bot.VBucks, "necesita", order.PriceVBucks)
			insufficientFundsBots++
			continue
		}
		fundedBots++

		if err != nil || !isFriend {
			continue // ya se contó arriba — este bot no puede recibir el envío ahora
		}

		// Verificar 48h de amistad
		hoursAsFriend := time.Since(friendSince).Hours()
		if hoursAsFriend < 48 {
			slog.Info("Worker: amistad reciente con este bot, probando siguiente",
				"bot", bot.DisplayName, "hours", hoursAsFriend)
			continue // probar siguiente bot
		}

		// Intentar enviar el regalo. Se marca "intento en curso" ANTES de
		// llamar a Epic (persistido en la base, no solo en memoria) — si el
		// proceso se cae justo durante esta llamada, la próxima vez que se
		// reclame este pedido (ClaimPendingOrders) sabremos que hubo un
		// intento real interrumpido, no solo que Epic dice "ya lo tiene" por
		// una compra o regalo de otra persona ajena a nosotros. Ver el
		// comentario completo en MarkOrderSendAttempted (db.go).
		wasAlreadyAttempting, attemptErr := db.MarkOrderSendAttempted(database, order.ID, true)
		if attemptErr != nil {
			slog.Error("Worker: no se pudo registrar el intento de envío, se reintenta en el próximo ciclo", "orderID", order.ID, "error", attemptErr)
			return
		}

		message := "¡Gracias por tu compra en KidStorePeru! 🎮"
		var evidence string
		evidence, err = fortnite.SendGift(database, *bot, receiverAccountID,
			order.ItemOfferID, order.PriceVBucks, order.ItemName, message)

		// IMPORTANTE: la marca "intento en curso" (send_attempted) NO se
		// limpia acá de forma incondicional — antes se limpiaba apenas
		// volvía SendGift, sin importar el resultado, lo que abría esta
		// secuencia: Epic entrega el ítem → se pierde la respuesta (fallo de
		// red) → se limpia la marca de todos modos → el siguiente intento
		// recibe ErrAlreadyOwned → como la marca ya estaba limpia, se
		// interpretaba como "lo tenía de antes" y se reembolsaba el KC pese
		// a que el regalo sí se había entregado. Ahora cada rama decide por
		// separado si es seguro limpiarla — solo cuando el resultado de ESTE
		// intento resuelve con certeza cualquier ambigüedad pendiente.

		if errors.Is(err, fortnite.ErrRequestUncertain) {
			// El request a Epic falló a nivel de transporte DESPUÉS de
			// mandarse (timeout, conexión cortada) — a diferencia de un
			// rechazo real de Epic, acá no hay ninguna respuesta que
			// confirme ni entrega ni rechazo. Un fallo de conexión no
			// demuestra que no hubo entrega: la marca se deja prendida tal
			// cual (ya quedó en true al principio de este intento), así que
			// si un intento posterior recibe ErrAlreadyOwned, se tratará
			// correctamente como entrega incierta y no como reembolso.
			// Tampoco se reintenta solo: un reintento podría duplicar el regalo
			// si el primero sí llegó. Revisión manual (ver sendInconclusive).
			slog.Warn("Worker: resultado incierto (fallo de transporte o respuesta sin confirmación)", "orderID", order.ID, "bot", bot.DisplayName, "error", err)
			sendInconclusive(database, order, fmt.Sprintf("Epic no dio una respuesta concluyente al envío (%v) — no se reenvía ni se reembolsa automáticamente; confirmar en Epic si el ítem llegó. bot=%s receiver=%s", err, bot.DisplayName, receiverAccountID))
			return
		}

		if errors.Is(err, fortnite.ErrAlreadyOwned) {
			needsReview := alreadyOwnedResolution(wasAlreadyAttempting) == "needs_review"
			reviewNote := fmt.Sprintf("Epic reportó que el receptor ya tiene el ítem, pero un intento de envío anterior de este mismo pedido se había interrumpido o perdido su respuesta — no se puede confirmar si fue ese intento el que lo entregó. bot=%s receiver=%s", bot.DisplayName, receiverAccountID)
			// SetOrderReviewOrClear aplica la decisión (needsReview, ya
			// tomada a partir de wasAlreadyAttempting) en UNA sola escritura
			// atómica — nunca en dos pasos separados (leer/limpiar la marca y
			// luego guardar 'review' aparte), para que un fallo o una caída
			// del proceso entre esos dos pasos no pueda perder la marca de
			// incertidumbre sin que 'review' quedara guardado.
			if setErr := db.SetOrderReviewOrClear(database, order.ID, needsReview, reviewNote); setErr != nil {
				slog.Error("Worker: no se pudo resolver la marca de intento de envío, se reintenta en el próximo ciclo", "orderID", order.ID, "error", setErr)
				return
			}
			if needsReview {
				// Había un intento de envío de ESTE pedido interrumpido antes de
				// esta llamada (el proceso se cayó a mitad de camino la vez
				// anterior, o el intento anterior fue un ErrRequestUncertain) —
				// es razonable pensar que ese intento sí llegó a entregar el
				// ítem, pero no hay ninguna evidencia real de Epic confirmando
				// ESE envío puntual, así que no se inventa una. Se deja en
				// revisión manual: ni se marca entregado sin pruebas, ni se
				// reembolsa a ciegas (podría estar duplicando un reembolso si
				// en realidad sí se entregó). No se vuelve a descontar
				// V-Bucks/slot del bot: si el intento original ya los descontó,
				// hacerlo de nuevo los dejaría mal contados.
				discordbot.AlertOrderNeedsReview(order.ID.String(), order.EpicUsername, order.ItemName)
				db.AddAuditLog(database, &order.CustomerID, "ORDER_NEEDS_REVIEW", reviewNote, "worker")
				slog.Warn("Worker: pedido en revisión manual — entrega incierta tras una caída", "orderID", order.ID, "bot", bot.DisplayName)
				return
			}
			// No había ningún intento previo sin resolver: esta es la única
			// llamada a Epic hecha hasta ahora para este pedido, y Epic
			// responde que el receptor ya tiene el ítem — sin ambigüedad
			// posible, lo obtuvo por su cuenta (lo compró él mismo, o se lo
			// regaló otra persona) ANTES de este pedido, nuestro pedido nunca
			// lo entregó. La marca ya quedó limpia (SetOrderReviewOrClear,
			// arriba) antes de reembolsar. No corresponde marcarlo "sent" solo
			// porque el cliente posee el artículo.
			refundMsg := fmt.Sprintf("el receptor '%s' ya posee este ítem (adquirido por su cuenta, no por este pedido) — Epic no permite regalarlo de nuevo", order.EpicUsername)
			// El motivo NO afirma que el reembolso ya se completó — failOrderAndRefund
			// decide eso según si RefundOrder realmente tuvo éxito, y el correo
			// (SendOrderFailedEmail) ya refleja el resultado real por su cuenta
			// (intro/hero distintos si refunded es true o false). Si acá también
			// se afirmara "se reembolsó tu KC" a ciegas, el correo podría decir
			// "reembolso en proceso" en la intro y "ya se reembolsó" en el motivo
			// al mismo tiempo.
			failOrderAndRefund(database, order, refundMsg, "El destinatario ya tiene este ítem en su cuenta de Fortnite (obtenido por su cuenta), por lo que Epic Games no permite regalarlo de nuevo.")
			return
		}

		if err == nil {
			// ── Éxito ──
			accountID := bot.ID
			// Guardamos la respuesta cruda de Epic como evidencia de entrega:
			// si algún día hay una disputa de pago/contracargo, esta es la
			// prueba de que el ítem sí se entregó a la cuenta correcta.
			// MarkOrderDelivered limpia send_attempted en la MISMA escritura
			// atómica que marca 'sent' — no queda ninguna ventana entre
			// "limpiar la marca" y "persistir la entrega" donde una caída del
			// proceso pudiera dejar el pedido en un estado contradictorio.
			// Si no se puede guardar, NUNCA se marca 'sent' sin evidencia: queda
			// pendiente y se reintenta sin reenviar (ver persistDelivery).
			persistDelivery(database, order, accountID, evidence)
			// Resta atómica en SQL, no un SET absoluto calculado en memoria —
			// evita que dos instancias del worker procesando la misma cuenta
			// bot casi al mismo tiempo pisen el contador real de regalos
			// (ver el comentario de DecrementRemainingGifts en db.go).
			if decErr := db.DecrementRemainingGifts(database, bot.ID); decErr != nil {
				slog.Warn("Worker: error descontando regalo restante del bot", "bot", bot.DisplayName, "error", decErr)
			}
			bot.RemainingGifts--

			if order.PriceVBucks > 0 {
				if deductErr := db.DeductBotVbucks(database, bot.ID, order.PriceVBucks); deductErr != nil {
					slog.Warn("Worker: error descontando pavos del bot", "bot", bot.DisplayName, "error", deductErr)
				} else {
					// Reflejar el descuento también en memoria (igual que ya se
					// hace con RemainingGifts) — accounts se reutiliza para el
					// resto de pedidos de este mismo ciclo de processOrders, así
					// que si no se actualiza acá, un segundo pedido caro de este
					// mismo ciclo podría creer que el bot todavía tiene fondos
					// que en la base de datos ya se gastaron.
					bot.VBucks -= order.PriceVBucks
					slog.Info("Worker: pavos descontados", "vbucks", order.PriceVBucks, "bot", bot.DisplayName)
				}
			}

			db.AddAuditLog(database, &order.CustomerID, "ORDER_SENT",
				fmt.Sprintf("pedido %s enviado por bot %s → %s", order.ID, bot.DisplayName, order.EpicUsername), "worker")

			if customer, custErr := db.GetCustomerByID(database, order.CustomerID); custErr == nil {
				if customer.Email != nil && *customer.Email != "" {
					itemImage := ""
					if order.ItemImage != nil { itemImage = *order.ItemImage }
					go SendOrderSentEmail(smtpConfig, *customer.Email, order.EpicUsername, order.ItemName, itemImage, order.ID.String(), order.PriceKC, "es")
				}
				discordbot.NotifyPurchase(customer, order.EpicUsername, order.ItemName, order.ItemImage, order.PriceKC, order.PriceVBucks)
			}

			slog.Info("Worker: pedido enviado", "orderID", order.ID, "bot", bot.DisplayName,
				"recipient", order.EpicUsername, "item", order.ItemName)
			return
		}

		// ── Error al enviar gift ──
		// A partir de acá, cualquier error es una respuesta REAL de Epic (no
		// un fallo de transporte — ErrRequestUncertain ya se descartó arriba,
		// y ErrAlreadyOwned ya tuvo su propio manejo) — se sabe con certeza
		// que ESTE intento en particular no entregó el ítem. Si no había
		// ningún intento previo sin resolver, es seguro limpiar la marca
		// ahora; si lo había, se deja prendida — el resultado de este bot
		// distinto no resuelve la ambigüedad de un intento anterior (podría
		// ser, por ejemplo, que este bot llegó a su límite diario mientras
		// OTRO bot, en un intento anterior interrumpido, sí llegó a entregar
		// el ítem).
		if !wasAlreadyAttempting {
			db.MarkOrderSendAttempted(database, order.ID, false)
		}

		errMsg := err.Error()
		errLower := strings.ToLower(errMsg)
		slog.Error("Worker: error enviando gift", "orderID", order.ID, "bot", bot.DisplayName, "msg", errMsg)

		// Token/auth → desactivar bot y probar el siguiente
		if strings.Contains(errLower, "token") || strings.Contains(errLower, "401") ||
			strings.Contains(errLower, "403") || strings.Contains(errLower, "unauthorized") ||
			strings.Contains(errLower, "deactivated") {
			slog.Warn("Worker: token invalido, marcando bot como inactivo", "bot", bot.DisplayName)
			db.DeactivateGameAccount(database, bot.ID)
			discordbot.AlertBotDeactivated(bot.ID, bot.DisplayName, "error de autenticación al intentar enviar un regalo: "+errMsg)
			bot.RemainingGifts = 0
			continue // probar siguiente bot
		}

		// Fondos insuficientes en Epic (aunque nuestro contador decía que sí
		// alcanzaba — ej. otro pedido gastó el saldo justo antes) → tratar
		// igual que "sin V-Bucks": no reintentar este bot, probar el
		// siguiente, y avisar para que se recargue pronto.
		if strings.Contains(errLower, "insufficient") || strings.Contains(errLower, "currency") {
			slog.Warn("Worker: fondos insuficientes en Epic pese al chequeo previo, probando siguiente bot",
				"bot", bot.DisplayName, "orderID", order.ID, "msg", errMsg)
			db.UpdateBotVbucks(database, bot.ID, 0)
			discordbot.CheckVBucksAlert(bot.ID, bot.DisplayName, 0)
			bot.VBucks = 0
			continue // probar siguiente bot
		}

		// Gift limit → marcar bot sin slots y probar el siguiente inmediatamente
		if strings.Contains(errLower, "gift_limit_reached") {
			slog.Warn("Worker: límite de gifts alcanzado, probando siguiente bot",
				"bot", bot.DisplayName, "orderID", order.ID)
			db.UpdateRemainingGifts(database, bot.ID, 0)
			discordbot.AlertNoGiftSlots(bot.ID, bot.DisplayName)
			bot.RemainingGifts = 0
			anyGiftLimit = true
			continue // probar siguiente bot
		}

		// Nota: los fallos de transporte (timeout, conexión cortada, etc.) ya
		// se manejan arriba mediante fortnite.ErrRequestUncertain — llegar
		// hasta acá significa que Epic sí respondió (con una razón real,
		// aunque no esté en ninguna de las categorías de arriba), así que es
		// seguro tratarlo como un rechazo definitivo.

		// Error permanente → fallar y reembolsar
		// Al cliente no se le manda el error técnico crudo de Epic, solo un
		// motivo genérico y entendible.
		failOrderAndRefund(database, order, errMsg, "Ocurrió un error técnico al procesar el envío.")
		return
	}

	// Todos los bots probados sin éxito — determinar resultado final. La
	// decisión en sí (decideFinalOrderOutcome) es una función pura para
	// poder cubrirla con pruebas de tabla simples: es justo la parte que
	// decide si se cancela el pedido o se reintenta, y un error ahí (como el
	// que motivó este arreglo) tiene consecuencias reales para el cliente.
	switch decideFinalOrderOutcome(anyGiftLimit, friendBots, notFriendBots, friendCheckErrorBots, fundedBots, insufficientFundsBots, noSlotsFriendBots, time.Since(order.CreatedAt)) {
	case outcomeGiftLimitPending:
		// Algún bot alcanzó el límite diario → mantener pending (se resetea al día siguiente)
		noSlotsMsg := "Todas las cuentas bot han agotado sus envíos del día. Los gifts se resetean diariamente."
		slog.Warn("Worker: todos los bots agotaron límite de gifts", "orderID", order.ID)
		db.UpdateOrderStatus(database, order.ID, "pending", nil, &noSlotsMsg)

	case outcomeFriendshipUnverifiedPending:
		// Al menos un bot activo no pudo verificar la amistad (y ninguno la
		// confirmó) — nunca se concluye "no es amigo de ningún bot" ni se le
		// pide al cliente que agregue al bot por algo que no se pudo
		// comprobar. Se deja pending, sin margen de gracia especial: el
		// próximo ciclo del worker (cada 30s) vuelve a intentar la
		// verificación real.
		pendingMsg := "No pudimos verificar tu amistad con nuestros bots por un problema temporal, reintentando automáticamente."
		slog.Warn("Worker: no se pudo verificar la amistad con ningún bot activo (fallos temporales), se reintenta",
			"orderID", order.ID, "bots_sin_verificar", friendCheckErrorBots, "bots_no_amigos_confirmados", notFriendBots)
		db.UpdateOrderStatus(database, order.ID, "pending", nil, &pendingMsg)

	case outcomeNotFriendGracePeriod:
		// El receptor no es amigo de ningún bot, pero el pedido todavía está
		// dentro del margen de gracia (ver friendGracePeriod) — tiempo de
		// sobra para que pase al menos un ciclo del aceptador automático de
		// solicitudes de amistad, incluso si el cliente todavía no había
		// agregado a nadie en el momento de comprar.
		pendingMsg := "Esperando a que agregues alguno de nuestros bots como amigo en Fortnite."
		slog.Info("Worker: usuario aún no es amigo de ningún bot, dentro del margen de gracia", "orderID", order.ID, "edad", time.Since(order.CreatedAt))
		db.UpdateOrderStatus(database, order.ID, "pending", nil, &pendingMsg)

	case outcomeNotFriendCancel:
		// El receptor no es amigo de ningún bot, CONFIRMADO por Epic para
		// cada uno (sin errores de verificación) y ya fuera del margen de
		// gracia — recién acá se cancela y reembolsa.
		errMsg := fmt.Sprintf("el usuario '%s' no está en la lista de amigos de ningún bot disponible", order.EpicUsername)
		slog.Error("Worker: usuario no es amigo de ningún bot tras el margen de gracia", "orderID", order.ID)
		failOrderAndRefund(database, order, errMsg, "Tu cuenta de Epic Games no es amiga de ninguno de nuestros bots todavía. Agrega alguno desde la página de Bots y vuelve a intentar tu compra.")

	case outcomeNoSlotsPending:
		// Hay al menos un bot CONFIRMADO amigo del cliente, pero todos los
		// bots amigos agotaron sus envíos de hoy — el motivo real es "sin
		// cupos", nunca "no agregaste a ningún bot" (eso sería falso: el
		// cliente sí agregó a uno, solo que ahora mismo no tiene cupo) ni
		// "sin fondos" (podría tener saldo de sobra). Se resetea a diario,
		// así que se deja pending sin margen de gracia especial.
		noSlotsMsg := "El bot amigo de tu cuenta ya agotó sus envíos de hoy. Los gifts se resetean diariamente, tu pedido se reintentará automáticamente."
		slog.Warn("Worker: el/los bot(s) amigo(s) del cliente están sin cupo hoy, se reintenta", "orderID", order.ID)
		db.UpdateOrderStatus(database, order.ID, "pending", nil, &noSlotsMsg)

	case outcomeNoFundsPending:
		// Ningún bot con slots tenía V-Bucks suficientes para este pedido en
		// particular — no es que no haya bots, es que ninguno tiene fondos
		// para ESTE monto. Se avisa (con el mismo cooldown que "sin bots
		// disponibles", para no saturar si hay varios pedidos caros en cola)
		// y se mantiene "pending" para reintentar en cuanto se recarguen.
		noFundsMsg := "Ningún bot tiene V-Bucks suficientes para este pedido en este momento."
		slog.Warn("Worker: ningún bot con fondos suficientes", "orderID", order.ID, "necesita", order.PriceVBucks)
		discordbot.AlertNoActiveBots(1)
		db.UpdateOrderStatus(database, order.ID, "pending", nil, &noFundsMsg)

	default: // outcomeGenericPending (ej: amistad reciente en todos los bots)
		slog.Warn("Worker: ningún bot pudo enviar el regalo en este ciclo, reintentando", "orderID", order.ID)
		db.UpdateOrderStatus(database, order.ID, "pending", nil, nil)
	}
}

// orderFinalOutcome resume qué hacer con un pedido cuando ningún bot pudo
// enviar el regalo en este ciclo.
type orderFinalOutcome int

const (
	outcomeGiftLimitPending orderFinalOutcome = iota
	outcomeFriendshipUnverifiedPending
	outcomeNotFriendGracePeriod
	outcomeNotFriendCancel
	outcomeNoSlotsPending
	outcomeNoFundsPending
	outcomeGenericPending
)

// decideFinalOrderOutcome es la parte pura (sin red ni base de datos) de la
// decisión de processOrder cuando todos los bots fallaron — separada
// justamente para poder probar con una tabla simple el punto 3 del pedido
// de correcciones: friendCheckErrorBots (bots cuya amistad NO se pudo
// verificar) nunca debe poder producir outcomeNotFriendCancel, ni siquiera
// mezclado con bots que sí confirmaron que no son amigos, y un bot amigo
// sin fondos O sin cupos (friendBots>0) tampoco — solo cuando NINGÚN bot es
// amigo confirmado (friendBots==0) Y no quedó ninguno sin verificar
// (friendCheckErrorBots==0) se considera esa vía. noSlotsFriendBots (bots
// CONFIRMADOS amigos que agotaron sus envíos de hoy) se revisa ANTES que
// "sin fondos": si todos los bots amigos confirmados están sin cupo, el
// motivo real es "sin cupos", no "sin fondos" (un bot amigo sin cupo podría
// tener saldo de sobra) ni "no es amigo de ningún bot".
func decideFinalOrderOutcome(anyGiftLimit bool, friendBots, notFriendBots, friendCheckErrorBots, fundedBots, insufficientFundsBots, noSlotsFriendBots int, orderAge time.Duration) orderFinalOutcome {
	switch {
	case anyGiftLimit:
		return outcomeGiftLimitPending
	case friendBots == 0 && friendCheckErrorBots > 0:
		return outcomeFriendshipUnverifiedPending
	case friendBots == 0 && notFriendBots > 0:
		if orderAge < friendGracePeriod {
			return outcomeNotFriendGracePeriod
		}
		return outcomeNotFriendCancel
	case friendBots > 0 && friendBots == noSlotsFriendBots:
		return outcomeNoSlotsPending
	case fundedBots == 0 && insufficientFundsBots > 0:
		return outcomeNoFundsPending
	default:
		return outcomeGenericPending
	}
}
