package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Tras un deploy, el contenedor nuevo arranca sin memoria ni disco: si
// fortnite-api.com está caído en ese momento, la tienda tiene que salir del
// respaldo guardado en la base de datos en vez de mostrar un error.
func TestTienda_TrasUnDeployConElProveedorCaidoSaleDeLaBaseDeDatos(t *testing.T) {
	conn := setupShopTestDB(t)
	f := withFakeShop(t)
	conn.Exec(`DELETE FROM shop_cache`)
	SetShopCacheDB(conn)
	shopCacheMu.Lock()
	shopDBSavedHash = map[string][32]byte{}
	shopCacheMu.Unlock()
	t.Cleanup(func() {
		SetShopCacheDB(nil)
		conn.Exec(`DELETE FROM shop_cache`)
		shopCacheMu.Lock()
		shopDBSavedHash = map[string][32]byte{}
		shopCacheMu.Unlock()
	})

	// Funcionamiento normal: la tienda buena queda guardada en la base.
	if _, err := fetchShopBody(context.Background(), "es-419"); err != nil {
		t.Fatal(err)
	}
	var savedAt time.Time
	if err := conn.QueryRow(`SELECT fetched_at FROM shop_cache WHERE lang = 'es-419'`).Scan(&savedAt); err != nil {
		t.Fatalf("la tienda no se guardó en la base: %v", err)
	}

	// El mismo catálogo otra vez no se reescribe (solo se escribe si cambia).
	expireShopCache()
	if _, err := fetchShopBody(context.Background(), "es-419"); err != nil {
		t.Fatal(err)
	}
	var again time.Time
	conn.QueryRow(`SELECT fetched_at FROM shop_cache WHERE lang = 'es-419'`).Scan(&again)
	if !again.Equal(savedAt) {
		t.Error("un catálogo idéntico no debería volver a escribirse en la base")
	}

	// Deploy: memoria y disco vacíos, y el proveedor caído.
	forgetShopMemory()
	shopDiskCacheDir = t.TempDir()
	shopCacheMu.Lock()
	shopLastFailure = map[string]time.Time{}
	shopCacheMu.Unlock()
	f.failing.Store(true)

	body, err := fetchShopBody(context.Background(), "es-419")
	if err != nil {
		t.Fatalf("con un respaldo en la base NO debe devolver error: %v", err)
	}
	if !isStale(t, body) || !strings.Contains(string(body), "Objeto real") {
		t.Errorf("debería servir la última tienda buena marcada _stale, obtuve %s", body)
	}

	// Las visitas siguientes salen de memoria, sin volver a leer la base.
	conn.Exec(`DELETE FROM shop_cache`)
	body, err = fetchShopBody(context.Background(), "es-419")
	if err != nil || !isStale(t, body) {
		t.Errorf("la segunda visita debería salir de memoria: err=%v", err)
	}
	// La verificación de precios al crear un pedido también funciona.
	if item, err := resolveShopItem(context.Background(), "v2:/offer-1"); err != nil || item.FinalPrice != 800 {
		t.Errorf("resolveShopItem: item=%+v err=%v", item, err)
	}
}

func TestTienda_RespaldoDanadoEnLaBaseSeIgnora(t *testing.T) {
	conn := setupShopTestDB(t)
	f := withFakeShop(t)
	SetShopCacheDB(conn)
	t.Cleanup(func() {
		SetShopCacheDB(nil)
		conn.Exec(`DELETE FROM shop_cache`)
	})
	conn.Exec(`DELETE FROM shop_cache`)
	if _, err := conn.Exec(`INSERT INTO shop_cache (lang, body) VALUES ('en', '{"status":200,"data":null}')`); err != nil {
		t.Fatal(err)
	}
	f.failing.Store(true)
	if _, err := fetchShopBody(context.Background(), "en"); err == nil {
		t.Error("un respaldo inválido no debería servirse como tienda")
	}
}
