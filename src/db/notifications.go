package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ==================== NOTIFICACIONES (campana de la web) ====================

// Tipos de aviso. El frontend arma el texto de cada uno en el idioma del
// cliente a partir de Data.
const (
	NotifWishlistBack = "wishlist_back" // volvió a la tienda un objeto de la lista de deseos
	NotifOrderSent    = "order_sent"    // pedido entregado
	NotifOrderFailed  = "order_failed"  // pedido no entregado (refunded indica si ya se devolvieron los KC)
	NotifKCCredited   = "kc_credited"   // recarga de KC acreditada
	NotifManualRejected = "manual_payment_rejected" // comprobante de pago manual rechazado (con motivo)
)

// NotificationRetention — los avisos más viejos se borran solos.
const NotificationRetention = 90 * 24 * time.Hour

type Notification struct {
	ID        uuid.UUID       `json:"id"`
	Kind      string          `json:"kind"`
	Data      json.RawMessage `json:"data"`
	Read      bool            `json:"read"`
	CreatedAt time.Time       `json:"created_at"`
}

// AddNotification guarda un aviso para la campana. Es "mejor esfuerzo": un
// fallo se registra en el log pero nunca interrumpe lo que lo originó (un
// pedido entregado sigue entregado aunque no se pueda guardar el aviso).
func AddNotification(db *sql.DB, customerID uuid.UUID, kind string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		slog.Warn("notificación: datos inválidos", "kind", kind, "error", err)
		return
	}
	if _, err := db.Exec(`INSERT INTO notifications (customer_id, kind, data) VALUES ($1, $2, $3::jsonb)`,
		customerID, kind, string(payload)); err != nil {
		slog.Warn("notificación: no se pudo guardar", "kind", kind, "customer", customerID, "error", err)
	}
}

