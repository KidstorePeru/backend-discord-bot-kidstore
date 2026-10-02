package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ==================== PAGOS MANUALES CON COMPROBANTE ====================

// MaxPendingManualPayments — solicitudes en revisión que puede tener un
// cliente a la vez.
const MaxPendingManualPayments = 2

// ProofRetention — cuánto se guarda la imagen del comprobante.
const ProofRetention = 30 * 24 * time.Hour

var (
	ErrTooManyPendingManual  = errors.New("demasiadas solicitudes en revisión")
	ErrManualAlreadyReviewed = errors.New("esta solicitud ya fue revisada")
)

type ManualPaymentRequest struct {
	ID               uuid.UUID  `json:"id"`
	CustomerID       uuid.UUID  `json:"-"`
	PackageID        string     `json:"package_id"`
	PackageName      string     `json:"package_name"`
	KCAmount         int        `json:"kc_amount"`
	Amount           float64    `json:"amount"`
	Currency         string     `json:"currency"`
	AmountPEN        float64    `json:"amount_pen"`
	Method           string     `json:"method"`
	OperationNumber  string     `json:"operation_number"`
	ProofKey         *string    `json:"-"`
	ProofContentType string     `json:"proof_content_type"`
	Lang             string     `json:"-"`
	Status           string     `json:"status"`
	RejectReason     *string    `json:"reject_reason,omitempty"`
	ReviewedBy       *string    `json:"reviewed_by,omitempty"`
	ReviewedAt       *time.Time `json:"reviewed_at,omitempty"`
	RechargeID       *uuid.UUID `json:"recharge_id,omitempty"`
	ProofDeleted     bool       `json:"proof_deleted"`
	CreatedAt        time.Time  `json:"created_at"`
}

const manualColumns = `id, customer_id, package_id, package_name, kc_amount, amount, currency, amount_pen, method,
	operation_number, proof_key, proof_content_type, lang, status, reject_reason, reviewed_by, reviewed_at,
	recharge_id, proof_deleted_at IS NOT NULL, created_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanManual(r rowScanner) (ManualPaymentRequest, error) {
	var m ManualPaymentRequest
	var proofKey, reason, reviewedBy sql.NullString
	var reviewedAt sql.NullTime
	var rechargeID uuid.NullUUID
	err := r.Scan(&m.ID, &m.CustomerID, &m.PackageID, &m.PackageName, &m.KCAmount, &m.Amount, &m.Currency, &m.AmountPEN,
		&m.Method, &m.OperationNumber, &proofKey, &m.ProofContentType, &m.Lang, &m.Status, &reason, &reviewedBy,
		&reviewedAt, &rechargeID, &m.ProofDeleted, &m.CreatedAt)
	if err != nil {
		return m, err
	}
	if proofKey.Valid {
		m.ProofKey = &proofKey.String
	}
	if reason.Valid {
		m.RejectReason = &reason.String
	}
	if reviewedBy.Valid {
		m.ReviewedBy = &reviewedBy.String
	}
	if reviewedAt.Valid {
		m.ReviewedAt = &reviewedAt.Time
	}
	if rechargeID.Valid {
		id := rechargeID.UUID
		m.RechargeID = &id
	}
	return m, nil
}

func queryManual(db *sql.DB, query string, args ...any) ([]ManualPaymentRequest, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ManualPaymentRequest{}
	for rows.Next() {
		m, err := scanManual(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CreateManualPaymentRequest registra la solicitud (con el comprobante ya
// guardado en proofKey). Falla con ErrTooManyPendingManual si el cliente ya
// tiene MaxPendingManualPayments en revisión.
func CreateManualPaymentRequest(db *sql.DB, m ManualPaymentRequest) (ManualPaymentRequest, error) {
	tx, err := db.Begin()
	if err != nil {
		return m, err
	}
	defer tx.Rollback()
	// Bloquea al cliente: dos envíos simultáneos no pueden pasar el límite.
	if _, err := tx.Exec(`SELECT 1 FROM customers WHERE id = $1 FOR UPDATE`, m.CustomerID); err != nil {
		return m, err
	}
	var pending int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM manual_payment_requests WHERE customer_id = $1 AND status = 'pending'`,
		m.CustomerID).Scan(&pending); err != nil {
		return m, err
	}
	if pending >= MaxPendingManualPayments {
		return m, ErrTooManyPendingManual
	}
	row := tx.QueryRow(`INSERT INTO manual_payment_requests
		(id, customer_id, package_id, package_name, kc_amount, amount, currency, amount_pen, method,
		 operation_number, proof_key, proof_content_type, lang)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING `+manualColumns,
		m.ID, m.CustomerID, m.PackageID, m.PackageName, m.KCAmount, m.Amount, m.Currency, m.AmountPEN, m.Method,
		m.OperationNumber, m.ProofKey, m.ProofContentType, m.Lang)
	created, err := scanManual(row)
	if err != nil {
		return m, err
	}
	return created, tx.Commit()
}

func GetManualPaymentRequest(db *sql.DB, id uuid.UUID) (ManualPaymentRequest, error) {
	return scanManual(db.QueryRow(`SELECT `+manualColumns+` FROM manual_payment_requests WHERE id = $1`, id))
}

// ListManualPaymentsByCustomer — las solicitudes más recientes del cliente.
func ListManualPaymentsByCustomer(db *sql.DB, customerID uuid.UUID, limit int) ([]ManualPaymentRequest, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	return queryManual(db, `SELECT `+manualColumns+` FROM manual_payment_requests
		WHERE customer_id = $1 ORDER BY created_at DESC LIMIT $2`, customerID, limit)
}

