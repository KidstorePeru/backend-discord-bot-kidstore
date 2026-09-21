package fortnite

// Pruebas de regresión para SendGift contra un servidor simulado
// (httptest.Server) — nunca contra la API real de Epic Games, y sin tocar
// ninguna base de datos (mcpGiftCatalogBaseURL se redirige al servidor de
// prueba, y ninguno de los escenarios cubiertos aquí llega a las ramas que
// necesitan *sql.DB, así que se pasa nil a propósito). Cubren el punto 2
// del pedido de correcciones: distinguir una entrega confirmada, un
// rechazo definitivo y verificable, y un resultado incierto (502/504 de un
// intermediario, o una respuesta cuya lectura se corta a mitad de camino).

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"KidStoreStore/src/types"

	"github.com/google/uuid"
)

// newTestAccount arma una cuenta bot mínima cuyo token NUNCA está por
// vencer (así executeWithRefresh no intenta refrescarlo, lo que evitaría
// tocar la base de datos) — todos los escenarios de esta prueba dependen
// de que el único request real sea el de GiftCatalogEntry contra el
// servidor simulado.
func newTestAccount() types.GameAccount {
	return types.GameAccount{
		ID:                 uuid.New(),
		DisplayName:        "bot_test",
		AccessToken:        "test-token",
		AccessTokenExpDate: time.Now().Add(24 * time.Hour),
	}
}

func withMockEpic(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	prev := mcpGiftCatalogBaseURL
	mcpGiftCatalogBaseURL = server.URL
	t.Cleanup(func() { mcpGiftCatalogBaseURL = prev })
}

func TestSendGift_EntregaConfirmada(t *testing.T) {
	withMockEpic(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"profileRevision": 42, "profileId": "common_core"}`))
	})

	evidence, err := SendGift(nil, newTestAccount(), uuid.New().String(), "offer-1", 500, "Item", "hola")
	if err != nil {
		t.Fatalf("una respuesta 200 con cuerpo completo debería ser una entrega confirmada, obtuve error: %v", err)
	}
	if evidence == "" {
		t.Error("una entrega confirmada debería dejar evidencia (la respuesta cruda de Epic)")
	}
}

func TestSendGift_RechazoDefinitivoVerificable(t *testing.T) {
	withMockEpic(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"errorCode":"errors.com.epicgames.modules.gamesubcatalog.receiver_will_own_more_than_one"}`))
	})

	_, err := SendGift(nil, newTestAccount(), uuid.New().String(), "offer-1", 500, "Item", "hola")
	if !errors.Is(err, ErrAlreadyOwned) {
		t.Errorf("un errorCode reconocible de Epic (receiver_will_own_more_than_one) debería mapear a ErrAlreadyOwned, obtuve: %v", err)
	}
	if errors.Is(err, ErrRequestUncertain) {
		t.Error("un rechazo verificable de Epic (con errorCode reconocido) NUNCA debe tratarse como incierto")
	}
}

func TestSendGift_RechazoDefinitivoSinCodigoReconocido(t *testing.T) {
	withMockEpic(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"errorCode":"errors.com.epicgames.algo.que.no.reconocemos"}`))
	})

	_, err := SendGift(nil, newTestAccount(), uuid.New().String(), "offer-1", 500, "Item", "hola")
	if err == nil {
		t.Fatal("se esperaba un error")
	}
	if errors.Is(err, ErrRequestUncertain) || errors.Is(err, ErrAlreadyOwned) {
		t.Errorf("un 400 con un errorCode real de Epic (aunque no esté en nuestra lista) es un rechazo verificable, no incierto: %v", err)
	}
}

func TestSendGift_502EsIncierto(t *testing.T) {
	withMockEpic(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`<html><body>502 Bad Gateway</body></html>`))
	})

	_, err := SendGift(nil, newTestAccount(), uuid.New().String(), "offer-1", 500, "Item", "hola")
	if !errors.Is(err, ErrRequestUncertain) {
		t.Errorf("un 502 (respuesta de un intermediario, no de Epic) debe tratarse como resultado incierto, obtuve: %v", err)
	}
}

func TestSendGift_504EsIncierto(t *testing.T) {
	withMockEpic(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
		w.Write([]byte(`upstream request timeout`))
	})

	_, err := SendGift(nil, newTestAccount(), uuid.New().String(), "offer-1", 500, "Item", "hola")
	if !errors.Is(err, ErrRequestUncertain) {
		t.Errorf("un 504 (respuesta de un intermediario, no de Epic) debe tratarse como resultado incierto — Epic pudo haber procesado el regalo antes de que el proxy cortara la respuesta. Obtuve: %v", err)
	}
}

func TestSendGift_500SinErrorCodeEsIncierto(t *testing.T) {
	withMockEpic(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`internal server error`))
	})

	_, err := SendGift(nil, newTestAccount(), uuid.New().String(), "offer-1", 500, "Item", "hola")
	if !errors.Is(err, ErrRequestUncertain) {
		t.Errorf("un 5xx sin errorCode reconocible de Epic debe tratarse como incierto, no como rechazo confirmado. Obtuve: %v", err)
	}
}

// TestSendGift_RespuestaPerdidaEsIncierta cubre "regalo ejecutado con
// respuesta perdida": la conexión se corta a mitad de un cuerpo que
// prometía más bytes de los que realmente llegan (vía http.Hijacker, para
// forzar un corte real a nivel de conexión TCP en vez de depender de que
// el cliente de Go detecte un Content-Length incompleto en un cierre
// "prolijo") — exactamente lo que pasaría si Epic empezó a responder pero
// la conexión se cortó antes de que la respuesta completa llegara. No hay
// forma de confirmar ni una entrega ni un rechazo con un cuerpo a medias.
func TestSendGift_RespuestaPerdidaEsIncierta(t *testing.T) {
	withMockEpic(t, func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		bufrw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\nContent-Type: application/json\r\n\r\n")
		bufrw.WriteString(`{"partial`)
		bufrw.Flush()
		// conn.Close() (deferred) corta la conexión ACÁ, a mitad del cuerpo
		// de 1000 bytes que el encabezado prometía.
	})

	_, err := SendGift(nil, newTestAccount(), uuid.New().String(), "offer-1", 500, "Item", "hola")
	if !errors.Is(err, ErrRequestUncertain) {
		t.Errorf("una respuesta cuya lectura se interrumpe a mitad de camino debe tratarse como resultado incierto, obtuve: %v", err)
	}
}