// ListNotifications devuelve los avisos más recientes y cuántos hay sin leer.
func ListNotifications(db *sql.DB, customerID uuid.UUID, limit int) ([]Notification, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := db.Query(`SELECT id, kind, data, read_at IS NOT NULL, created_at FROM notifications
		WHERE customer_id = $1 ORDER BY created_at DESC, id LIMIT $2`, customerID, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		var data string
		if err := rows.Scan(&n.ID, &n.Kind, &data, &n.Read, &n.CreatedAt); err != nil {
			return nil, 0, err
		}
		n.Data = json.RawMessage(data)
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	unread, err := CountUnreadNotifications(db, customerID)
	return out, unread, err
}

func CountUnreadNotifications(db *sql.DB, customerID uuid.UUID) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE customer_id = $1 AND read_at IS NULL`, customerID).Scan(&n)
	return n, err
}

// MarkNotificationsRead marca como leídos todos los avisos del cliente.
func MarkNotificationsRead(db *sql.DB, customerID uuid.UUID) error {
	_, err := db.Exec(`UPDATE notifications SET read_at = NOW() WHERE customer_id = $1 AND read_at IS NULL`, customerID)
	return err
}

// DeleteOldNotifications borra los avisos con más de NotificationRetention.
func DeleteOldNotifications(db *sql.DB) (int64, error) {
	res, err := db.Exec(`DELETE FROM notifications WHERE created_at < $1`, time.Now().Add(-NotificationRetention))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ==================== PREFERENCIAS DE AVISO ====================

type NotificationPrefs struct {
	Email   bool `json:"email"`
	Discord bool `json:"discord"`
}

func GetNotificationPrefs(db *sql.DB, customerID uuid.UUID) (NotificationPrefs, error) {
	var p NotificationPrefs
	err := db.QueryRow(`SELECT notify_email, notify_discord FROM customers WHERE id = $1`, customerID).Scan(&p.Email, &p.Discord)
	return p, err
}

func SetNotificationPrefs(db *sql.DB, customerID uuid.UUID, p NotificationPrefs) error {
	_, err := db.Exec(`UPDATE customers SET notify_email = $1, notify_discord = $2, updated_at = NOW() WHERE id = $3`,
		p.Email, p.Discord, customerID)
	return err
}

// ==================== LISTA DE DESEOS ====================

// WishlistLimit — máximo de objetos por cliente.
const WishlistLimit = 30

var ErrWishlistFull = errors.New("lista de deseos llena")

type WishlistItem struct {
	ItemID    string    `json:"item_id"`
	Name      string    `json:"name"`
	ItemType  string    `json:"item_type"`
	Image     string    `json:"image"`
	CreatedAt time.Time `json:"created_at"`
}

// AddWishlistItem agrega un objeto (si ya estaba, solo actualiza sus datos).
// alreadyInShopSince: si el objeto está en la tienda AHORA, la fecha de
// entrada de esa aparición — se marca como ya avisada, porque el cliente lo
// está viendo; el aviso llegará cuando vuelva la próxima vez.
func AddWishlistItem(db *sql.DB, customerID uuid.UUID, item WishlistItem, lang string, alreadyInShopSince *time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Bloquea al cliente para que dos pestañas no superen el límite a la vez.
	if _, err := tx.Exec(`SELECT 1 FROM customers WHERE id = $1 FOR UPDATE`, customerID); err != nil {
		return err
	}
	var count int
	var exists bool
	if err := tx.QueryRow(`SELECT COUNT(*), COALESCE(BOOL_OR(item_id = $2), false) FROM wishlist_items WHERE customer_id = $1`,
		customerID, item.ItemID).Scan(&count, &exists); err != nil {
		return err
	}
	if !exists && count >= WishlistLimit {
		return ErrWishlistFull
	}
	if _, err := tx.Exec(`INSERT INTO wishlist_items (customer_id, item_id, name, item_type, image, lang, notified_in_date)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (customer_id, item_id) DO UPDATE SET name = EXCLUDED.name, item_type = EXCLUDED.item_type,
			image = EXCLUDED.image, lang = EXCLUDED.lang,
			notified_in_date = GREATEST(wishlist_items.notified_in_date, EXCLUDED.notified_in_date)`,
		customerID, item.ItemID, item.Name, item.ItemType, item.Image, lang, alreadyInShopSince); err != nil {
		return err
	}
	return tx.Commit()
}

func RemoveWishlistItem(db *sql.DB, customerID uuid.UUID, itemID string) error {
	_, err := db.Exec(`DELETE FROM wishlist_items WHERE customer_id = $1 AND item_id = $2`, customerID, itemID)
	return err
}

func ListWishlistItems(db *sql.DB, customerID uuid.UUID) ([]WishlistItem, error) {
	rows, err := db.Query(`SELECT item_id, name, item_type, image, created_at FROM wishlist_items
		WHERE customer_id = $1 ORDER BY created_at DESC`, customerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WishlistItem{}
	for rows.Next() {
		var it WishlistItem
		if err := rows.Scan(&it.ItemID, &it.Name, &it.ItemType, &it.Image, &it.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// WishlistMatch es una fila de la lista de deseos cuyo objeto está hoy en la
// tienda y todavía no se avisó por esta aparición.
type WishlistMatch struct {
	RowID      uuid.UUID
	CustomerID uuid.UUID
	ItemID     string
	Name       string
	Image      string
	Lang       string
}

// PendingWishlistMatches busca, entre los objetos que están hoy en la tienda
// (itemID → fecha de entrada de su aparición actual), los que alguien sigue y
// todavía no se avisaron por esa aparición.
func PendingWishlistMatches(db *sql.DB, inDates map[string]time.Time) ([]WishlistMatch, error) {
	if len(inDates) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(inDates))
	for id := range inDates {
		ids = append(ids, id)
	}
	rows, err := db.Query(`SELECT id, customer_id, item_id, name, image, lang, notified_in_date FROM wishlist_items
		WHERE item_id = ANY($1) ORDER BY customer_id, created_at`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WishlistMatch
	for rows.Next() {
		var m WishlistMatch
		var notified sql.NullTime
		if err := rows.Scan(&m.RowID, &m.CustomerID, &m.ItemID, &m.Name, &m.Image, &m.Lang, &notified); err != nil {
			return nil, err
		}
		if notified.Valid && !notified.Time.Before(inDates[m.ItemID]) {
			continue // ya se avisó por esta aparición
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ClaimWishlistNotification marca la fila como avisada para la aparición que
// entró en inDate. Devuelve false si ya estaba marcada (otro proceso la tomó
// primero): así nunca se avisa dos veces por el mismo regreso.
func ClaimWishlistNotification(db *sql.DB, rowID uuid.UUID, inDate time.Time) (bool, error) {
	res, err := db.Exec(`UPDATE wishlist_items SET notified_in_date = $2
		WHERE id = $1 AND (notified_in_date IS NULL OR notified_in_date < $2)`, rowID, inDate)
	if err != nil {
		return false, fmt.Errorf("marcando aviso de lista de deseos: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