// GetManualPaymentByRechargeID — la solicitud que generó esa recarga, si la hay
// (para que el comprobante muestre lo que el cliente pagó de verdad).
func GetManualPaymentByRechargeID(db *sql.DB, rechargeID uuid.UUID) (ManualPaymentRequest, error) {
	return scanManual(db.QueryRow(`SELECT `+manualColumns+` FROM manual_payment_requests WHERE recharge_id = $1`, rechargeID))
}

// AdminListManualPayments — por estado ("" = todas); las pendientes, de la
// más antigua a la más nueva (para atenderlas en orden).
func AdminListManualPayments(db *sql.DB, status string, limit int) ([]ManualPaymentRequest, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	order := "created_at DESC"
	if status == "pending" {
		order = "created_at ASC"
	}
	return queryManual(db, `SELECT `+manualColumns+` FROM manual_payment_requests
		WHERE ($1 = '' OR status = $1) ORDER BY `+order+` LIMIT $2`, status, limit)
}

// OperationNumberUses cuenta OTRAS solicitudes con el mismo número de
// operación (posible comprobante reutilizado).
func OperationNumberUses(db *sql.DB, operation string, exclude uuid.UUID) (int, error) {
	operation = strings.TrimSpace(operation)
	if operation == "" {
		return 0, nil
	}
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM manual_payment_requests
		WHERE lower(operation_number) = lower($1) AND id <> $2`, operation, exclude).Scan(&n)
	return n, err
}

// ApproveManualPayment acredita los KC y marca la solicitud como aprobada en
// UNA transacción: si dos personas la aprueban a la vez (panel y Discord),
// solo una acredita; la otra recibe ErrManualAlreadyReviewed.
//
// allowRejected permite aprobar una solicitud que se rechazó por error (el
// cliente habló con soporte); solo el panel admin lo pide explícitamente. Una
// aprobada nunca se vuelve a acreditar.
func ApproveManualPayment(db *sql.DB, id uuid.UUID, reviewer string, allowRejected bool) (ManualPaymentRequest, uuid.UUID, error) {
	tx, err := db.Begin()
	if err != nil {
		return ManualPaymentRequest{}, uuid.Nil, err
	}
	defer tx.Rollback()
	m, err := scanManual(tx.QueryRow(`SELECT `+manualColumns+` FROM manual_payment_requests WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return m, uuid.Nil, err
	}
	if m.Status != "pending" && !(allowRejected && m.Status == "rejected") {
		return m, uuid.Nil, ErrManualAlreadyReviewed
	}
	res, err := tx.Exec(`UPDATE customers SET kc_balance = kc_balance + $1, updated_at = NOW() WHERE id = $2 AND is_active = true`,
		m.KCAmount, m.CustomerID)
	if err != nil {
		return m, uuid.Nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return m, uuid.Nil, fmt.Errorf("el cliente no existe o está desactivado")
	}
	rechargeID := uuid.New()
	note := fmt.Sprintf("Comprobante %s · %s %.2f %s", strings.ToUpper(m.ID.String()[:8]), m.Method, m.Amount, m.Currency)
	if m.OperationNumber != "" {
		note += " · op. " + m.OperationNumber
	}
	if m.Status == "rejected" {
		note += " · aprobado tras rechazo"
	}
	if _, err := tx.Exec(`INSERT INTO kc_recharges (id, customer_id, amount_kc, amount_soles, method, note, approved_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())`,
		rechargeID, m.CustomerID, m.KCAmount, m.AmountPEN, m.Method, note, reviewer); err != nil {
		return m, uuid.Nil, err
	}
	if _, err := tx.Exec(`UPDATE manual_payment_requests SET status = 'approved', reviewed_by = $2, reviewed_at = NOW(),
		recharge_id = $3 WHERE id = $1`, id, reviewer, rechargeID); err != nil {
		return m, uuid.Nil, err
	}
	if err := tx.Commit(); err != nil {
		return m, uuid.Nil, err
	}
	m.Status, m.RechargeID, m.ReviewedBy = "approved", &rechargeID, &reviewer
	return m, rechargeID, nil
}

// RejectManualPayment rechaza una solicitud pendiente con un motivo.
func RejectManualPayment(db *sql.DB, id uuid.UUID, reason, reviewer string) (ManualPaymentRequest, error) {
	m, err := scanManual(db.QueryRow(`UPDATE manual_payment_requests
		SET status = 'rejected', reject_reason = $2, reviewed_by = $3, reviewed_at = NOW()
		WHERE id = $1 AND status = 'pending' RETURNING `+manualColumns, id, reason, reviewer))
	if errors.Is(err, sql.ErrNoRows) {
		if _, getErr := GetManualPaymentRequest(db, id); getErr == nil {
			return m, ErrManualAlreadyReviewed
		}
	}
	return m, err
}

// ExpiredProofs — solicitudes cuyo comprobante ya superó ProofRetention.
func ExpiredProofs(db *sql.DB, now time.Time) (map[uuid.UUID]string, error) {
	rows, err := db.Query(`SELECT id, proof_key FROM manual_payment_requests
		WHERE proof_key IS NOT NULL AND created_at < $1`, now.Add(-ProofRetention))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, err
		}
		out[id] = key
	}
	return out, rows.Err()
}

// MarkProofDeleted deja constancia de que la imagen ya se borró.
func MarkProofDeleted(db *sql.DB, id uuid.UUID) error {
	_, err := db.Exec(`UPDATE manual_payment_requests SET proof_key = NULL, proof_deleted_at = NOW() WHERE id = $1`, id)
	return err
}
