package db

import (
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ==================== RESEÑAS VERIFICADAS ====================

// ReviewWindow — hasta cuánto tiempo después de la entrega se puede reseñar.
const ReviewWindow = 90 * 24 * time.Hour

var (
	// ErrReviewNotAllowed: el pedido no es del cliente, no se entregó, o ya
	// pasó el plazo para reseñarlo.
	ErrReviewNotAllowed = errors.New("este pedido no se puede reseñar")
	ErrReviewExists     = errors.New("este pedido ya tiene una reseña")
)

type Review struct {
	ID          uuid.UUID  `json:"id"`
	OrderID     uuid.UUID  `json:"order_id"`
	DisplayName string     `json:"display_name"`
	Rating      int        `json:"rating"`
	Comment     string     `json:"comment"`
	ItemName    string     `json:"item_name"`
	ItemImage   string     `json:"item_image"`
	Lang        string     `json:"lang"`
	Status      string     `json:"status"`
	Reply       *string    `json:"reply,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ModeratedAt *time.Time `json:"moderated_at,omitempty"`
}

type ReviewSummary struct {
	Average float64 `json:"average"`
	Count   int     `json:"count"`
}

const reviewColumns = `id, order_id, display_name, rating, comment, item_name, item_image, lang, status, reply, created_at, moderated_at`

func scanReviews(rows *sql.Rows) ([]Review, error) {
	defer rows.Close()
	out := []Review{}
	for rows.Next() {
		var r Review
		var reply sql.NullString
		var moderated sql.NullTime
		if err := rows.Scan(&r.ID, &r.OrderID, &r.DisplayName, &r.Rating, &r.Comment, &r.ItemName, &r.ItemImage,
			&r.Lang, &r.Status, &reply, &r.CreatedAt, &moderated); err != nil {
			return nil, err
		}
		if reply.Valid {
			r.Reply = &reply.String
		}
		if moderated.Valid {
			r.ModeratedAt = &moderated.Time
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateReview guarda la reseña de un pedido ENTREGADO del propio cliente
// (una sola por pedido). Queda pendiente hasta que el admin la apruebe.
// displayName es el nombre ya enmascarado que se mostrará en la web.
func CreateReview(db *sql.DB, customerID, orderID uuid.UUID, rating int, comment, displayName, lang string) (Review, error) {
	if rating < 1 || rating > 5 {
		return Review{}, ErrReviewNotAllowed
	}
	tx, err := db.Begin()
	if err != nil {
		return Review{}, err
	}
	defer tx.Rollback()

	var owner uuid.UUID
	var status, itemName string
	var itemImage sql.NullString
	var deliveredAt time.Time
	err = tx.QueryRow(`SELECT customer_id, status, item_name, item_image, updated_at FROM orders WHERE id = $1 FOR UPDATE`, orderID).
		Scan(&owner, &status, &itemName, &itemImage, &deliveredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Review{}, ErrReviewNotAllowed
	}
	if err != nil {
		return Review{}, err
	}
	if owner != customerID || status != "sent" || time.Since(deliveredAt) > ReviewWindow {
		return Review{}, ErrReviewNotAllowed
	}
	rows, err := tx.Query(`INSERT INTO reviews (order_id, customer_id, display_name, rating, comment, item_name, item_image, lang)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (order_id) DO NOTHING
		RETURNING `+reviewColumns,
		orderID, customerID, displayName, rating, comment, itemName, itemImage.String, lang)
	if err != nil {
		return Review{}, err
	}
	created, err := scanReviews(rows)
	if err != nil {
		return Review{}, err
	}
	if len(created) == 0 {
		return Review{}, ErrReviewExists
	}
	return created[0], tx.Commit()
}

// ReviewableOrders devuelve los pedidos entregados del cliente que todavía
// puede reseñar (sin reseña y dentro del plazo).
func ReviewableOrders(db *sql.DB, customerID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := db.Query(`SELECT o.id FROM orders o
		WHERE o.customer_id = $1 AND o.status = 'sent' AND o.updated_at > $2
		  AND NOT EXISTS (SELECT 1 FROM reviews r WHERE r.order_id = o.id)
		ORDER BY o.updated_at DESC LIMIT 100`, customerID, time.Now().Add(-ReviewWindow))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// PublicReviews devuelve las reseñas aprobadas más recientes y el promedio de
// TODAS las aprobadas.
func PublicReviews(db *sql.DB, limit int) ([]Review, ReviewSummary, error) {
	if limit <= 0 || limit > 50 {
		limit = 12
	}
	var summary ReviewSummary
	if err := db.QueryRow(`SELECT COALESCE(AVG(rating), 0), COUNT(*) FROM reviews WHERE status = 'approved'`).
		Scan(&summary.Average, &summary.Count); err != nil {
		return nil, summary, err
	}
	rows, err := db.Query(`SELECT `+reviewColumns+` FROM reviews WHERE status = 'approved'
		ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, summary, err
	}
	list, err := scanReviews(rows)
	return list, summary, err
}

// AdminListReviews lista reseñas por estado ("" = todas).
func AdminListReviews(db *sql.DB, status string, limit int) ([]Review, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := db.Query(`SELECT `+reviewColumns+` FROM reviews
		WHERE ($1 = '' OR status = $1) ORDER BY created_at DESC LIMIT $2`, status, limit)
	if err != nil {
		return nil, err
	}
	return scanReviews(rows)
}

// ModerateReview aprueba o rechaza una reseña, con una respuesta pública
// opcional de la tienda (reply vacío = sin respuesta).
func ModerateReview(db *sql.DB, id uuid.UUID, status, reply string) error {
	if status != "approved" && status != "rejected" && status != "pending" {
		return errors.New("estado inválido")
	}
	var replyArg any
	if reply != "" {
		replyArg = reply
	}
	res, err := db.Exec(`UPDATE reviews SET status = $2, reply = $3, moderated_at = NOW() WHERE id = $1`, id, status, replyArg)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CountDeliveredOrders cuenta los pedidos entregados por la web.
func CountDeliveredOrders(db *sql.DB) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM orders WHERE status = 'sent'`).Scan(&n)
	return n, err
}
