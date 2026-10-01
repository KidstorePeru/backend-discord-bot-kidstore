package store

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"KidStoreStore/src/types"
)

func withFakeCosmetics(t *testing.T, handler http.HandlerFunc) *int32 {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		handler(w, r)
	}))
	prev := cosmeticsSearchURL
	cosmeticsSearchURL = srv.URL
	searchCacheMu.Lock()
	searchCache = map[string]searchCacheEntry{}
	searchCacheMu.Unlock()
	t.Cleanup(func() { srv.Close(); cosmeticsSearchURL = prev })
	return &calls
}

func TestBuscadorDeCosmeticos_SoloLoQueEstuvoEnLaTienda(t *testing.T) {
	var gotQuery string
	calls := withFakeCosmetics(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		io.WriteString(w, `{"status":200,"data":[
			{"id":"CID_Pase","name":"Jonesy del pase","type":{"displayValue":"Atuendo"},"images":{"smallIcon":"https://x/a.png"},"shopHistory":[]},
			{"id":"CID_Viejo","name":"Jonesy viejo","type":{"displayValue":"Atuendo"},"images":{"smallIcon":"https://x/b.png"},"shopHistory":["2020-03-29T00:00:00Z"]},
			{"id":"CID_Nuevo","name":"Jonesy nuevo","type":{"displayValue":"Atuendo"},"images":{"icon":"https://x/c.png"},"shopHistory":["2025-01-01T00:00:00Z","2026-09-13T00:00:00Z"]}
		]}`)
	})
	results, err := searchCosmetics(context.Background(), "jonesy", "es")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].ItemID != "CID_Nuevo" || results[1].ItemID != "CID_Viejo" {
		t.Fatalf("resultados = %+v (sin el del pase, el más reciente primero)", results)
	}
	if results[0].Image != "https://x/c.png" || results[0].LastSeen != "2026-09-13T00:00:00Z" {
		t.Errorf("resultado = %+v", results[0])
	}
	if !strings.Contains(gotQuery, "responseFlags=4") || !strings.Contains(gotQuery, "language=es-419") {
		t.Errorf("consulta = %s", gotQuery)
	}
	// La misma búsqueda sale de la caché.
	searchCosmetics(context.Background(), "JONESY", "es")
	if atomic.LoadInt32(calls) != 1 {
		t.Errorf("la búsqueda repetida no debería volver a consultar, hubo %d", *calls)
	}
}

func TestBuscadorDeCosmeticos_SinResultadosYErrores(t *testing.T) {
	status := http.StatusNotFound
	withFakeCosmetics(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
	if results, err := searchCosmetics(context.Background(), "zzzz", "en"); err != nil || len(results) != 0 {
		t.Errorf("sin coincidencias: %v %v", results, err)
	}
	status = http.StatusServiceUnavailable
	if _, err := searchCosmetics(context.Background(), "otra", "en"); err == nil {
		t.Error("un 503 debería ser un error")
	}
}

func TestCorreoListaDeDeseos_EscapaYMuestraLosObjetos(t *testing.T) {
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	prev := resendAPIURL
	resendAPIURL = srv.URL
	defer func() { resendAPIURL = prev }()

	cfg := types.EnvConfig{ResendAPIKey: "re_test"}
	SendWishlistBackEmail(cfg, "cliente@example.com", []WishlistBackItem{
		{Name: `<script>x</script>`, Image: "https://x/a.png", PriceKC: 1200, OutDate: time.Date(2026, 10, 4, 23, 59, 59, 0, time.UTC)},
		{Name: "Pico", PriceKC: 800},
	}, "es")
	if strings.Contains(sent, "<script>x") {
		t.Error("el nombre debe ir escapado")
	}
	for _, want := range []string{"1200 KC", "800 KC", "hasta el 4 oct.", "Volvieron 2 objetos", "/notifications"} {
		if !strings.Contains(sent, want) {
			t.Errorf("el correo no contiene %q", want)
		}
	}
}
