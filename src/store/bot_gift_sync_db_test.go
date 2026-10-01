package store

import (
	"testing"
	"time"

	"KidStoreStore/src/db"
	"KidStoreStore/src/types"

	"github.com/google/uuid"
)

// La sincronización de regalos disponibles con Epic (healthcheck) solo
// escribe si remaining_gifts sigue valiendo lo que se leyó antes de
// consultar a Epic — así nunca pisa un descuento que el worker de pedidos
// hizo mientras tanto.
func TestSyncRemainingGifts_NoPisaUnDescuentoConcurrente(t *testing.T) {
	conn := setupShopTestDB(t)
	bot := types.GameAccount{
		ID: uuid.New(), DisplayName: "bot_sync_test", RemainingGifts: 5, VBucks: 1000,
		AccessToken: "t", AccessTokenExpDate: time.Now().Add(time.Hour),
	}
	insertBotAccount(t, conn, bot)

	// Epic dice que le quedan 2 (se enviaron 3 regalos desde otro lado).
	updated, err := db.SyncRemainingGifts(conn, bot.ID, 5, 2)
	if err != nil || !updated {
		t.Fatalf("SyncRemainingGifts = %v, %v; se esperaba que actualice", updated, err)
	}
	if got, _ := botAccountState(t, conn, bot.ID); got != 2 {
		t.Fatalf("remaining_gifts = %d, se esperaba 2", got)
	}

	// El worker envía un regalo (2 → 1) mientras el healthcheck todavía
	// tenía leído el valor viejo (2) y quiere escribir 3.
	if err := db.DecrementRemainingGifts(conn, bot.ID); err != nil {
		t.Fatal(err)
	}
	updated, err = db.SyncRemainingGifts(conn, bot.ID, 2, 3)
	if err != nil || updated {
		t.Fatalf("SyncRemainingGifts con un valor desactualizado = %v, %v; no debería escribir", updated, err)
	}
	if got, _ := botAccountState(t, conn, bot.ID); got != 1 {
		t.Errorf("remaining_gifts = %d, se esperaba 1 (el descuento del worker se respeta)", got)
	}
}
