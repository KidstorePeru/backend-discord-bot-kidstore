package store

// Pruebas de integración del RECORRIDO COMPLETO de una compra: pago
// aprobado → acreditación de KC → creación del pedido (gastando ese KC) →
// el worker reclama el pedido → selección de bot → entrega (o rechazo
// definitivo → reembolso). Hasta ahora, la acreditación de pagos y el
// cumplimiento de pedidos se probaban por separado (ver
// TestProcessNOWPaymentsPaymentID_* para lo primero, TestProcessOrder_* /
// TestFailOrderAndRefund_* para lo segundo) — acá se encadenan usando las
// mismas funciones REALES que usa la aplicación en cada paso
// (processApprovedPayment, db.DeductKCAndCreateOrder, db.ClaimPendingOrders,
// processOrder), nunca una reimplementación paralela de esa lógica. Usa la
// misma base de datos de PRUEBA que el resto de este archivo
// (setupShopTestDB) y un servidor Epic simulado (httptest) — nunca la API
// real de Epic, nunca un cobro/regalo/correo real.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"KidStoreStore/src/db"
	"KidStoreStore/src/fortnite"
	"KidStoreStore/src/types"

	"github.com/google/uuid"
)

// epicMockServer arma un httptest.Server que responde a los tres endpoints
// de Epic que toca processOrder (cuenta del receptor, amistad, y el envío
// del regalo en sí) — giftOutcome decide qué responde el tercero.
func epicMockServer(t *testing.T, receiverAccountID, friendBotIDClean string, giftOutcome func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/account/api/public/account/displayName/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"id": receiverAccountID, "displayName": "receiver_test"})
	})
	mux.HandleFunc("/friends/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, friendBotIDClean) {
			json.NewEncoder(w).Encode(map[string]string{
				"accountId": receiverAccountID,
				// Bien por encima de las 48h que exige processOrder.
				"created": time.Now().Add(-60 * time.Hour).Format(time.RFC3339),
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/fortnite/api/game/v2/profile/", giftOutcome)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// withMockedEpicURLs redirige las tres variables de entorno de prueba del
// paquete fortnite al servidor simulado, y las restaura al terminar.
func withMockedEpicURLs(t *testing.T, url string) {
	t.Helper()
	prevAccount, prevFriends, prevGift := fortnite.EpicAccountBaseURL, fortnite.EpicFriendsBaseURL, fortnite.McpGiftCatalogBaseURL
	fortnite.EpicAccountBaseURL = url
	fortnite.EpicFriendsBaseURL = url
	fortnite.McpGiftCatalogBaseURL = url
	t.Cleanup(func() {
		fortnite.EpicAccountBaseURL = prevAccount
		fortnite.EpicFriendsBaseURL = prevFriends
		fortnite.McpGiftCatalogBaseURL = prevGift
	})
}

// creditApprovedPayment inserta una transacción de pago ya en 'pending' y la
// acredita con processApprovedPayment — el mismo camino que toma un webhook
// real de cualquier pasarela una vez que YA verificó el pago (esa
// verificación es aparte, ver TestPayPalOrderMatchesTx /
// TestVerifyNOWPaymentsSignature — acá se empieza justo donde ese trabajo
// termina).
func creditApprovedPayment(t *testing.T, conn *sql.DB, custID uuid.UUID, kcAmount int) uuid.UUID {
	t.Helper()
	txID := uuid.New()
	if _, err := conn.Exec(`
		INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, created_at, updated_at)
		VALUES ($1,$2,'mercadopago','kc_recharge','starter','Starter',10.40,2.80,$3,$4,'pending',NOW(),NOW())`,
		txID, custID, kcAmount, "ext-"+txID.String()); err != nil {
		t.Fatalf("insert payment_transactions: %v", err)
	}
	t.Cleanup(func() { conn.Exec(`DELETE FROM payment_transactions WHERE id=$1`, txID) })
	if err := processApprovedPayment(conn, txID); err != nil {
		t.Fatalf("processApprovedPayment: %v", err)
	}
	return txID
}

// buyItemWithKC ejercita la MISMA transacción atómica que usa
// HandlerCreateOrder (db.DeductKCAndCreateOrder) para descontar KC y crear
// el pedido — se usa directamente en vez de pasar por el handler HTTP
// porque este último primero re-valida el precio contra la tienda real de
// Epic (fortnite-api.com, un tercer servicio externo, sin seam de prueba
// hoy); la lógica de dinero que de verdad importa acá (descuento atómico +
// creación del pedido) es exactamente la misma de cualquier forma.
func buyItemWithKC(t *testing.T, conn *sql.DB, custID uuid.UUID, priceKC int) types.Order {
	t.Helper()
	order, err := db.DeductKCAndCreateOrder(conn, custID, "receiver_test", types.CreateOrderRequest{
		ItemOfferID: "offer-full-flow-" + uuid.NewString(),
		ItemName:    "Item de prueba (recorrido completo)",
		PriceKC:     priceKC,
		PriceVBucks: priceKC,
	}, 10)
	if err != nil {
		t.Fatalf("DeductKCAndCreateOrder: %v", err)
	}
	t.Cleanup(func() { conn.Exec(`DELETE FROM orders WHERE id=$1`, order.ID) })
	return order
}

// claimOrder ejercita db.ClaimPendingOrders (el mismo mecanismo FOR UPDATE
// SKIP LOCKED que usa StartOrderWorker cada 30s) y devuelve el pedido
// reclamado que coincide con orderID.
func claimOrder(t *testing.T, conn *sql.DB, orderID uuid.UUID) types.Order {
	t.Helper()
	claimed, err := db.ClaimPendingOrders(conn)
	if err != nil {
		t.Fatalf("ClaimPendingOrders: %v", err)
	}
	for _, o := range claimed {
		if o.ID == orderID {
			var status string
			conn.QueryRow(`SELECT status FROM orders WHERE id=$1`, orderID).Scan(&status)
			if status != "processing" {
				t.Fatalf("el pedido reclamado debería quedar 'processing', obtuve %q", status)
			}
			return o
		}
	}
	t.Fatalf("ClaimPendingOrders no reclamó el pedido %s recién creado", orderID)
	return types.Order{}
}

func kcBalance(t *testing.T, conn *sql.DB, custID uuid.UUID) int {
	t.Helper()
	var balance int
	if err := conn.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1`, custID).Scan(&balance); err != nil {
		t.Fatalf("no se pudo leer kc_balance: %v", err)
	}
	return balance
}

// insertBotAccount persiste el bot en game_accounts (no solo en memoria) —
// necesario porque db.DecrementRemainingGifts / db.DeductBotVbucks (llamadas
// por processOrder en su rama de éxito) son UPDATEs directos contra esa
// tabla por id: sin esta fila, esos UPDATE afectan cero filas y el
// descuento real nunca ocurre, aunque el struct en memoria que se le pasa a
// processOrder sí muestre RemainingGifts/VBucks actualizados (ver el
// bot.RemainingGifts-- y bot.VBucks-= en shop.go, que son aparte de la
// escritura en base). access_token/refresh_token van en texto plano (no por
// crypto.Encrypt, a diferencia de db.UpsertGameAccount) porque esta prueba
// nunca los relee desde la base — GetReceiverAccountID/CheckFriendship/
// SendGift reciben el *types.GameAccount ya armado en memoria por el test,
// con su AccessToken en claro, y lo mandan tal cual al servidor Epic
// simulado.
func insertBotAccount(t *testing.T, conn *sql.DB, bot types.GameAccount) {
	t.Helper()
	if _, err := conn.Exec(`
		INSERT INTO game_accounts (id, display_name, remaining_gifts, vbucks,
			access_token, access_token_exp_date, refresh_token, refresh_token_exp_date,
			is_active, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,'refresh-test',$6,true,NOW(),NOW())`,
		bot.ID, bot.DisplayName, bot.RemainingGifts, bot.VBucks, bot.AccessToken, bot.AccessTokenExpDate); err != nil {
		t.Fatalf("insert game_accounts: %v", err)
	}
	t.Cleanup(func() { conn.Exec(`DELETE FROM game_accounts WHERE id=$1`, bot.ID) })
}

// botAccountState lee remaining_gifts/vbucks directo de game_accounts — la
// misma fuente de verdad que db.DecrementRemainingGifts/db.DeductBotVbucks
// escriben, nunca el struct en memoria (que solo refleja lo que el propio
// processOrder ASUME que escribió, no lo que realmente quedó grabado).
func botAccountState(t *testing.T, conn *sql.DB, botID uuid.UUID) (remainingGifts, vbucks int) {
	t.Helper()
	if err := conn.QueryRow(`SELECT remaining_gifts, vbucks FROM game_accounts WHERE id=$1`, botID).Scan(&remainingGifts, &vbucks); err != nil {
		t.Fatalf("no se pudo leer el estado del bot: %v", err)
	}
	return
}

// TestFullPurchaseFlow_EntregaExitosa cubre el recorrido feliz completo:
// pago aprobado (800 KC) → compra de un ítem (500 KC) → el worker reclama
// el pedido → un bot amigo (con fondos, cupos y más de 48h de amistad)
// entrega el regalo. El balance final (300 KC) y el estado 'sent' con
// evidencia de entrega confirman que las CUATRO etapas encajaron con los
// datos reales que se van pasando entre sí (nunca con valores
// reinventados a mano en cada paso).
func TestFullPurchaseFlow_EntregaExitosa(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()

	creditApprovedPayment(t, conn, custID, 800)
	if got := kcBalance(t, conn, custID); got != 800 {
		t.Fatalf("tras acreditar el pago esperaba 800 KC, obtuve %d", got)
	}

	order := buyItemWithKC(t, conn, custID, 500)
	if order.Status != "pending" {
		t.Fatalf("el pedido debería crearse 'pending', obtuve %q", order.Status)
	}
	if got := kcBalance(t, conn, custID); got != 300 {
		t.Fatalf("tras la compra esperaba 300 KC (800-500), obtuve %d", got)
	}

	claimed := claimOrder(t, conn, order.ID)

	const receiverAccountID = "receiver-full-flow-0000000000001"
	bot := types.GameAccount{
		ID: uuid.New(), DisplayName: "bot_full_flow_ok",
		RemainingGifts: 5, VBucks: 999999,
		AccessToken: "tok", AccessTokenExpDate: time.Now().Add(24 * time.Hour),
	}
	insertBotAccount(t, conn, bot)
	botIDClean := strings.ReplaceAll(bot.ID.String(), "-", "")

	server := epicMockServer(t, receiverAccountID, botIDClean, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"profileRevision":1,"profileId":"common_core"}`)
	})
	withMockedEpicURLs(t, server.URL)

	processOrder(conn, claimed, []types.GameAccount{bot})

	var finalStatus string
	var deliveryEvidence sql.NullString
	if err := conn.QueryRow(`SELECT status, delivery_evidence FROM orders WHERE id=$1`, order.ID).Scan(&finalStatus, &deliveryEvidence); err != nil {
		t.Fatalf("no se pudo leer el pedido final: %v", err)
	}
	if finalStatus != "sent" {
		t.Fatalf("el pedido debería quedar 'sent', obtuve %q", finalStatus)
	}
	if !deliveryEvidence.Valid || deliveryEvidence.String == "" {
		t.Error("un pedido entregado debería guardar la evidencia cruda de Epic (para disputas de pago)")
	}
	if got := kcBalance(t, conn, custID); got != 300 {
		t.Errorf("una entrega exitosa no debe tocar el balance (sigue en 300), obtuve %d", got)
	}

	// El bot arrancó con 5 cupos y 999999 V-Bucks; esta entrega (item de
	// 500 V-Bucks) debe descontar EXACTAMENTE 1 cupo y 500 V-Bucks en la
	// fila real de game_accounts — no en el struct en memoria (ver
	// botAccountState).
	gifts, vbucks := botAccountState(t, conn, bot.ID)
	if gifts != 4 {
		t.Errorf("tras una entrega exitosa el bot debería tener 4 cupos restantes (5-1), obtuve %d", gifts)
	}
	if vbucks != 999999-500 {
		t.Errorf("tras una entrega exitosa el bot debería tener %d V-Bucks (999999-500), obtuve %d", 999999-500, vbucks)
	}

	// ── Sin descuentos duplicados al reintentar ──
	// El pedido ya quedó 'sent' (arriba). El mismo mecanismo real que evita
	// un reenvío — la cláusula WHERE de db.ClaimPendingOrders, que solo
	// reclama pedidos 'pending' o 'processing' atascados hace más de 15
	// minutos — debe negarse a reclamarlo de nuevo aunque se llame otra vez
	// (por ejemplo, tras reiniciar el worker). Sin ese reclamo no hay
	// segunda llamada a processOrder, así que el bot no puede perder un
	// segundo cupo/V-Bucks por el mismo pedido ya entregado.
	reclaimed, err := db.ClaimPendingOrders(conn)
	if err != nil {
		t.Fatalf("ClaimPendingOrders (reintento): %v", err)
	}
	for _, o := range reclaimed {
		if o.ID == order.ID {
			t.Fatalf("un pedido ya 'sent' no debería poder reclamarse de nuevo (reintento)")
		}
	}
	giftsAfterRetry, vbucksAfterRetry := botAccountState(t, conn, bot.ID)
	if giftsAfterRetry != gifts || vbucksAfterRetry != vbucks {
		t.Errorf("un reintento sobre un pedido ya entregado no debe volver a descontar cupos/V-Bucks: antes=(%d,%d) después=(%d,%d)",
			gifts, vbucks, giftsAfterRetry, vbucksAfterRetry)
	}
}