// TestSendGift_ConexionRechazadaEsIncierta cubre el fallo de transporte
// clásico (nada responde del todo) — apunta a un puerto donde no hay
// ningún servidor escuchando.
func TestSendGift_ConexionRechazadaEsIncierta(t *testing.T) {
	prev := mcpGiftCatalogBaseURL
	mcpGiftCatalogBaseURL = "http://127.0.0.1:1" // puerto reservado, nadie escucha ahí
	defer func() { mcpGiftCatalogBaseURL = prev }()

	_, err := SendGift(nil, newTestAccount(), uuid.New().String(), "offer-1", 500, "Item", "hola")
	if !errors.Is(err, ErrRequestUncertain) {
		t.Errorf("un fallo de conexión (nadie responde) debe tratarse como resultado incierto, obtuve: %v", err)
	}
}

// Pruebas de regresión para GetReceiverAccountID — cubren el punto 3 del
// pedido de correcciones: un 404 real de Epic es la ÚNICA señal válida de
// que el usuario no existe. Antes, CUALQUIER fallo (token del bot
// rechazado, límite de solicitudes, el servicio de Epic caído, o un fallo
// de red) se reportaba exactamente igual que un 404, y el worker cancelaba
// el pedido diciéndole al cliente que su usuario podía estar mal escrito
// por un problema que en realidad era nuestro o de Epic.

func withMockEpicAccount(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	prev := EpicAccountBaseURL
	EpicAccountBaseURL = server.URL
	t.Cleanup(func() { EpicAccountBaseURL = prev })
}

func TestGetReceiverAccountID_404EsConfirmado(t *testing.T) {
	withMockEpicAccount(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"errorCode":"errors.com.epicgames.account.account_not_found"}`))
	})

	_, err := GetReceiverAccountID(nil, newTestAccount(), "usuario_inexistente")
	if !errors.Is(err, ErrEpicUserNotFound) {
		t.Errorf("un 404 real de Epic debe mapear a ErrEpicUserNotFound, obtuve: %v", err)
	}
}

func TestGetReceiverAccountID_200Encuentra(t *testing.T) {
	withMockEpicAccount(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"abc123","displayName":"alguien"}`))
	})

	id, err := GetReceiverAccountID(nil, newTestAccount(), "alguien")
	if err != nil {
		t.Fatalf("una respuesta 200 no debería producir error, obtuve: %v", err)
	}
	if id != "abc123" {
		t.Errorf("GetReceiverAccountID = %q, want %q", id, "abc123")
	}
}

// Nota: un 401/403 del bot dispara, dentro de executeWithRefresh, el flujo
// normal de auto-refresco de token (ya cubierto por sus propias pruebas) —
// acá se prueban códigos que NO disparan ese flujo, para aislar
// específicamente la clasificación de GetReceiverAccountID.

func TestGetReceiverAccountID_429NuncaEsNotFound(t *testing.T) {
	withMockEpicAccount(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := GetReceiverAccountID(nil, newTestAccount(), "alguien")
	if err == nil {
		t.Fatal("se esperaba un error")
	}
	if errors.Is(err, ErrEpicUserNotFound) {
		t.Error("un 429 (límite de solicitudes) NUNCA debe reportarse como 'usuario no encontrado'")
	}
}

func TestGetReceiverAccountID_503NuncaEsNotFound(t *testing.T) {
	withMockEpicAccount(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	_, err := GetReceiverAccountID(nil, newTestAccount(), "alguien")
	if err == nil {
		t.Fatal("se esperaba un error")
	}
	if errors.Is(err, ErrEpicUserNotFound) {
		t.Error("un 503 (servicio de Epic caído) NUNCA debe reportarse como 'usuario no encontrado'")
	}
}

func TestGetReceiverAccountID_FalloDeConexionNuncaEsNotFound(t *testing.T) {
	prev := EpicAccountBaseURL
	EpicAccountBaseURL = "http://127.0.0.1:1" // puerto reservado, nadie escucha ahí
	defer func() { EpicAccountBaseURL = prev }()

	_, err := GetReceiverAccountID(nil, newTestAccount(), "alguien")
	if err == nil {
		t.Fatal("se esperaba un error")
	}
	if errors.Is(err, ErrEpicUserNotFound) {
		t.Error("un fallo de conexión NUNCA debe reportarse como 'usuario no encontrado'")
	}
}
