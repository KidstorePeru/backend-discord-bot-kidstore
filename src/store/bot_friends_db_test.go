package store

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"KidStoreStore/src/crypto"
	"KidStoreStore/src/db"
	"KidStoreStore/src/types"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// La página de Bots muestra cuántos amigos tiene cada cuenta (límite 1000)
// para que los clientes no le manden solicitud a una cuenta llena.
func TestBots_CantidadDeAmigos(t *testing.T) {
	conn := setupShopTestDB(t)
	key := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	prevKey := encryptionKey
	encryptionKey = key
	t.Cleanup(func() { encryptionKey = prevKey })

	enc, err := crypto.Encrypt("token", key)
	if err != nil {
		t.Fatal(err)
	}
	bot := types.GameAccount{ID: uuid.New(), DisplayName: "bot_amigos_test", RemainingGifts: 5,
		AccessToken: enc, AccessTokenExpDate: time.Now().Add(time.Hour)}
	insertBotAccount(t, conn, bot)
	conn.Exec(`UPDATE game_accounts SET refresh_token=$2 WHERE id=$1`, bot.ID, enc)

	friendsOf := func() *int {
		t.Helper()
		accounts, err := db.GetAllGameAccounts(conn, key)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range accounts {
			if a.ID == bot.ID {
				return a.FriendsCount
			}
		}
		t.Fatal("no se encontró la cuenta de prueba")
		return nil
	}

	// Recién vinculada: todavía no se sabe (null), y sumar no inventa un número.
	if got := friendsOf(); got != nil {
		t.Fatalf("una cuenta sin sincronizar no debería tener cantidad, tiene %d", *got)
	}
	db.AddBotFriendsCount(conn, bot.ID, 1)
	if got := friendsOf(); got != nil {
		t.Fatalf("sumar sin conocer la cantidad no debería inventar un número: %d", *got)
	}

	db.SetBotFriendsCount(conn, bot.ID, 998)
	db.AddBotFriendsCount(conn, bot.ID, 1)
	if got := friendsOf(); got == nil || *got != 999 {
		t.Fatalf("amigos = %v, se esperaba 999", got)
	}
	db.AddBotFriendsCount(conn, bot.ID, 5)
	if got := friendsOf(); got == nil || *got != db.BotFriendsLimit {
		t.Fatalf("nunca debe pasar del límite: %v", got)
	}

	// La ruta pública lo muestra con el límite.
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/store/bots-status", HandlerBotsStatus(conn))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/store/bots-status", nil))
	var res struct {
		Accounts []struct {
			ID           string `json:"id"`
			FriendsCount *int   `json:"friends_count"`
			FriendsLimit int    `json:"friends_limit"`
		} `json:"accounts"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	found := false
	for _, a := range res.Accounts {
		if a.ID == bot.ID.String() {
			found = true
			if a.FriendsCount == nil || *a.FriendsCount != 1000 || a.FriendsLimit != 1000 {
				t.Errorf("bots-status = %+v", a)
			}
		}
	}
	if !found {
		t.Errorf("la cuenta no aparece en bots-status: %s", w.Body.String())
	}
}
