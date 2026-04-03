package db

import (
	"KidStoreStore/src/crypto"
	"KidStoreStore/src/types"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ==================== SETUP ====================

func CreateTables(db *sql.DB) error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS customers (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			epic_username VARCHAR(255) NOT NULL UNIQUE,
			email VARCHAR(255) UNIQUE,
			password_hash VARCHAR(255) NOT NULL,
			kc_balance INTEGER NOT NULL DEFAULT 0 CHECK (kc_balance >= 0),
			discord_id VARCHAR(255) UNIQUE,
			discord_username VARCHAR(255),
			is_active BOOLEAN NOT NULL DEFAULT true,
			created_at TIMESTAMP NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS kc_recharges (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
			amount_kc INTEGER NOT NULL CHECK (amount_kc > 0),
			amount_soles NUMERIC(10,2),
			method VARCHAR(50) NOT NULL DEFAULT 'manual',
			note TEXT,
			approved_by VARCHAR(255),
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS orders (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			customer_id UUID NOT NULL REFERENCES customers(id),
			epic_username VARCHAR(255) NOT NULL,
			item_offer_id VARCHAR(500) NOT NULL,
			item_name VARCHAR(255) NOT NULL,
			item_image VARCHAR(500),
			price_kc INTEGER NOT NULL CHECK (price_kc > 0),
			price_vbucks INTEGER NOT NULL DEFAULT 0,
			status VARCHAR(50) NOT NULL DEFAULT 'pending'
				CHECK (status IN ('pending','processing','sent','failed','refunded')),
			game_account_id UUID,
			error_msg TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			customer_id UUID REFERENCES customers(id) ON DELETE SET NULL,
			action VARCHAR(100) NOT NULL,
			details TEXT,
			ip_address VARCHAR(45),
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS password_reset_tokens (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
			token VARCHAR(255) NOT NULL UNIQUE,
			expires_at TIMESTAMP NOT NULL,
			used_at TIMESTAMP,
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS email_verification_tokens (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
			token VARCHAR(255) NOT NULL UNIQUE,
			expires_at TIMESTAMP NOT NULL,
			used_at TIMESTAMP,
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS pending_registrations (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			epic_username VARCHAR(255) NOT NULL,
			email VARCHAR(255) NOT NULL UNIQUE,
			password_hash VARCHAR(255) NOT NULL,
			verification_token VARCHAR(255) NOT NULL UNIQUE,
			lang VARCHAR(2) NOT NULL DEFAULT 'es',
			expires_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS game_accounts (
			id UUID PRIMARY KEY,
			display_name VARCHAR(255) NOT NULL,
			remaining_gifts INTEGER NOT NULL DEFAULT 5,
			vbucks INTEGER NOT NULL DEFAULT 0,
			access_token TEXT NOT NULL DEFAULT '',
			access_token_exp_date TIMESTAMP NOT NULL DEFAULT NOW(),
			refresh_token TEXT NOT NULL DEFAULT '',
			refresh_token_exp_date TIMESTAMP NOT NULL DEFAULT NOW(),
			is_active BOOLEAN NOT NULL DEFAULT true,
			created_at TIMESTAMP NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS game_account_secrets (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			account_id UUID NOT NULL REFERENCES game_accounts(id) ON DELETE CASCADE,
			device_id VARCHAR(255) NOT NULL,
			secret TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS bot_schedule (
			id INTEGER PRIMARY KEY DEFAULT 1,
			enabled BOOLEAN NOT NULL DEFAULT true,
			start_hour INTEGER NOT NULL DEFAULT 0  CHECK (start_hour >= 0 AND start_hour <= 23),
			end_hour   INTEGER NOT NULL DEFAULT 9  CHECK (end_hour   >= 0 AND end_hour   <= 23),
			timezone   VARCHAR(64) NOT NULL DEFAULT 'America/Lima',
			updated_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`INSERT INTO bot_schedule (id, enabled, start_hour, end_hour, timezone)
			VALUES (1, true, 0, 9, 'America/Lima')
			ON CONFLICT (id) DO NOTHING`,
		`CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status)`,
		`CREATE INDEX IF NOT EXISTS idx_orders_customer ON orders(customer_id)`,
		`CREATE INDEX IF NOT EXISTS idx_kc_recharges_customer ON kc_recharges(customer_id)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_customer ON audit_logs(customer_id)`,
		`CREATE INDEX IF NOT EXISTS idx_reset_token ON password_reset_tokens(token)`,
		`CREATE INDEX IF NOT EXISTS idx_game_accounts_active ON game_accounts(is_active)`,
		`CREATE INDEX IF NOT EXISTS idx_verification_token ON email_verification_tokens(token)`,
		`CREATE INDEX IF NOT EXISTS idx_pending_reg_token ON pending_registrations(verification_token)`,
		`CREATE INDEX IF NOT EXISTS idx_pending_reg_email ON pending_registrations(email)`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='orders' AND column_name='price_vbucks') THEN
				ALTER TABLE orders ADD COLUMN price_vbucks INTEGER NOT NULL DEFAULT 0;
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='customers' AND column_name='discord_lang') THEN
				ALTER TABLE customers ADD COLUMN discord_lang VARCHAR(2) DEFAULT 'es';
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='customers' AND column_name='is_verified') THEN
				ALTER TABLE customers ADD COLUMN is_verified BOOLEAN NOT NULL DEFAULT true;
			END IF;
		END $$`,
		`CREATE TABLE IF NOT EXISTS bot_config (
			key   VARCHAR(50) NOT NULL,
			value VARCHAR(255) NOT NULL,
			UNIQUE(key, value)
		)`,
		`CREATE TABLE IF NOT EXISTS refresh_tokens (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
			token_hash VARCHAR(255) NOT NULL UNIQUE,
			expires_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_refresh_token_hash ON refresh_tokens(token_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_refresh_token_customer ON refresh_tokens(customer_id)`,
		`CREATE TABLE IF NOT EXISTS payment_transactions (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
			gateway VARCHAR(50) NOT NULL,
			payment_type VARCHAR(50) NOT NULL,
			product_id VARCHAR(100),
			product_name VARCHAR(255),
			amount_pen NUMERIC(10,2) NOT NULL DEFAULT 0,
			amount_usd NUMERIC(10,2) NOT NULL DEFAULT 0,
			kc_amount INTEGER NOT NULL DEFAULT 0,
			external_id VARCHAR(255),
			status VARCHAR(50) NOT NULL DEFAULT 'pending'
				CHECK (status IN ('pending','approved','failed','expired','fulfilled','activating')),
			created_at TIMESTAMP NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_tx_customer ON payment_transactions(customer_id)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_tx_external ON payment_transactions(external_id)`,
		`CREATE INDEX IF NOT EXISTS idx_payment_tx_status ON payment_transactions(status)`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='payment_transactions' AND column_name='progress') THEN
				ALTER TABLE payment_transactions ADD COLUMN progress TEXT DEFAULT '';
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='payment_transactions' AND column_name='activation_code') THEN
				ALTER TABLE payment_transactions ADD COLUMN activation_code VARCHAR(8);
				ALTER TABLE payment_transactions ADD COLUMN autobuyer_task_id VARCHAR(100);
			END IF;
		END $$`,
		`CREATE INDEX IF NOT EXISTS idx_payment_tx_activation ON payment_transactions(activation_code)`,
		`CREATE TABLE IF NOT EXISTS product_availability (
			product_id VARCHAR(100) PRIMARY KEY,
			enabled BOOLEAN NOT NULL DEFAULT true,
			schedule_enabled BOOLEAN NOT NULL DEFAULT false,
			start_hour INTEGER NOT NULL DEFAULT 0 CHECK (start_hour >= 0 AND start_hour <= 23),
			end_hour INTEGER NOT NULL DEFAULT 23 CHECK (end_hour >= 0 AND end_hour <= 23),
			timezone VARCHAR(64) NOT NULL DEFAULT 'America/Lima',
			updated_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		// Update CHECK constraint to include fulfilled and activating statuses
		`DO $$ BEGIN
			ALTER TABLE payment_transactions DROP CONSTRAINT IF EXISTS payment_transactions_status_check;
			ALTER TABLE payment_transactions ADD CONSTRAINT payment_transactions_status_check
				CHECK (status IN ('pending','approved','failed','expired','fulfilled','activating'));
		END $$`,
		`DELETE FROM pending_registrations WHERE expires_at < NOW()`,
		`DELETE FROM refresh_tokens WHERE expires_at < NOW()`,
	}

	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("error creating tables: %w", err)
		}
	}
	return nil
}

// ==================== PRODUCT AVAILABILITY ====================

type ProductAvailability struct {
	ProductID       string `json:"product_id"`
	Enabled         bool   `json:"enabled"`
	ScheduleEnabled bool   `json:"schedule_enabled"`
	StartHour       int    `json:"start_hour"`
	EndHour         int    `json:"end_hour"`
	Timezone        string `json:"timezone"`
}

func GetProductAvailability(db *sql.DB, productID string) (ProductAvailability, error) {
	var a ProductAvailability
	err := db.QueryRow(`SELECT product_id, enabled, schedule_enabled, start_hour, end_hour, timezone FROM product_availability WHERE product_id=$1`, productID).
		Scan(&a.ProductID, &a.Enabled, &a.ScheduleEnabled, &a.StartHour, &a.EndHour, &a.Timezone)
	if err != nil {
		// Not configured = available by default
		return ProductAvailability{ProductID: productID, Enabled: true}, nil
	}
	return a, nil
}

func GetAllProductAvailability(db *sql.DB) ([]ProductAvailability, error) {
	rows, err := db.Query(`SELECT product_id, enabled, schedule_enabled, start_hour, end_hour, timezone FROM product_availability ORDER BY product_id`)
	if err != nil { return nil, err }
	defer rows.Close()
	var items []ProductAvailability
	for rows.Next() {
		var a ProductAvailability
		if err := rows.Scan(&a.ProductID, &a.Enabled, &a.ScheduleEnabled, &a.StartHour, &a.EndHour, &a.Timezone); err != nil { return nil, err }
		items = append(items, a)
	}
	return items, nil
}

func UpsertProductAvailability(db *sql.DB, a ProductAvailability) error {
	_, err := db.Exec(`
		INSERT INTO product_availability (product_id, enabled, schedule_enabled, start_hour, end_hour, timezone, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,NOW())
		ON CONFLICT (product_id) DO UPDATE SET
			enabled=EXCLUDED.enabled, schedule_enabled=EXCLUDED.schedule_enabled,
			start_hour=EXCLUDED.start_hour, end_hour=EXCLUDED.end_hour,
			timezone=EXCLUDED.timezone, updated_at=NOW()`,
		a.ProductID, a.Enabled, a.ScheduleEnabled, a.StartHour, a.EndHour, a.Timezone)
	return err
}

func IsProductAvailable(db *sql.DB, productID string) bool {
	a, _ := GetProductAvailability(db, productID)
	if !a.Enabled { return false }
	if !a.ScheduleEnabled { return true }
	loc, err := time.LoadLocation(a.Timezone)
	if err != nil { loc = time.UTC }
	hour := time.Now().In(loc).Hour()
	if a.StartHour <= a.EndHour {
		return hour >= a.StartHour && hour < a.EndHour
	}
	return hour >= a.StartHour || hour < a.EndHour
}

// ==================== ENCRYPTION MIGRATION ====================

// MigrateEncryptTokens encrypts any plaintext tokens in the database.
// It's idempotent: already-encrypted values (prefixed with "enc:") are skipped.
func MigrateEncryptTokens(db *sql.DB, encKey string) error {
	if encKey == "" {
		return nil
	}

	// Migrate game_accounts tokens
	rows, err := db.Query(`SELECT id, access_token, refresh_token FROM game_accounts`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var id uuid.UUID
		var accessToken, refreshToken string
		if err := rows.Scan(&id, &accessToken, &refreshToken); err != nil {
			return err
		}
		// Skip if already encrypted
		if (accessToken == "" || len(accessToken) > 4 && accessToken[:4] == "enc:") &&
			(refreshToken == "" || len(refreshToken) > 4 && refreshToken[:4] == "enc:") {
			continue
		}
		encAccess, err := crypto.Encrypt(accessToken, encKey)
		if err != nil {
			return fmt.Errorf("migrating access token for %s: %w", id, err)
		}
		encRefresh, err := crypto.Encrypt(refreshToken, encKey)
		if err != nil {
			return fmt.Errorf("migrating refresh token for %s: %w", id, err)
		}
		if _, err := db.Exec(`UPDATE game_accounts SET access_token=$1, refresh_token=$2 WHERE id=$3`,
			encAccess, encRefresh, id); err != nil {
			return fmt.Errorf("updating encrypted tokens for %s: %w", id, err)
		}
	}

	// Migrate game_account_secrets
	sRows, err := db.Query(`SELECT id, device_id, secret FROM game_account_secrets`)
	if err != nil {
		return err
	}
	defer sRows.Close()

	for sRows.Next() {
		var id uuid.UUID
		var deviceID, secret string
		if err := sRows.Scan(&id, &deviceID, &secret); err != nil {
			return err
		}
		if (deviceID == "" || len(deviceID) > 4 && deviceID[:4] == "enc:") &&
			(secret == "" || len(secret) > 4 && secret[:4] == "enc:") {
			continue
		}
		encDeviceID, err := crypto.Encrypt(deviceID, encKey)
		if err != nil {
			return fmt.Errorf("migrating device_id for %s: %w", id, err)
		}
		encSecret, err := crypto.Encrypt(secret, encKey)
		if err != nil {
			return fmt.Errorf("migrating secret for %s: %w", id, err)
		}
		if _, err := db.Exec(`UPDATE game_account_secrets SET device_id=$1, secret=$2 WHERE id=$3`,
			encDeviceID, encSecret, id); err != nil {
			return fmt.Errorf("updating encrypted secrets for %s: %w", id, err)
		}
	}

	return nil
}

// ==================== BOT SCHEDULE ====================

func GetBotSchedule(db *sql.DB) (types.BotSchedule, error) {
	var s types.BotSchedule
	err := db.QueryRow(`SELECT id, enabled, start_hour, end_hour, timezone, updated_at FROM bot_schedule WHERE id=1`).
		Scan(&s.ID, &s.Enabled, &s.StartHour, &s.EndHour, &s.Timezone, &s.UpdatedAt)
	return s, err
}

func UpdateBotSchedule(db *sql.DB, enabled bool, startHour, endHour int, timezone string) error {
	if startHour < 0 || startHour > 23 || endHour < 0 || endHour > 23 {
		return fmt.Errorf("start_hour y end_hour deben estar entre 0 y 23")
	}
	if timezone == "" { timezone = "America/Lima" }
	if _, err := time.LoadLocation(timezone); err != nil {
		return fmt.Errorf("timezone inválida: %s", timezone)
	}
	_, err := db.Exec(`UPDATE bot_schedule SET enabled=$1, start_hour=$2, end_hour=$3, timezone=$4, updated_at=NOW() WHERE id=1`,
		enabled, startHour, endHour, timezone)
	return err
}

func IsWithinSchedule(db *sql.DB) (bool, string) {
	s, err := GetBotSchedule(db)
	if err != nil { return true, "" }
	if !s.Enabled { return false, "worker deshabilitado por el administrador" }
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil { loc = time.UTC }
	now := time.Now().In(loc)
	hour := now.Hour()
	var inRange bool
	if s.StartHour <= s.EndHour {
		inRange = hour >= s.StartHour && hour < s.EndHour
	} else {
		inRange = hour >= s.StartHour || hour < s.EndHour
	}
	if !inRange {
		return false, fmt.Sprintf("fuera de horario de operación (%02d:00 - %02d:00 %s) — hora actual: %02d:00",
			s.StartHour, s.EndHour, s.Timezone, hour)
	}
	return true, ""
}

// ==================== PENDING REGISTRATIONS ====================

type PendingRegistration struct {
	ID                uuid.UUID
	EpicUsername      string
	Email             string
	PasswordHash      string
	VerificationToken string
	Lang              string
	ExpiresAt         time.Time
	CreatedAt         time.Time
}

// Usa NOW() de PostgreSQL para evitar desfase de reloj entre Go y Railway
func CreatePendingRegistration(db *sql.DB, epicUsername, email, passwordHash, token, lang string) error {
	_, err := db.Exec(`
		INSERT INTO pending_registrations (id, epic_username, email, password_hash, verification_token, lang, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW() + INTERVAL '24 hours', NOW())`,
		uuid.New(), epicUsername, email, passwordHash, token, lang)
	return err
}

func GetPendingRegistration(db *sql.DB, token string) (PendingRegistration, error) {
	var p PendingRegistration
	err := db.QueryRow(`
		SELECT id, epic_username, email, password_hash, verification_token, lang, expires_at, created_at
		FROM pending_registrations
		WHERE verification_token=$1 AND expires_at > NOW()`, token).
		Scan(&p.ID, &p.EpicUsername, &p.Email, &p.PasswordHash, &p.VerificationToken, &p.Lang, &p.ExpiresAt, &p.CreatedAt)
	return p, err
}

func GetPendingRegistrationByEmail(db *sql.DB, email string) (PendingRegistration, error) {
	var p PendingRegistration
	err := db.QueryRow(`
		SELECT id, epic_username, email, password_hash, verification_token, lang, expires_at, created_at
		FROM pending_registrations
		WHERE email=$1 AND expires_at > NOW()`, email).
		Scan(&p.ID, &p.EpicUsername, &p.Email, &p.PasswordHash, &p.VerificationToken, &p.Lang, &p.ExpiresAt, &p.CreatedAt)
	return p, err
}

func UpdatePendingRegistrationToken(db *sql.DB, email, newToken string) {
	db.Exec(`UPDATE pending_registrations SET verification_token=$1, expires_at=NOW() + INTERVAL '24 hours' WHERE email=$2`,
		newToken, email)
}

func DeletePendingRegistration(db *sql.DB, token string) {
	db.Exec(`DELETE FROM pending_registrations WHERE verification_token=$1`, token)
}

func PendingRegistrationExists(db *sql.DB, email string) bool {
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM pending_registrations WHERE email=$1 AND expires_at > NOW()`, email).Scan(&count)
	return count > 0
}

// ==================== CUSTOMER ====================

func EmailExists(db *sql.DB, email string) bool {
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM customers WHERE email=$1`, email).Scan(&count)
	return count > 0
}

func EpicUsernameExists(db *sql.DB, epicUsername string) bool {
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM customers WHERE epic_username=$1`, epicUsername).Scan(&count)
	return count > 0
}

func CreateVerifiedCustomer(db *sql.DB, c types.Customer) error {
	_, err := db.Exec(`
		INSERT INTO customers (id, epic_username, email, password_hash, kc_balance, is_verified, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 0, true, NOW(), NOW())`,
		c.ID, c.EpicUsername, c.Email, c.PasswordHash)
	return err
}

func CreateCustomer(db *sql.DB, c types.Customer) error {
	_, err := db.Exec(`
		INSERT INTO customers (id, epic_username, email, password_hash, kc_balance, is_verified, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 0, false, NOW(), NOW())`,
		c.ID, c.EpicUsername, c.Email, c.PasswordHash)
	return err
}

func GetCustomerByEmail(db *sql.DB, email string) (types.Customer, error) {
	var c types.Customer
	err := db.QueryRow(`
		SELECT id, epic_username, email, password_hash, kc_balance,
		       discord_id, discord_username, is_active, is_verified, created_at, updated_at
		FROM customers WHERE email = $1 AND is_active = true`, email).
		Scan(&c.ID, &c.EpicUsername, &c.Email, &c.PasswordHash, &c.KCBalance,
			&c.DiscordID, &c.DiscordUsername, &c.IsActive, &c.IsVerified, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func GetCustomerByID(db *sql.DB, id uuid.UUID) (types.Customer, error) {
	var c types.Customer
	err := db.QueryRow(`
		SELECT id, epic_username, email, password_hash, kc_balance,
		       discord_id, discord_username, is_active, is_verified, created_at, updated_at
		FROM customers WHERE id = $1 AND is_active = true`, id).
		Scan(&c.ID, &c.EpicUsername, &c.Email, &c.PasswordHash, &c.KCBalance,
			&c.DiscordID, &c.DiscordUsername, &c.IsActive, &c.IsVerified, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func GetAllCustomers(db *sql.DB, page, limit int) ([]types.Customer, int, error) {
	if page < 1 { page = 1 }
	if limit < 1 || limit > 200 { limit = 50 }
	offset := (page - 1) * limit

	var total int
	db.QueryRow(`SELECT COUNT(*) FROM customers WHERE is_active=true`).Scan(&total)

	rows, err := db.Query(`
    SELECT id, epic_username, email, kc_balance,
           discord_id, discord_username, is_active, is_verified, created_at, updated_at
    FROM customers WHERE is_active=true ORDER BY created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil { return nil, 0, err }
	defer rows.Close()
	var customers []types.Customer
	for rows.Next() {
		var c types.Customer
		if err := rows.Scan(&c.ID, &c.EpicUsername, &c.Email, &c.KCBalance,
			&c.DiscordID, &c.DiscordUsername, &c.IsActive, &c.IsVerified, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, 0, err
		}
		customers = append(customers, c)
	}
	return customers, total, nil
}

func VerifyCustomerEmail(db *sql.DB, customerID uuid.UUID) error {
	_, err := db.Exec(`UPDATE customers SET is_verified=true, updated_at=NOW() WHERE id=$1`, customerID)
	return err
}

func LinkDiscord(db *sql.DB, customerID uuid.UUID, discordID, discordUsername string) error {
	_, err := db.Exec(`UPDATE customers SET discord_id=$1, discord_username=$2, updated_at=NOW() WHERE id=$3`,
		discordID, discordUsername, customerID)
	return err
}

func UpdateProfile(db *sql.DB, customerID uuid.UUID, epicUsername, email, passwordHash string) error {
	if epicUsername != "" {
		if _, err := db.Exec(`UPDATE customers SET epic_username=$1, updated_at=NOW() WHERE id=$2`, epicUsername, customerID); err != nil {
			return err
		}
	}
	if email != "" {
		if _, err := db.Exec(`UPDATE customers SET email=$1, updated_at=NOW() WHERE id=$2`, email, customerID); err != nil {
			return err
		}
	}
	if passwordHash != "" {
		if _, err := db.Exec(`UPDATE customers SET password_hash=$1, updated_at=NOW() WHERE id=$2`, passwordHash, customerID); err != nil {
			return err
		}
	}
	return nil
}

// ==================== EMAIL VERIFICATION ====================

// Usa NOW() de PostgreSQL para evitar desfase de reloj
func CreateEmailVerificationToken(db *sql.DB, customerID uuid.UUID, token string) error {
	db.Exec(`DELETE FROM email_verification_tokens WHERE customer_id=$1`, customerID)
	_, err := db.Exec(`
		INSERT INTO email_verification_tokens (id, customer_id, token, expires_at, created_at)
		VALUES ($1, $2, $3, NOW() + INTERVAL '24 hours', NOW())`,
		uuid.New(), customerID, token)
	return err
}

func GetEmailVerificationToken(db *sql.DB, token string) (types.EmailVerificationToken, error) {
	var t types.EmailVerificationToken
	err := db.QueryRow(`
		SELECT id, customer_id, token, expires_at, used_at, created_at
		FROM email_verification_tokens
		WHERE token=$1 AND used_at IS NULL AND expires_at > NOW()`, token).
		Scan(&t.ID, &t.CustomerID, &t.Token, &t.ExpiresAt, &t.UsedAt, &t.CreatedAt)
	return t, err
}

func MarkVerificationTokenUsed(db *sql.DB, token string) error {
	_, err := db.Exec(`UPDATE email_verification_tokens SET used_at=NOW() WHERE token=$1`, token)
	return err
}

// ==================== PASSWORD RESET ====================

// Usa NOW() de PostgreSQL para evitar desfase de reloj
func CreatePasswordResetToken(db *sql.DB, customerID uuid.UUID, token string) error {
	db.Exec(`DELETE FROM password_reset_tokens WHERE customer_id=$1`, customerID)
	_, err := db.Exec(`
		INSERT INTO password_reset_tokens (id, customer_id, token, expires_at, created_at)
		VALUES ($1, $2, $3, NOW() + INTERVAL '10 minutes', NOW())`,
		uuid.New(), customerID, token)
	return err
}

func GetPasswordResetToken(db *sql.DB, token string) (types.PasswordResetToken, error) {
	var t types.PasswordResetToken
	err := db.QueryRow(`
		SELECT id, customer_id, token, expires_at, used_at, created_at
		FROM password_reset_tokens
		WHERE token=$1 AND used_at IS NULL AND expires_at > NOW()`, token).
		Scan(&t.ID, &t.CustomerID, &t.Token, &t.ExpiresAt, &t.UsedAt, &t.CreatedAt)
	return t, err
}

func MarkResetTokenUsed(db *sql.DB, token string) error {
	_, err := db.Exec(`UPDATE password_reset_tokens SET used_at=NOW() WHERE token=$1`, token)
	return err
}

// ==================== KC — TRANSACCIONES ATÓMICAS ====================

func RechargeKC(db *sql.DB, customerID uuid.UUID, amountKC int, amountSoles *float64, note *string, approvedBy string, method string) error {
	if amountKC <= 0 { return fmt.Errorf("amount_kc must be positive") }
	if method == "" { method = "manual" }
	tx, err := db.Begin()
	if err != nil { return err }
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE customers SET kc_balance=kc_balance+$1, updated_at=NOW() WHERE id=$2 AND is_active=true`, amountKC, customerID)
	if err != nil { return err }
	rows, _ := result.RowsAffected()
	if rows == 0 { return fmt.Errorf("customer not found or inactive") }
	_, err = tx.Exec(`INSERT INTO kc_recharges (id, customer_id, amount_kc, amount_soles, method, note, approved_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())`,
		uuid.New(), customerID, amountKC, amountSoles, method, note, approvedBy)
	if err != nil { return err }
	return tx.Commit()
}

func DeductKCAndCreateOrder(db *sql.DB, customerID uuid.UUID, epicUsername string, req types.CreateOrderRequest) (types.Order, error) {
	tx, err := db.Begin()
	if err != nil { return types.Order{}, err }
	defer tx.Rollback()
	var currentBalance int
	err = tx.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1 AND is_active=true FOR UPDATE`, customerID).Scan(&currentBalance)
	if err != nil { return types.Order{}, fmt.Errorf("customer not found") }
	if currentBalance < req.PriceKC { return types.Order{}, fmt.Errorf("insufficient KC balance: have %d, need %d", currentBalance, req.PriceKC) }
	_, err = tx.Exec(`UPDATE customers SET kc_balance=kc_balance-$1, updated_at=NOW() WHERE id=$2`, req.PriceKC, customerID)
	if err != nil { return types.Order{}, err }
	orderID := uuid.New()
	var imgPtr *string
	if req.ItemImage != "" { imgPtr = &req.ItemImage }
	_, err = tx.Exec(`
		INSERT INTO orders (id, customer_id, epic_username, item_offer_id, item_name, item_image, price_kc, price_vbucks, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', NOW(), NOW())`,
		orderID, customerID, epicUsername, req.ItemOfferID, req.ItemName, imgPtr, req.PriceKC, req.PriceVBucks)
	if err != nil { return types.Order{}, err }
	if err := tx.Commit(); err != nil { return types.Order{}, err }
	return types.Order{
		ID: orderID, CustomerID: customerID, EpicUsername: epicUsername,
		ItemOfferID: req.ItemOfferID, ItemName: req.ItemName, ItemImage: imgPtr,
		PriceKC: req.PriceKC, PriceVBucks: req.PriceVBucks, Status: "pending",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, nil
}

func RefundOrder(db *sql.DB, orderID uuid.UUID) error {
	tx, err := db.Begin()
	if err != nil { return err }
	defer tx.Rollback()
	var customerID uuid.UUID
	var priceKC int
	var status string
	err = tx.QueryRow(`SELECT customer_id, price_kc, status FROM orders WHERE id=$1 FOR UPDATE`, orderID).
		Scan(&customerID, &priceKC, &status)
	if err != nil { return fmt.Errorf("order not found") }
	if status == "refunded" || status == "sent" { return fmt.Errorf("order cannot be refunded: status is %s", status) }
	tx.Exec(`UPDATE customers SET kc_balance=kc_balance+$1, updated_at=NOW() WHERE id=$2`, priceKC, customerID)
	tx.Exec(`UPDATE orders SET status='refunded', updated_at=NOW() WHERE id=$1`, orderID)
	return tx.Commit()
}

// ==================== ORDERS ====================

func GetPendingOrders(db *sql.DB) ([]types.Order, error) {
	rows, err := db.Query(`
		SELECT id, customer_id, epic_username, item_offer_id, item_name,
		       item_image, price_kc, price_vbucks, status, game_account_id, error_msg, created_at, updated_at
		FROM orders WHERE status='pending' ORDER BY created_at ASC`)
	if err != nil { return nil, err }
	defer rows.Close()
	return scanOrders(rows)
}

func UpdateOrderStatus(db *sql.DB, orderID uuid.UUID, status string, gameAccountID *uuid.UUID, errMsg *string) error {
	_, err := db.Exec(`UPDATE orders SET status=$1, game_account_id=$2, error_msg=$3, updated_at=NOW() WHERE id=$4`,
		status, gameAccountID, errMsg, orderID)
	return err
}

func GetOrdersByCustomer(db *sql.DB, customerID uuid.UUID, page, limit int) ([]types.Order, int, error) {
	if page < 1 { page = 1 }
	if limit < 1 || limit > 100 { limit = 20 }
	offset := (page - 1) * limit

	var total int
	db.QueryRow(`SELECT COUNT(*) FROM orders WHERE customer_id=$1`, customerID).Scan(&total)

	rows, err := db.Query(`
		SELECT id, customer_id, epic_username, item_offer_id, item_name,
		       item_image, price_kc, price_vbucks, status, game_account_id, error_msg, created_at, updated_at
		FROM orders WHERE customer_id=$1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`, customerID, limit, offset)
	if err != nil { return nil, 0, err }
	defer rows.Close()
	orders, err := scanOrders(rows)
	return orders, total, err
}

func GetAllOrders(db *sql.DB, page, limit int) ([]types.Order, int, error) {
	if page < 1 { page = 1 }
	if limit < 1 || limit > 200 { limit = 50 }
	offset := (page - 1) * limit

	var total int
	db.QueryRow(`SELECT COUNT(*) FROM orders`).Scan(&total)

	rows, err := db.Query(`
		SELECT id, customer_id, epic_username, item_offer_id, item_name,
		       item_image, price_kc, price_vbucks, status, game_account_id, error_msg, created_at, updated_at
		FROM orders ORDER BY created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil { return nil, 0, err }
	defer rows.Close()
	orders, err := scanOrders(rows)
	return orders, total, err
}

func scanOrders(rows *sql.Rows) ([]types.Order, error) {
	var orders []types.Order
	for rows.Next() {
		var o types.Order
		if err := rows.Scan(&o.ID, &o.CustomerID, &o.EpicUsername, &o.ItemOfferID,
			&o.ItemName, &o.ItemImage, &o.PriceKC, &o.PriceVBucks, &o.Status,
			&o.GameAccountID, &o.ErrorMsg, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		orders = append(orders, o)
	}
	return orders, nil
}

// ==================== GAME ACCOUNTS ====================

func UpsertGameAccount(db *sql.DB, a types.GameAccount, encKey string) error {
	encAccess, err := crypto.Encrypt(a.AccessToken, encKey)
	if err != nil {
		return fmt.Errorf("encrypting access token: %w", err)
	}
	encRefresh, err := crypto.Encrypt(a.RefreshToken, encKey)
	if err != nil {
		return fmt.Errorf("encrypting refresh token: %w", err)
	}
	_, err = db.Exec(`
		INSERT INTO game_accounts (id, display_name, remaining_gifts, vbucks,
			access_token, access_token_exp_date, refresh_token, refresh_token_exp_date,
			is_active, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,true,$9,NOW())
		ON CONFLICT (id) DO UPDATE SET
			display_name=EXCLUDED.display_name,
			remaining_gifts=EXCLUDED.remaining_gifts,
			vbucks=EXCLUDED.vbucks,
			access_token=EXCLUDED.access_token,
			access_token_exp_date=EXCLUDED.access_token_exp_date,
			refresh_token=EXCLUDED.refresh_token,
			refresh_token_exp_date=EXCLUDED.refresh_token_exp_date,
			is_active=true,
			updated_at=NOW()`,
		a.ID, a.DisplayName, a.RemainingGifts, a.VBucks,
		encAccess, a.AccessTokenExpDate, encRefresh, a.RefreshTokenExpDate, a.CreatedAt)
	return err
}

func GetAllGameAccounts(db *sql.DB, encKey string) ([]types.GameAccount, error) {
	rows, err := db.Query(`
		SELECT id, display_name, remaining_gifts, vbucks,
		       access_token, access_token_exp_date, refresh_token, refresh_token_exp_date,
		       is_active, created_at, updated_at
		FROM game_accounts ORDER BY created_at ASC`)
	if err != nil { return nil, err }
	defer rows.Close()
	return scanGameAccounts(rows, encKey)
}

func GetActiveGameAccounts(db *sql.DB, encKey string) ([]types.GameAccount, error) {
	rows, err := db.Query(`
		SELECT id, display_name, remaining_gifts, vbucks,
		       access_token, access_token_exp_date, refresh_token, refresh_token_exp_date,
		       is_active, created_at, updated_at
		FROM game_accounts WHERE is_active=true ORDER BY remaining_gifts DESC`)
	if err != nil { return nil, err }
	defer rows.Close()
	return scanGameAccounts(rows, encKey)
}

func scanGameAccounts(rows *sql.Rows, encKey string) ([]types.GameAccount, error) {
	var accounts []types.GameAccount
	for rows.Next() {
		var a types.GameAccount
		var encAccess, encRefresh string
		if err := rows.Scan(&a.ID, &a.DisplayName, &a.RemainingGifts, &a.VBucks,
			&encAccess, &a.AccessTokenExpDate, &encRefresh, &a.RefreshTokenExpDate,
			&a.IsActive, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		var err error
		a.AccessToken, err = crypto.Decrypt(encAccess, encKey)
		if err != nil {
			return nil, fmt.Errorf("decrypting access token for %s: %w", a.ID, err)
		}
		a.RefreshToken, err = crypto.Decrypt(encRefresh, encKey)
		if err != nil {
			return nil, fmt.Errorf("decrypting refresh token for %s: %w", a.ID, err)
		}
		accounts = append(accounts, a)
	}
	return accounts, nil
}

func UpdateRemainingGifts(db *sql.DB, accountID uuid.UUID, remaining int) error {
	_, err := db.Exec(`UPDATE game_accounts SET remaining_gifts=$1, updated_at=NOW() WHERE id=$2`, remaining, accountID)
	return err
}

func UpdateBotVbucks(db *sql.DB, accountID uuid.UUID, vbucks int) error {
	_, err := db.Exec(`UPDATE game_accounts SET vbucks=$1, updated_at=NOW() WHERE id=$2`, vbucks, accountID)
	return err
}

func DeductBotVbucks(db *sql.DB, accountID uuid.UUID, amount int) error {
	_, err := db.Exec(`UPDATE game_accounts SET vbucks=GREATEST(0,vbucks-$1), updated_at=NOW() WHERE id=$2`, amount, accountID)
	return err
}

func DeactivateGameAccount(db *sql.DB, accountID uuid.UUID) error {
	_, err := db.Exec(`UPDATE game_accounts SET is_active=false, updated_at=NOW() WHERE id=$1`, accountID)
	return err
}

func DeleteGameAccount(db *sql.DB, accountID uuid.UUID) error {
	_, err := db.Exec(`DELETE FROM game_accounts WHERE id=$1`, accountID)
	return err
}

// ==================== GAME ACCOUNT SECRETS ====================

func UpsertGameAccountSecrets(db *sql.DB, s types.GameAccountSecrets, encKey string) error {
	encDeviceID, err := crypto.Encrypt(s.DeviceID, encKey)
	if err != nil {
		return fmt.Errorf("encrypting device_id: %w", err)
	}
	encSecret, err := crypto.Encrypt(s.Secret, encKey)
	if err != nil {
		return fmt.Errorf("encrypting secret: %w", err)
	}
	_, err = db.Exec(`
		INSERT INTO game_account_secrets (id, account_id, device_id, secret, created_at)
		VALUES ($1,$2,$3,$4,NOW())
		ON CONFLICT (account_id) DO UPDATE SET device_id=EXCLUDED.device_id, secret=EXCLUDED.secret`,
		s.ID, s.AccountID, encDeviceID, encSecret)
	return err
}

func GetGameAccountSecrets(db *sql.DB, accountID uuid.UUID, encKey string) (types.GameAccountSecrets, error) {
	var s types.GameAccountSecrets
	var encDeviceID, encSecret string
	err := db.QueryRow(`SELECT id, account_id, device_id, secret, created_at FROM game_account_secrets WHERE account_id=$1`, accountID).
		Scan(&s.ID, &s.AccountID, &encDeviceID, &encSecret, &s.CreatedAt)
	if err != nil {
		return s, err
	}
	s.DeviceID, err = crypto.Decrypt(encDeviceID, encKey)
	if err != nil {
		return s, fmt.Errorf("decrypting device_id: %w", err)
	}
	s.Secret, err = crypto.Decrypt(encSecret, encKey)
	if err != nil {
		return s, fmt.Errorf("decrypting secret: %w", err)
	}
	return s, nil
}

// ==================== PAYMENT TRANSACTIONS ====================

type PaymentTransactionInput struct {
	ID          uuid.UUID
	CustomerID  uuid.UUID
	Gateway     string
	PaymentType string
	ProductID   string
	ProductName string
	AmountPEN   float64
	AmountUSD   float64
	KCAmount    int
	ExternalID  string
}

func CreatePaymentTransaction(db *sql.DB, tx PaymentTransactionInput) error {
	_, err := db.Exec(`
		INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'pending',NOW(),NOW())`,
		tx.ID, tx.CustomerID, tx.Gateway, tx.PaymentType, tx.ProductID, tx.ProductName, tx.AmountPEN, tx.AmountUSD, tx.KCAmount, tx.ExternalID)
	return err
}

func GetPaymentTransaction(db *sql.DB, id uuid.UUID) (types.PaymentTransaction, error) {
	var t types.PaymentTransaction
	err := db.QueryRow(`
		SELECT id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, COALESCE(activation_code,''), COALESCE(autobuyer_task_id,''), created_at, updated_at
		FROM payment_transactions WHERE id=$1`, id).
		Scan(&t.ID, &t.CustomerID, &t.Gateway, &t.PaymentType, &t.ProductID, &t.ProductName, &t.AmountPEN, &t.AmountUSD, &t.KCAmount, &t.ExternalID, &t.Status, &t.ActivationCode, &t.AutobuyerTaskID, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

func GetPaymentTransactionByExternalID(db *sql.DB, externalID string) (types.PaymentTransaction, error) {
	var t types.PaymentTransaction
	err := db.QueryRow(`
		SELECT id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, COALESCE(activation_code,''), COALESCE(autobuyer_task_id,''), created_at, updated_at
		FROM payment_transactions WHERE external_id=$1`, externalID).
		Scan(&t.ID, &t.CustomerID, &t.Gateway, &t.PaymentType, &t.ProductID, &t.ProductName, &t.AmountPEN, &t.AmountUSD, &t.KCAmount, &t.ExternalID, &t.Status, &t.ActivationCode, &t.AutobuyerTaskID, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

func SetActivationCode(db *sql.DB, id uuid.UUID, code string) error {
	_, err := db.Exec(`UPDATE payment_transactions SET activation_code=$1, updated_at=NOW() WHERE id=$2`, code, id)
	return err
}

func GetPaymentByActivationCode(db *sql.DB, code string) (types.PaymentTransaction, error) {
	var t types.PaymentTransaction
	err := db.QueryRow(`
		SELECT id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, COALESCE(activation_code,''), COALESCE(autobuyer_task_id,''), created_at, updated_at
		FROM payment_transactions WHERE activation_code=$1`, code).
		Scan(&t.ID, &t.CustomerID, &t.Gateway, &t.PaymentType, &t.ProductID, &t.ProductName, &t.AmountPEN, &t.AmountUSD, &t.KCAmount, &t.ExternalID, &t.Status, &t.ActivationCode, &t.AutobuyerTaskID, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

func UpdatePaymentActivation(db *sql.DB, id uuid.UUID, taskID, status string) error {
	_, err := db.Exec(`UPDATE payment_transactions SET autobuyer_task_id=$1, status=$2, updated_at=NOW() WHERE id=$3`, taskID, status, id)
	return err
}

func UpdatePaymentProgress(db *sql.DB, taskID, progress string) error {
	_, err := db.Exec(`UPDATE payment_transactions SET progress=$1, updated_at=NOW() WHERE autobuyer_task_id=$2`, progress, taskID)
	return err
}

func GetPaymentProgress(db *sql.DB, taskID string) string {
	var progress sql.NullString
	db.QueryRow(`SELECT progress FROM payment_transactions WHERE autobuyer_task_id=$1`, taskID).Scan(&progress)
	if progress.Valid { return progress.String }
	return ""
}

func UpdatePaymentStatus(db *sql.DB, id uuid.UUID, status string, externalID string) error {
	_, err := db.Exec(`UPDATE payment_transactions SET status=$1, external_id=$2, updated_at=NOW() WHERE id=$3`,
		status, externalID, id)
	return err
}

func GetAllPaymentTransactions(db *sql.DB, page, limit int) ([]types.PaymentTransaction, int, error) {
	if page < 1 { page = 1 }
	if limit < 1 || limit > 200 { limit = 50 }
	offset := (page - 1) * limit
	var total int
	db.QueryRow(`SELECT COUNT(*) FROM payment_transactions`).Scan(&total)
	rows, err := db.Query(`
		SELECT id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, COALESCE(activation_code,''), COALESCE(autobuyer_task_id,''), created_at, updated_at
		FROM payment_transactions ORDER BY created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil { return nil, 0, err }
	defer rows.Close()
	var txs []types.PaymentTransaction
	for rows.Next() {
		var t types.PaymentTransaction
		if err := rows.Scan(&t.ID, &t.CustomerID, &t.Gateway, &t.PaymentType, &t.ProductID, &t.ProductName, &t.AmountPEN, &t.AmountUSD, &t.KCAmount, &t.ExternalID, &t.Status, &t.ActivationCode, &t.AutobuyerTaskID, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, 0, err
		}
		txs = append(txs, t)
	}
	return txs, total, nil
}

func GetPaymentsByCustomer(db *sql.DB, customerID uuid.UUID) ([]types.PaymentTransaction, error) {
	rows, err := db.Query(`
		SELECT id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, external_id, status, COALESCE(activation_code,''), COALESCE(autobuyer_task_id,''), created_at, updated_at
		FROM payment_transactions WHERE customer_id=$1 ORDER BY created_at DESC LIMIT 50`, customerID)
	if err != nil { return nil, err }
	defer rows.Close()
	var txs []types.PaymentTransaction
	for rows.Next() {
		var t types.PaymentTransaction
		if err := rows.Scan(&t.ID, &t.CustomerID, &t.Gateway, &t.PaymentType, &t.ProductID, &t.ProductName, &t.AmountPEN, &t.AmountUSD, &t.KCAmount, &t.ExternalID, &t.Status, &t.ActivationCode, &t.AutobuyerTaskID, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		txs = append(txs, t)
	}
	return txs, nil
}

// ==================== REFRESH TOKENS ====================

func CreateRefreshToken(db *sql.DB, customerID uuid.UUID, tokenHash string, expiresAt time.Time) error {
	_, err := db.Exec(`
		INSERT INTO refresh_tokens (id, customer_id, token_hash, expires_at, created_at)
		VALUES ($1, $2, $3, $4, NOW())`,
		uuid.New(), customerID, tokenHash, expiresAt)
	return err
}

func GetRefreshToken(db *sql.DB, tokenHash string) (types.RefreshToken, error) {
	var t types.RefreshToken
	err := db.QueryRow(`
		SELECT id, customer_id, token_hash, expires_at, created_at
		FROM refresh_tokens
		WHERE token_hash=$1 AND expires_at > NOW()`, tokenHash).
		Scan(&t.ID, &t.CustomerID, &t.TokenHash, &t.ExpiresAt, &t.CreatedAt)
	return t, err
}

func DeleteRefreshToken(db *sql.DB, tokenHash string) error {
	_, err := db.Exec(`DELETE FROM refresh_tokens WHERE token_hash=$1`, tokenHash)
	return err
}

func DeleteCustomerRefreshTokens(db *sql.DB, customerID uuid.UUID) error {
	_, err := db.Exec(`DELETE FROM refresh_tokens WHERE customer_id=$1`, customerID)
	return err
}

// ==================== AUDIT LOG ====================

func AddAuditLog(db *sql.DB, customerID *uuid.UUID, action, details, ip string) {
	go func() {
		db.Exec(`INSERT INTO audit_logs (id, customer_id, action, details, ip_address, created_at) VALUES ($1,$2,$3,$4,$5,NOW())`,
			uuid.New(), customerID, action, details, ip)
	}()
}

// ==================== KC RECHARGES ====================

func GetRechargesByCustomer(db *sql.DB, customerID uuid.UUID) ([]types.KCRecharge, error) {
	rows, err := db.Query(`
		SELECT id, customer_id, amount_kc, amount_soles, method, note, approved_by, created_at
		FROM kc_recharges WHERE customer_id=$1 ORDER BY created_at DESC`, customerID)
	if err != nil { return nil, err }
	defer rows.Close()
	var recharges []types.KCRecharge
	for rows.Next() {
		var r types.KCRecharge
		if err := rows.Scan(&r.ID, &r.CustomerID, &r.AmountKC, &r.AmountSoles,
			&r.Method, &r.Note, &r.ApprovedBy, &r.CreatedAt); err != nil {
			return nil, err
		}
		recharges = append(recharges, r)
	}
	return recharges, nil
}

func GetCustomerByDiscordID(db *sql.DB, discordID string) (types.Customer, error) {
	var c types.Customer
	err := db.QueryRow(`
		SELECT id, epic_username, email, password_hash, kc_balance,
		       discord_id, discord_username, is_active, is_verified, created_at, updated_at
		FROM customers WHERE discord_id = $1 AND is_active = true`, discordID).
		Scan(&c.ID, &c.EpicUsername, &c.Email, &c.PasswordHash, &c.KCBalance,
			&c.DiscordID, &c.DiscordUsername, &c.IsActive, &c.IsVerified, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// ==================== DISCORD LANG ====================

func GetDiscordLang(db *sql.DB, discordID string) (string, error) {
	var lang string
	err := db.QueryRow(`SELECT discord_lang FROM customers WHERE discord_id = $1`, discordID).Scan(&lang)
	return lang, err
}

func SetDiscordLang(db *sql.DB, discordID string, lang string) {
	db.Exec(`UPDATE customers SET discord_lang = $1 WHERE discord_id = $2`, lang, discordID)
}

// ==================== BOT CONFIG ====================

func GetBotPrefix(db *sql.DB) (string, error) {
	var prefix string
	err := db.QueryRow(`SELECT value FROM bot_config WHERE key = 'prefix'`).Scan(&prefix)
	return prefix, err
}

func SetBotPrefix(db *sql.DB, prefix string) {
	db.Exec(`INSERT INTO bot_config (key, value) VALUES ('prefix', $1) ON CONFLICT (key) DO UPDATE SET value = $1`, prefix)
}

func GetBotAdmins(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT value FROM bot_config WHERE key = 'admin'`)
	if err != nil { return nil, err }
	defer rows.Close()
	var admins []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err == nil { admins = append(admins, v) }
	}
	return admins, nil
}

func AddBotAdmin(db *sql.DB, discordID string) {
	db.Exec(`INSERT INTO bot_config (key, value) VALUES ('admin', $1) ON CONFLICT DO NOTHING`, discordID)
}

func DeductKCAdmin(db *sql.DB, customerID uuid.UUID, amount int, note string) error {
	tx, err := db.Begin()
	if err != nil { return err }
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE customers SET kc_balance=kc_balance-$1, updated_at=NOW() WHERE id=$2 AND is_active=true AND kc_balance >= $1`, amount, customerID)
	if err != nil { return err }
	rows, _ := result.RowsAffected()
	if rows == 0 { return fmt.Errorf("saldo insuficiente o cliente no encontrado") }
	tx.Exec(`INSERT INTO kc_recharges (id, customer_id, amount_kc, method, note, approved_by, created_at) VALUES ($1,$2,$3,'admin_deduct',$4,'discord-admin',NOW())`,
		uuid.New(), customerID, -amount, note)
	return tx.Commit()
}

func UnlinkDiscord(db *sql.DB, customerID uuid.UUID) error {
	_, err := db.Exec(`UPDATE customers SET discord_id=NULL, discord_username=NULL, discord_lang='es', updated_at=NOW() WHERE id=$1`, customerID)
	return err
}

func CountPendingOrdersByCustomer(db *sql.DB, customerID uuid.UUID) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM orders WHERE customer_id=$1 AND status IN ('pending','processing')`, customerID).Scan(&count)
	return count, err
}