// TestFullPurchaseFlow_RechazoDefinitivoReembolsaCompleto cubre el mismo
// recorrido, pero con un rechazo DEFINITIVO y verificable de Epic al
// intentar entregar (no ErrAlreadyOwned, no un fallo de transporte — un
// 400 con un errorCode real que no reconocemos, igual que
// TestSendGift_RechazoDefinitivoSinCodigoReconocido en el paquete
// fortnite). El pedido debe terminar 'refunded' y el balance debe volver
// EXACTAMENTE al monto acreditado por el pago — la prueba de que gastar y
// reembolsar se cancelan entre sí sin perder ni inventar KC.
func TestFullPurchaseFlow_RechazoDefinitivoReembolsaCompleto(t *testing.T) {
	conn := setupShopTestDB(t)
	custID, cleanup := newShopTestCustomer(t, conn, 0)
	defer cleanup()

	creditApprovedPayment(t, conn, custID, 800)
	order := buyItemWithKC(t, conn, custID, 500)
	if got := kcBalance(t, conn, custID); got != 300 {
		t.Fatalf("tras la compra esperaba 300 KC (800-500), obtuve %d", got)
	}

	claimed := claimOrder(t, conn, order.ID)

	const receiverAccountID = "receiver-full-flow-0000000000002"
	bot := types.GameAccount{
		ID: uuid.New(), DisplayName: "bot_full_flow_rechazo",
		RemainingGifts: 5, VBucks: 999999,
		AccessToken: "tok", AccessTokenExpDate: time.Now().Add(24 * time.Hour),
	}
	botIDClean := strings.ReplaceAll(bot.ID.String(), "-", "")

	server := epicMockServer(t, receiverAccountID, botIDClean, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errorCode":"errors.com.epicgames.algo.que.no.reconocemos"}`)
	})
	withMockedEpicURLs(t, server.URL)

	processOrder(conn, claimed, []types.GameAccount{bot})

	var finalStatus string
	if err := conn.QueryRow(`SELECT status FROM orders WHERE id=$1`, order.ID).Scan(&finalStatus); err != nil {
		t.Fatalf("no se pudo leer el pedido final: %v", err)
	}
	if finalStatus != "refunded" {
		t.Fatalf("el pedido debería quedar 'refunded' tras el rechazo definitivo de Epic, obtuve %q", finalStatus)
	}
	if got := kcBalance(t, conn, custID); got != 800 {
		t.Errorf("el balance debería volver exactamente a lo acreditado por el pago (800 = 800 - 500 + 500), obtuve %d", got)
	}
}
