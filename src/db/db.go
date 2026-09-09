package db

import (
	"KidStoreStore/src/crypto"
	"KidStoreStore/src/safe"
	"KidStoreStore/src/types"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
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
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='orders' AND column_name='delivery_evidence') THEN
				ALTER TABLE orders ADD COLUMN delivery_evidence TEXT;
			END IF;
		END $$`,
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
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='game_accounts' AND column_name='last_gift_reset_date') THEN
				ALTER TABLE game_accounts ADD COLUMN last_gift_reset_date DATE NOT NULL DEFAULT CURRENT_DATE;
			END IF;
		END $$`,
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
		`CREATE TABLE IF NOT EXISTS discord_user_prefs (
			discord_id VARCHAR(30) PRIMARY KEY,
			lang       VARCHAR(2) NOT NULL DEFAULT 'es'
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
		// Add is_admin column to customers
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='customers' AND column_name='is_admin') THEN
				ALTER TABLE customers ADD COLUMN is_admin BOOLEAN NOT NULL DEFAULT false;
			END IF;
		END $$`,
		`DELETE FROM pending_registrations WHERE expires_at < NOW()`,
		`DELETE FROM refresh_tokens WHERE expires_at < NOW()`,
		// Add google_id column to customers (OAuth login)
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='customers' AND column_name='google_id') THEN
				ALTER TABLE customers ADD COLUMN google_id VARCHAR(255) UNIQUE;
			END IF;
		END $$`,
		`CREATE TABLE IF NOT EXISTS pending_oauth_registrations (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			provider VARCHAR(20) NOT NULL,
			provider_id VARCHAR(255) NOT NULL,
			email VARCHAR(255),
			display_name VARCHAR(255),
			token VARCHAR(255) NOT NULL UNIQUE,
			expires_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT NOW(),
			UNIQUE(provider, provider_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pending_oauth_token ON pending_oauth_registrations(token)`,
		`DELETE FROM pending_oauth_registrations WHERE expires_at < NOW()`,
		// Perfil ampliado: avatar, telefono, control de cambio de email, y si
		// el cliente tiene una contrasena real (falso para cuentas creadas por OAuth)
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='customers' AND column_name='avatar_url') THEN
				ALTER TABLE customers ADD COLUMN avatar_url TEXT;
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='customers' AND column_name='phone') THEN
				ALTER TABLE customers ADD COLUMN phone VARCHAR(30);
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='customers' AND column_name='email_changed_at') THEN
				ALTER TABLE customers ADD COLUMN email_changed_at TIMESTAMP;
			END IF;
		END $$`,
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='customers' AND column_name='has_password') THEN
				ALTER TABLE customers ADD COLUMN has_password BOOLEAN NOT NULL DEFAULT true;
			END IF;
		END $$`,
		// Verificacion 2FA/OTP por correo para confirmar el cambio de email
		`CREATE TABLE IF NOT EXISTS email_change_requests (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
			new_email VARCHAR(255) NOT NULL,
			code_hash VARCHAR(255) NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			expires_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_email_change_customer ON email_change_requests(customer_id)`,
		`DELETE FROM email_change_requests WHERE expires_at < NOW()`,
		// Historial de tiradas del /slot de Discord (auditoria)
		`CREATE TABLE IF NOT EXISTS slot_plays (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
			bet_amount INTEGER NOT NULL,
			won BOOLEAN NOT NULL,
			payout_amount INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_slot_plays_customer ON slot_plays(customer_id)`,
		// dLocal Go cobra en la divisa real del cliente (no solo PEN/USD) —
		// se guarda aparte para no romper el significado de amount_pen/amount_usd
		// que usan las demas pasarelas.
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='payment_transactions' AND column_name='currency_code') THEN
				ALTER TABLE payment_transactions ADD COLUMN currency_code VARCHAR(10);
				ALTER TABLE payment_transactions ADD COLUMN amount_local NUMERIC(14,2);
			END IF;
		END $$`,
		// Evita repetir el aviso de "ya cumpliste 48h de amistad" para el mismo par cliente-bot
		`CREATE TABLE IF NOT EXISTS friendship_48h_notified (
			customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
			bot_id UUID NOT NULL REFERENCES game_accounts(id) ON DELETE CASCADE,
			notified_at TIMESTAMP NOT NULL DEFAULT NOW(),
			PRIMARY KEY (customer_id, bot_id)
		)`,
		// Libro de Reclamaciones Virtual — requisito legal para negocios de
		// e-commerce en Perú (Código de Protección y Defensa del Consumidor,
		// Ley N° 29571). No requiere que el cliente esté logueado: cualquier
		// consumidor debe poder presentar un reclamo o queja.
		`CREATE TABLE IF NOT EXISTS consumer_complaints (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			reference VARCHAR(20) NOT NULL UNIQUE,
			kind VARCHAR(20) NOT NULL CHECK (kind IN ('reclamo','queja')),
			full_name VARCHAR(255) NOT NULL,
			document_type VARCHAR(20) NOT NULL,
			document_number VARCHAR(30) NOT NULL,
			email VARCHAR(255) NOT NULL,
			phone VARCHAR(30),
			address TEXT,
			is_minor BOOLEAN NOT NULL DEFAULT false,
			guardian_name VARCHAR(255),
			order_id UUID,
			amount_involved NUMERIC(10,2),
			product_description TEXT NOT NULL,
			detail TEXT NOT NULL,
			consumer_request TEXT NOT NULL,
			status VARCHAR(20) NOT NULL DEFAULT 'pendiente'
				CHECK (status IN ('pendiente','respondido','cerrado')),
			admin_response TEXT,
			responded_at TIMESTAMP,
			ip_address VARCHAR(45),
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_complaints_reference ON consumer_complaints(reference)`,
		`CREATE INDEX IF NOT EXISTS idx_complaints_status ON consumer_complaints(status)`,
		`CREATE INDEX IF NOT EXISTS idx_complaints_email ON consumer_complaints(email)`,
		// 2FA (TOTP) para cuentas admin — el secreto se guarda cifrado (igual
		// que los tokens de las cuentas bot) y totp_enabled solo pasa a true
		// tras confirmar un código real durante la activación.
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='customers' AND column_name='totp_secret_enc') THEN
				ALTER TABLE customers ADD COLUMN totp_secret_enc TEXT;
				ALTER TABLE customers ADD COLUMN totp_enabled BOOLEAN NOT NULL DEFAULT false;
			END IF;
		END $$`,
		// totp_pending_secret_enc: secreto de un intento de activación/
		// sustitución de 2FA todavía SIN confirmar — separado a propósito de
		// totp_secret_enc (el que de verdad protege el login). Mientras no se
		// confirme con un código real, el secreto ACTIVO no se toca — así,
		// alguien con solo un JWT robado (sin la contraseña) no puede
		// desactivar el 2FA de una cuenta llamando a /2fa/setup.
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='customers' AND column_name='totp_pending_secret_enc') THEN
				ALTER TABLE customers ADD COLUMN totp_pending_secret_enc TEXT;
			END IF;
		END $$`,
		`CREATE TABLE IF NOT EXISTS admin_backup_codes (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
			code_hash VARCHAR(255) NOT NULL,
			used_at TIMESTAMP,
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_backup_codes_customer ON admin_backup_codes(customer_id)`,
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

// dailyGiftLimit — cuántos regalos puede enviar cada cuenta bot por día.
// Debe coincidir con el DEFAULT de remaining_gifts en game_accounts y con el
// valor que se le asigna a una cuenta recién vinculada (ver fortnite.go).
const dailyGiftLimit = 5

// ResetDailyGifts repone remaining_gifts=5 en todas las cuentas bot cuyo
// último reseteo fue en un día anterior al de hoy (según la zona horaria
// configurada en el horario de bots) — esto es lo que hace real la promesa
// "los gifts se resetean diariamente" que se muestra en toda la web y en los
// mensajes de error del worker de pedidos. Antes de este fix, remaining_gifts
// nunca se reponía: una vez que una cuenta llegaba a 0 se quedaba así para
// siempre. Es una sola consulta UPDATE, segura de correr con la frecuencia
// que sea — solo toca las filas cuya fecha ya quedó vieja.
func ResetDailyGifts(db *sql.DB) (int64, error) {
	schedule, err := GetBotSchedule(db)
	timezone := "America/Lima"
	if err == nil && schedule.Timezone != "" {
		timezone = schedule.Timezone
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	today := time.Now().In(loc).Format("2006-01-02")

	result, err := db.Exec(`
		UPDATE game_accounts
		SET remaining_gifts=$1, last_gift_reset_date=$2, updated_at=NOW()
		WHERE last_gift_reset_date < $2`,
		dailyGiftLimit, today)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
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

// UpdatePendingRegistrationToken reemplaza TODO el registro pendiente de un
// email — no solo el token. Si alguien ya había dejado un registro
// pendiente sin verificar para este correo (a propósito, con datos
// ajenos — un usuario Epic o contraseña que no son los tuyos — o
// simplemente porque lo abandonó a medias), y el dueño real del correo
// intenta registrarse de nuevo, el intento MÁS RECIENTE debe ganar por
// completo: usuario Epic, contraseña e idioma nuevos, no solo un token
// nuevo sobre datos viejos. Sin esto, el correo de verificación le llega al
// dueño real del correo, pero la cuenta que activa terminaría teniendo el
// usuario/contraseña que haya elegido quien se registró primero — nunca
// hay que asumir que el primer intento es el legítimo.
func UpdatePendingRegistrationToken(db *sql.DB, epicUsername, email, passwordHash, newToken, lang string) {
	db.Exec(`UPDATE pending_registrations
		SET epic_username=$1, password_hash=$2, verification_token=$3, lang=$4, expires_at=NOW() + INTERVAL '24 hours'
		WHERE email=$5`,
		epicUsername, passwordHash, newToken, lang, email)
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

// GetCustomerByEpicUsername busca por usuario Epic sin distinguir mayúsculas/minúsculas.
func GetCustomerByEpicUsername(db *sql.DB, epicUsername string) (types.Customer, error) {
	var c types.Customer
	err := db.QueryRow(`
		SELECT id, epic_username, email, password_hash, kc_balance,
		       google_id, discord_id, discord_username, avatar_url, phone, has_password, email_changed_at,
		       is_active, is_verified, COALESCE(is_admin,false), totp_secret_enc, totp_enabled, totp_pending_secret_enc, created_at, updated_at
		FROM customers WHERE LOWER(epic_username) = LOWER($1) AND is_active = true`, epicUsername).
		Scan(&c.ID, &c.EpicUsername, &c.Email, &c.PasswordHash, &c.KCBalance,
			&c.GoogleID, &c.DiscordID, &c.DiscordUsername, &c.AvatarURL, &c.Phone, &c.HasPassword, &c.EmailChangedAt,
			&c.IsActive, &c.IsVerified, &c.IsAdmin, &c.TOTPSecretEnc, &c.TOTPEnabled, &c.TOTPPendingSecretEnc, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

// HasBeenNotified48h indica si ya se le avisó a este cliente que cumplió 48h
// de amistad con este bot (para no repetir el aviso en cada ciclo del worker).
func HasBeenNotified48h(db *sql.DB, customerID uuid.UUID, botID uuid.UUID) bool {
	var count int
	db.QueryRow(`SELECT COUNT(*) FROM friendship_48h_notified WHERE customer_id=$1 AND bot_id=$2`, customerID, botID).Scan(&count)
	return count > 0
}

func MarkNotified48h(db *sql.DB, customerID uuid.UUID, botID uuid.UUID) error {
	_, err := db.Exec(`
		INSERT INTO friendship_48h_notified (customer_id, bot_id, notified_at)
		VALUES ($1, $2, NOW()) ON CONFLICT (customer_id, bot_id) DO NOTHING`, customerID, botID)
	return err
}

func CreateVerifiedCustomer(db *sql.DB, c types.Customer) error {
	_, err := db.Exec(`
		INSERT INTO customers (id, epic_username, email, password_hash, kc_balance, is_verified, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 0, true, NOW(), NOW())`,
		c.ID, c.EpicUsername, c.Email, c.PasswordHash)
	return err
}

func GetCustomerByEmail(db *sql.DB, email string) (types.Customer, error) {
	var c types.Customer
	err := db.QueryRow(`
		SELECT id, epic_username, email, password_hash, kc_balance,
		       google_id, discord_id, discord_username, avatar_url, phone, has_password, email_changed_at,
		       is_active, is_verified, COALESCE(is_admin,false), totp_secret_enc, totp_enabled, totp_pending_secret_enc, created_at, updated_at
		FROM customers WHERE email = $1 AND is_active = true`, email).
		Scan(&c.ID, &c.EpicUsername, &c.Email, &c.PasswordHash, &c.KCBalance,
			&c.GoogleID, &c.DiscordID, &c.DiscordUsername, &c.AvatarURL, &c.Phone, &c.HasPassword, &c.EmailChangedAt,
			&c.IsActive, &c.IsVerified, &c.IsAdmin, &c.TOTPSecretEnc, &c.TOTPEnabled, &c.TOTPPendingSecretEnc, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func GetCustomerByID(db *sql.DB, id uuid.UUID) (types.Customer, error) {
	var c types.Customer
	err := db.QueryRow(`
		SELECT id, epic_username, email, password_hash, kc_balance,
		       google_id, discord_id, discord_username, avatar_url, phone, has_password, email_changed_at,
		       is_active, is_verified, COALESCE(is_admin,false), totp_secret_enc, totp_enabled, totp_pending_secret_enc, created_at, updated_at
		FROM customers WHERE id = $1 AND is_active = true`, id).
		Scan(&c.ID, &c.EpicUsername, &c.Email, &c.PasswordHash, &c.KCBalance,
			&c.GoogleID, &c.DiscordID, &c.DiscordUsername, &c.AvatarURL, &c.Phone, &c.HasPassword, &c.EmailChangedAt,
			&c.IsActive, &c.IsVerified, &c.IsAdmin, &c.TOTPSecretEnc, &c.TOTPEnabled, &c.TOTPPendingSecretEnc, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func GetCustomerByGoogleID(db *sql.DB, googleID string) (types.Customer, error) {
	var c types.Customer
	err := db.QueryRow(`
		SELECT id, epic_username, email, password_hash, kc_balance,
		       google_id, discord_id, discord_username, avatar_url, phone, has_password, email_changed_at,
		       is_active, is_verified, COALESCE(is_admin,false), totp_secret_enc, totp_enabled, totp_pending_secret_enc, created_at, updated_at
		FROM customers WHERE google_id = $1 AND is_active = true`, googleID).
		Scan(&c.ID, &c.EpicUsername, &c.Email, &c.PasswordHash, &c.KCBalance,
			&c.GoogleID, &c.DiscordID, &c.DiscordUsername, &c.AvatarURL, &c.Phone, &c.HasPassword, &c.EmailChangedAt,
			&c.IsActive, &c.IsVerified, &c.IsAdmin, &c.TOTPSecretEnc, &c.TOTPEnabled, &c.TOTPPendingSecretEnc, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func GetCustomerByDiscordID(db *sql.DB, discordID string) (types.Customer, error) {
	var c types.Customer
	err := db.QueryRow(`
		SELECT id, epic_username, email, password_hash, kc_balance,
		       google_id, discord_id, discord_username, avatar_url, phone, has_password, email_changed_at,
		       is_active, is_verified, COALESCE(is_admin,false), totp_secret_enc, totp_enabled, totp_pending_secret_enc, created_at, updated_at
		FROM customers WHERE discord_id = $1 AND is_active = true`, discordID).
		Scan(&c.ID, &c.EpicUsername, &c.Email, &c.PasswordHash, &c.KCBalance,
			&c.GoogleID, &c.DiscordID, &c.DiscordUsername, &c.AvatarURL, &c.Phone, &c.HasPassword, &c.EmailChangedAt,
			&c.IsActive, &c.IsVerified, &c.IsAdmin, &c.TOTPSecretEnc, &c.TOTPEnabled, &c.TOTPPendingSecretEnc, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func LinkGoogleID(db *sql.DB, customerID uuid.UUID, googleID string) error {
	_, err := db.Exec(`UPDATE customers SET google_id=$1, updated_at=NOW() WHERE id=$2`, googleID, customerID)
	return err
}

func LinkDiscordID(db *sql.DB, customerID uuid.UUID, discordID, discordUsername string) error {
	_, err := db.Exec(`UPDATE customers SET discord_id=$1, discord_username=$2, updated_at=NOW() WHERE id=$3`,
		discordID, discordUsername, customerID)
	return err
}

func UnlinkGoogleID(db *sql.DB, customerID uuid.UUID) error {
	_, err := db.Exec(`UPDATE customers SET google_id=NULL, updated_at=NOW() WHERE id=$1`, customerID)
	return err
}

func UnlinkDiscordID(db *sql.DB, customerID uuid.UUID) error {
	_, err := db.Exec(`UPDATE customers SET discord_id=NULL, discord_username=NULL, updated_at=NOW() WHERE id=$1`, customerID)
	return err
}

func GetAllCustomers(db *sql.DB, page, limit int) ([]types.Customer, int, error) {
	if page < 1 { page = 1 }
	if limit < 1 || limit > 200 { limit = 50 }
	offset := (page - 1) * limit

	var total int
	db.QueryRow(`SELECT COUNT(*) FROM customers WHERE is_active=true`).Scan(&total)

	rows, err := db.Query(`
    SELECT id, epic_username, email, kc_balance,
           google_id, discord_id, discord_username, avatar_url, phone, has_password, email_changed_at,
           is_active, is_verified, COALESCE(is_admin,false), totp_secret_enc, totp_enabled, totp_pending_secret_enc, created_at, updated_at
    FROM customers WHERE is_active=true ORDER BY created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil { return nil, 0, err }
	defer rows.Close()
	var customers []types.Customer
	for rows.Next() {
		var c types.Customer
		if err := rows.Scan(&c.ID, &c.EpicUsername, &c.Email, &c.KCBalance,
			&c.GoogleID, &c.DiscordID, &c.DiscordUsername, &c.AvatarURL, &c.Phone, &c.HasPassword, &c.EmailChangedAt,
			&c.IsActive, &c.IsVerified, &c.IsAdmin, &c.TOTPSecretEnc, &c.TOTPEnabled, &c.TOTPPendingSecretEnc, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, 0, err
		}
		customers = append(customers, c)
	}
	return customers, total, nil
}

func SetAvatar(db *sql.DB, customerID uuid.UUID, avatarURL string) error {
	_, err := db.Exec(`UPDATE customers SET avatar_url=$1, updated_at=NOW() WHERE id=$2`, avatarURL, customerID)
	return err
}

func VerifyCustomerEmail(db *sql.DB, customerID uuid.UUID) error {
	_, err := db.Exec(`UPDATE customers SET is_verified=true, updated_at=NOW() WHERE id=$1`, customerID)
	return err
}

// ==================== OAUTH REGISTRATION (Google / Discord) ====================

type PendingOAuthRegistration struct {
	ID          uuid.UUID
	Provider    string
	ProviderID  string
	Email       *string
	DisplayName *string
	Token       string
	ExpiresAt   time.Time
	CreatedAt   time.Time
}

// CreatePendingOAuthRegistration guarda (o renueva) el registro pendiente de un
// login OAuth de una cuenta nueva. Si el proveedor+provider_id ya tenia un
// registro pendiente (el usuario cerro la pantalla sin terminar), se reemplaza
// el token para que el enlace anterior deje de servir.
func CreatePendingOAuthRegistration(db *sql.DB, provider, providerID string, email, displayName *string, token string) error {
	_, err := db.Exec(`
		INSERT INTO pending_oauth_registrations (id, provider, provider_id, email, display_name, token, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW() + INTERVAL '30 minutes', NOW())
		ON CONFLICT (provider, provider_id) DO UPDATE SET
			email=EXCLUDED.email, display_name=EXCLUDED.display_name,
			token=EXCLUDED.token, expires_at=EXCLUDED.expires_at, created_at=NOW()`,
		uuid.New(), provider, providerID, email, displayName, token)
	return err
}

func GetPendingOAuthRegistration(db *sql.DB, token string) (PendingOAuthRegistration, error) {
	var p PendingOAuthRegistration
	err := db.QueryRow(`
		SELECT id, provider, provider_id, email, display_name, token, expires_at, created_at
		FROM pending_oauth_registrations
		WHERE token=$1 AND expires_at > NOW()`, token).
		Scan(&p.ID, &p.Provider, &p.ProviderID, &p.Email, &p.DisplayName, &p.Token, &p.ExpiresAt, &p.CreatedAt)
	return p, err
}

func DeletePendingOAuthRegistration(db *sql.DB, token string) {
	db.Exec(`DELETE FROM pending_oauth_registrations WHERE token=$1`, token)
}

// CreateOAuthCustomer crea una cuenta ya verificada (el proveedor OAuth ya
// confirmo el correo) con una contrasena aleatoria inutilizable — el cliente
// solo puede entrar via el proveedor OAuth hasta que configure una contrasena
// propia desde su perfil.
func CreateOAuthCustomer(db *sql.DB, epicUsername string, email *string, randomPasswordHash string, provider, providerID, displayName string) (types.Customer, error) {
	customerID := uuid.New()
	var err error
	switch provider {
	case "google":
		_, err = db.Exec(`
			INSERT INTO customers (id, epic_username, email, password_hash, kc_balance, google_id, has_password, is_verified, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 0, $5, false, true, NOW(), NOW())`,
			customerID, epicUsername, email, randomPasswordHash, providerID)
	case "discord":
		_, err = db.Exec(`
			INSERT INTO customers (id, epic_username, email, password_hash, kc_balance, discord_id, discord_username, has_password, is_verified, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 0, $5, $6, false, true, NOW(), NOW())`,
			customerID, epicUsername, email, randomPasswordHash, providerID, displayName)
	default:
		return types.Customer{}, fmt.Errorf("proveedor OAuth desconocido: %s", provider)
	}
	if err != nil {
		return types.Customer{}, err
	}
	return GetCustomerByID(db, customerID)
}

func UpdateProfile(db *sql.DB, customerID uuid.UUID, epicUsername, passwordHash string, phone *string) error {
	if epicUsername != "" {
		if _, err := db.Exec(`UPDATE customers SET epic_username=$1, updated_at=NOW() WHERE id=$2`, epicUsername, customerID); err != nil {
			return err
		}
	}
	if passwordHash != "" {
		if _, err := db.Exec(`UPDATE customers SET password_hash=$1, has_password=true, updated_at=NOW() WHERE id=$2`, passwordHash, customerID); err != nil {
			return err
		}
	}
	if phone != nil {
		if _, err := db.Exec(`UPDATE customers SET phone=$1, updated_at=NOW() WHERE id=$2`, *phone, customerID); err != nil {
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

// ==================== CAMBIO DE EMAIL (2FA / OTP) ====================

// Reemplaza cualquier solicitud pendiente del cliente por una nueva
func CreateEmailChangeRequest(db *sql.DB, customerID uuid.UUID, newEmail, codeHash string) error {
	db.Exec(`DELETE FROM email_change_requests WHERE customer_id=$1`, customerID)
	_, err := db.Exec(`
		INSERT INTO email_change_requests (id, customer_id, new_email, code_hash, expires_at, created_at)
		VALUES ($1, $2, $3, $4, NOW() + INTERVAL '15 minutes', NOW())`,
		uuid.New(), customerID, newEmail, codeHash)
	return err
}

func GetEmailChangeRequest(db *sql.DB, customerID uuid.UUID) (types.EmailChangeRequest, error) {
	var r types.EmailChangeRequest
	err := db.QueryRow(`
		SELECT id, customer_id, new_email, code_hash, attempts, expires_at, created_at
		FROM email_change_requests
		WHERE customer_id=$1 AND expires_at > NOW()`, customerID).
		Scan(&r.ID, &r.CustomerID, &r.NewEmail, &r.CodeHash, &r.Attempts, &r.ExpiresAt, &r.CreatedAt)
	return r, err
}

func IncrementEmailChangeAttempts(db *sql.DB, id uuid.UUID) error {
	_, err := db.Exec(`UPDATE email_change_requests SET attempts=attempts+1 WHERE id=$1`, id)
	return err
}

func DeleteEmailChangeRequest(db *sql.DB, customerID uuid.UUID) error {
	_, err := db.Exec(`DELETE FROM email_change_requests WHERE customer_id=$1`, customerID)
	return err
}

func ConfirmEmailChange(db *sql.DB, customerID uuid.UUID, newEmail string) error {
	_, err := db.Exec(`UPDATE customers SET email=$1, email_changed_at=NOW(), updated_at=NOW() WHERE id=$2`, newEmail, customerID)
	return err
}

// ==================== KC — TRANSACCIONES ATÓMICAS ====================

// RechargeKC devuelve el ID de la fila creada en kc_recharges — lo usan las
// recargas manuales (admin/Discord) para armar el link al comprobante.
func RechargeKC(db *sql.DB, customerID uuid.UUID, amountKC int, amountSoles *float64, note *string, approvedBy string, method string) (uuid.UUID, error) {
	if amountKC <= 0 { return uuid.Nil, fmt.Errorf("amount_kc must be positive") }
	if method == "" { method = "manual" }
	tx, err := db.Begin()
	if err != nil { return uuid.Nil, err }
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE customers SET kc_balance=kc_balance+$1, updated_at=NOW() WHERE id=$2 AND is_active=true`, amountKC, customerID)
	if err != nil { return uuid.Nil, err }
	rows, _ := result.RowsAffected()
	if rows == 0 { return uuid.Nil, fmt.Errorf("customer not found or inactive") }
	rechargeID := uuid.New()
	_, err = tx.Exec(`INSERT INTO kc_recharges (id, customer_id, amount_kc, amount_soles, method, note, approved_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())`,
		rechargeID, customerID, amountKC, amountSoles, method, note, approvedBy)
	if err != nil { return uuid.Nil, err }
	if err := tx.Commit(); err != nil { return uuid.Nil, err }
	return rechargeID, nil
}

// DeductKCManual quita KC del balance de un cliente (corrección administrativa,
// no una compra) — devuelve el nuevo balance.
func DeductKCManual(db *sql.DB, customerID uuid.UUID, amount int) (int, error) {
	if amount <= 0 { return 0, fmt.Errorf("amount must be positive") }
	tx, err := db.Begin()
	if err != nil { return 0, err }
	defer tx.Rollback()
	var currentBalance int
	err = tx.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1 AND is_active=true FOR UPDATE`, customerID).Scan(&currentBalance)
	if err != nil { return 0, fmt.Errorf("cliente no encontrado") }
	if currentBalance < amount { return 0, fmt.Errorf("balance insuficiente: tiene %d KC, se quiere quitar %d", currentBalance, amount) }
	_, err = tx.Exec(`UPDATE customers SET kc_balance=kc_balance-$1, updated_at=NOW() WHERE id=$2`, amount, customerID)
	if err != nil { return 0, err }
	if err := tx.Commit(); err != nil { return 0, err }
	return currentBalance - amount, nil
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

// PlaceSlotBet descuenta la apuesta y, si ganó, acredita el pago — todo en
// una sola transacción atómica con row lock, igual que DeductKCAndCreateOrder.
func PlaceSlotBet(db *sql.DB, customerID uuid.UUID, betAmount, payoutAmount int, won bool) (int, error) {
	tx, err := db.Begin()
	if err != nil { return 0, err }
	defer tx.Rollback()
	var currentBalance int
	err = tx.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1 AND is_active=true FOR UPDATE`, customerID).Scan(&currentBalance)
	if err != nil { return 0, fmt.Errorf("cliente no encontrado") }
	if currentBalance < betAmount { return 0, fmt.Errorf("balance insuficiente: tienes %d, necesitas %d", currentBalance, betAmount) }
	delta := -betAmount
	if won { delta += payoutAmount }
	_, err = tx.Exec(`UPDATE customers SET kc_balance=kc_balance+$1, updated_at=NOW() WHERE id=$2`, delta, customerID)
	if err != nil { return 0, err }
	if err := tx.Commit(); err != nil { return 0, err }
	return currentBalance + delta, nil
}

func RecordSlotPlay(db *sql.DB, customerID uuid.UUID, betAmount int, won bool, payoutAmount int) error {
	_, err := db.Exec(`
		INSERT INTO slot_plays (id, customer_id, bet_amount, won, payout_amount, created_at)
		VALUES ($1, $2, $3, $4, $5, NOW())`,
		uuid.New(), customerID, betAmount, won, payoutAmount)
	return err
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
	if _, err := tx.Exec(`UPDATE customers SET kc_balance=kc_balance+$1, updated_at=NOW() WHERE id=$2`, priceKC, customerID); err != nil { return err }
	if _, err := tx.Exec(`UPDATE orders SET status='refunded', updated_at=NOW() WHERE id=$1`, orderID); err != nil { return err }
	return tx.Commit()
}

// ==================== ORDERS ====================

func GetPendingOrders(db *sql.DB) ([]types.Order, error) {
	rows, err := db.Query(`
		SELECT id, customer_id, epic_username, item_offer_id, item_name,
		       item_image, price_kc, price_vbucks, status, game_account_id, error_msg, delivery_evidence, created_at, updated_at
		FROM orders WHERE status='pending' ORDER BY created_at ASC`)
	if err != nil { return nil, err }
	defer rows.Close()
	return scanOrders(rows)
}

// ClaimPendingOrders selecciona los pedidos pendientes Y los marca como
// "processing" en la MISMA transacción, usando FOR UPDATE SKIP LOCKED. Esto
// existe porque el entorno local y producción comparten la misma base de
// datos y cada uno corre su propio worker de pedidos en un ticker
// independiente — sin esto, dos instancias del backend podrían leer el
// mismo pedido "pending" al mismo tiempo y AMBAS enviarían el regalo real
// por Epic Games, duplicando el envío de un pedido pagado una sola vez.
// FOR UPDATE SKIP LOCKED hace que si una instancia ya está mirando esas
// filas, la otra simplemente las salte en vez de esperar o repetirlas.
func ClaimPendingOrders(database *sql.DB) ([]types.Order, error) {
	tx, err := database.Begin()
	if err != nil { return nil, err }
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT id, customer_id, epic_username, item_offer_id, item_name,
		       item_image, price_kc, price_vbucks, status, game_account_id, error_msg, delivery_evidence, created_at, updated_at
		FROM orders WHERE status='pending' ORDER BY created_at ASC
		FOR UPDATE SKIP LOCKED`)
	if err != nil { return nil, err }
	orders, scanErr := scanOrders(rows)
	rows.Close()
	if scanErr != nil { return nil, scanErr }

	for _, o := range orders {
		if _, err := tx.Exec(`UPDATE orders SET status='processing', updated_at=NOW() WHERE id=$1`, o.ID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil { return nil, err }
	return orders, nil
}

func UpdateOrderStatus(db *sql.DB, orderID uuid.UUID, status string, gameAccountID *uuid.UUID, errMsg *string) error {
	_, err := db.Exec(`UPDATE orders SET status=$1, game_account_id=$2, error_msg=$3, updated_at=NOW() WHERE id=$4`,
		status, gameAccountID, errMsg, orderID)
	return err
}

// MarkOrderDelivered marca un pedido como enviado Y guarda la evidencia de
// entrega (la respuesta cruda de Epic Games confirmando el envío) en la
// misma operación — esta es la prueba que se usa si algún día hay que
// responder a una disputa de pago (el banco/pasarela pregunta "¿en verdad
// se entregó lo que se cobró?").
func MarkOrderDelivered(db *sql.DB, orderID, gameAccountID uuid.UUID, evidence string) error {
	_, err := db.Exec(`UPDATE orders SET status='sent', game_account_id=$1, error_msg=NULL, delivery_evidence=$2, updated_at=NOW() WHERE id=$3`,
		gameAccountID, evidence, orderID)
	return err
}

// CountActiveCustomers devuelve cuántos clientes activos hay registrados —
// se usa para mostrar "eres el miembro #N" en la bienvenida de Discord.
func CountActiveCustomers(db *sql.DB) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM customers WHERE is_active=true`).Scan(&count)
	return count, err
}

// GetCustomerOrderStats resume pedidos totales, entregados y KC gastado en
// entregados — para mostrar el nivel/progreso del cliente en /perfil.
func GetCustomerOrderStats(db *sql.DB, customerID uuid.UUID) (totalOrders, sentOrders, totalSpentKC int, err error) {
	err = db.QueryRow(`
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE status = 'sent'),
		       COALESCE(SUM(price_kc) FILTER (WHERE status = 'sent'), 0)
		FROM orders WHERE customer_id=$1`, customerID).Scan(&totalOrders, &sentOrders, &totalSpentKC)
	return
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

func GetOrderByID(db *sql.DB, id uuid.UUID) (types.Order, error) {
	var o types.Order
	err := db.QueryRow(`
		SELECT id, customer_id, epic_username, item_offer_id, item_name,
		       item_image, price_kc, price_vbucks, status, game_account_id, error_msg, delivery_evidence, created_at, updated_at
		FROM orders WHERE id=$1`, id).
		Scan(&o.ID, &o.CustomerID, &o.EpicUsername, &o.ItemOfferID, &o.ItemName,
			&o.ItemImage, &o.PriceKC, &o.PriceVBucks, &o.Status, &o.GameAccountID, &o.ErrorMsg, &o.DeliveryEvidence, &o.CreatedAt, &o.UpdatedAt)
	return o, err
}

func GetAllOrders(db *sql.DB, page, limit int) ([]types.Order, int, error) {
	if page < 1 { page = 1 }
	if limit < 1 || limit > 200 { limit = 50 }
	offset := (page - 1) * limit

	var total int
	db.QueryRow(`SELECT COUNT(*) FROM orders`).Scan(&total)

	rows, err := db.Query(`
		SELECT id, customer_id, epic_username, item_offer_id, item_name,
		       item_image, price_kc, price_vbucks, status, game_account_id, error_msg, delivery_evidence, created_at, updated_at
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
			&o.GameAccountID, &o.ErrorMsg, &o.DeliveryEvidence, &o.CreatedAt, &o.UpdatedAt); err != nil {
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
	ID           uuid.UUID
	CustomerID   uuid.UUID
	Gateway      string
	PaymentType  string
	ProductID    string
	ProductName  string
	AmountPEN    float64
	AmountUSD    float64
	CurrencyCode string  // divisa real cobrada por dLocal Go (vacio para las demas pasarelas)
	AmountLocal  float64 // monto en CurrencyCode
	KCAmount     int
	ExternalID   string
}

func CreatePaymentTransaction(db *sql.DB, tx PaymentTransactionInput) error {
	var currencyCode *string
	if tx.CurrencyCode != "" {
		currencyCode = &tx.CurrencyCode
	}
	_, err := db.Exec(`
		INSERT INTO payment_transactions (id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, currency_code, amount_local, kc_amount, external_id, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'pending',NOW(),NOW())`,
		tx.ID, tx.CustomerID, tx.Gateway, tx.PaymentType, tx.ProductID, tx.ProductName, tx.AmountPEN, tx.AmountUSD, currencyCode, tx.AmountLocal, tx.KCAmount, tx.ExternalID)
	return err
}

func GetPaymentTransaction(db *sql.DB, id uuid.UUID) (types.PaymentTransaction, error) {
	var t types.PaymentTransaction
	var currencyCode sql.NullString
	err := db.QueryRow(`
		SELECT id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, COALESCE(currency_code,''), COALESCE(amount_local,0), kc_amount, COALESCE(external_id,''), status, COALESCE(activation_code,''), COALESCE(autobuyer_task_id,''), created_at, updated_at
		FROM payment_transactions WHERE id=$1`, id).
		Scan(&t.ID, &t.CustomerID, &t.Gateway, &t.PaymentType, &t.ProductID, &t.ProductName, &t.AmountPEN, &t.AmountUSD, &currencyCode, &t.AmountLocal, &t.KCAmount, &t.ExternalID, &t.Status, &t.ActivationCode, &t.AutobuyerTaskID, &t.CreatedAt, &t.UpdatedAt)
	t.CurrencyCode = currencyCode.String
	return t, err
}

// CancelPendingPayment marca como "failed" una transacción propia del cliente
// que siga "pending" — se usa cuando el propio frontend detecta que la
// pasarela redirigió con status=failure (el cliente canceló el pago), para no
// dejarla mostrando "pendiente" en su historial hasta que el barrido
// automático la expire a los 30 min. La condición WHERE customer_id=$2 AND
// status='pending' hace que sea imposible cancelar un pago ajeno o uno que ya
// se aprobó (p. ej. si el webhook llegó justo en ese instante).
func CancelPendingPayment(db *sql.DB, id uuid.UUID, customerID uuid.UUID) (bool, error) {
	result, err := db.Exec(`
		UPDATE payment_transactions SET status='failed', updated_at=NOW()
		WHERE id=$1 AND customer_id=$2 AND status='pending'`, id, customerID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

func UpdatePaymentStatus(db *sql.DB, id uuid.UUID, status string, externalID string) error {
	_, err := db.Exec(`UPDATE payment_transactions SET status=$1, external_id=$2, updated_at=NOW() WHERE id=$3`,
		status, externalID, id)
	return err
}

// ClaimPaymentForApproval marca atómicamente un pago como "approved" SOLO si
// todavía no estaba aprobado/cumplido, en una única sentencia UPDATE (Postgres
// garantiza que es atómica incluso con muchas conexiones concurrentes). Esto
// existe para cerrar una condición de carrera real: los webhooks de pago son
// rutas públicas sin autenticación (los llaman las pasarelas), así que
// cualquiera puede mandar la misma notificación muchas veces en paralelo. Sin
// esto, un "leer estado → decidir en Go → escribir estado" no atómico permite
// que varias llamadas concurrentes, para el mismo pago real y ya aprobado por
// la pasarela, pasen todas el chequeo de "todavía no procesado" antes de que
// la primera termine de escribir — acreditando el mismo pago varias veces.
// Devuelve true solo para la llamada que efectivamente lo reclamó.
func ClaimPaymentForApproval(db *sql.DB, id uuid.UUID, externalID string) (bool, error) {
	result, err := db.Exec(`
		UPDATE payment_transactions SET status='approved', external_id=$2, updated_at=NOW()
		WHERE id=$1 AND status NOT IN ('approved','fulfilled')`, id, externalID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

func GetAllPaymentTransactions(db *sql.DB, page, limit int) ([]types.PaymentTransaction, int, error) {
	if page < 1 { page = 1 }
	if limit < 1 || limit > 200 { limit = 50 }
	offset := (page - 1) * limit
	var total int
	db.QueryRow(`SELECT COUNT(*) FROM payment_transactions`).Scan(&total)
	rows, err := db.Query(`
		SELECT id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, COALESCE(external_id,''), status, COALESCE(activation_code,''), COALESCE(autobuyer_task_id,''), created_at, updated_at
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
		SELECT id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, COALESCE(external_id,''), status, COALESCE(activation_code,''), COALESCE(autobuyer_task_id,''), created_at, updated_at
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

// DeleteAllRefreshTokensForCustomer revoca TODAS las sesiones (refresh
// tokens) de una cuenta — se usa al cambiar la contraseña, para que un
// atacante que ya tuviera un refresh token robado (de antes del cambio) no
// pueda seguir renovando su sesión indefinidamente después de que el dueño
// real haya "cerrado la puerta" cambiando su contraseña.
func DeleteAllRefreshTokensForCustomer(db *sql.DB, customerID uuid.UUID) error {
	_, err := db.Exec(`DELETE FROM refresh_tokens WHERE customer_id=$1`, customerID)
	return err
}

// ==================== AUDIT LOG ====================

func AddAuditLog(db *sql.DB, customerID *uuid.UUID, action, details, ip string) {
	go func() {
		defer safe.Recover("AddAuditLog")
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

func GetKCRechargeByID(db *sql.DB, id uuid.UUID) (types.KCRecharge, error) {
	var r types.KCRecharge
	err := db.QueryRow(`
		SELECT id, customer_id, amount_kc, amount_soles, method, note, approved_by, created_at
		FROM kc_recharges WHERE id=$1`, id).
		Scan(&r.ID, &r.CustomerID, &r.AmountKC, &r.AmountSoles, &r.Method, &r.Note, &r.ApprovedBy, &r.CreatedAt)
	return r, err
}

func CountPendingOrdersByCustomer(db *sql.DB, customerID uuid.UUID) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM orders WHERE customer_id=$1 AND status IN ('pending','processing')`, customerID).Scan(&count)
	return count, err
}

// ==================== PAYMENT EXPIRATION ====================

func ExpirePendingPayments(db *sql.DB) (int64, error) {
	result, err := db.Exec(`UPDATE payment_transactions SET status='expired', updated_at=NOW() WHERE status='pending' AND created_at < NOW() - INTERVAL '30 minutes'`)
	if err != nil { return 0, err }
	return result.RowsAffected()
}

func AdminUpdatePaymentStatus(db *sql.DB, id uuid.UUID, status string) error {
	_, err := db.Exec(`UPDATE payment_transactions SET status=$1, updated_at=NOW() WHERE id=$2`, status, id)
	return err
}

func DeletePayment(db *sql.DB, id uuid.UUID) error {
	_, err := db.Exec(`DELETE FROM payment_transactions WHERE id=$1`, id)
	return err
}

func GetPaymentByID(db *sql.DB, id uuid.UUID) (types.PaymentTransaction, error) {
	var t types.PaymentTransaction
	err := db.QueryRow(`SELECT id, customer_id, gateway, payment_type, product_id, product_name, amount_pen, amount_usd, kc_amount, COALESCE(external_id,''), status, COALESCE(activation_code,''), COALESCE(autobuyer_task_id,''), created_at, updated_at
		FROM payment_transactions WHERE id=$1`, id).
		Scan(&t.ID, &t.CustomerID, &t.Gateway, &t.PaymentType, &t.ProductID, &t.ProductName, &t.AmountPEN, &t.AmountUSD, &t.KCAmount, &t.ExternalID, &t.Status, &t.ActivationCode, &t.AutobuyerTaskID, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

// ==================== ADMIN ROLE ====================

func SetCustomerAdmin(db *sql.DB, customerID uuid.UUID, isAdmin bool) error {
	_, err := db.Exec(`UPDATE customers SET is_admin=$1, updated_at=NOW() WHERE id=$2`, isAdmin, customerID)
	return err
}

// ==================== 2FA (TOTP) ====================

// SetPendingTOTPSecret guarda un secreto TOTP recién generado, todavía sin
// confirmar (totp_enabled sigue en false hasta que HandlerConfirm2FA valide
// un código real generado con ese secreto).
// SetPendingTOTPSecret guarda un secreto candidato SIN tocar el secreto
// activo ni totp_enabled — si la cuenta ya tenía 2FA activado, sigue
// protegida con su factor anterior hasta que se confirme la sustitución
// (ver PromotePendingTOTPSecret). Así, generar un secreto nuevo por sí
// solo (ej. con un JWT robado pero sin la contraseña) nunca desactiva el
// 2FA existente.
func SetPendingTOTPSecret(db *sql.DB, customerID uuid.UUID, encSecret string) error {
	_, err := db.Exec(`UPDATE customers SET totp_pending_secret_enc=$1, updated_at=NOW() WHERE id=$2`, encSecret, customerID)
	return err
}

// PromotePendingTOTPSecret confirma una activación o sustitución de 2FA:
// el secreto pendiente pasa a ser el activo, se activa (o se mantiene
// activado) el 2FA, y se limpia la columna pendiente. Es el ÚNICO lugar
// donde el secreto activo cambia — y solo se llama tras validar un código
// real generado con el secreto pendiente (ver HandlerConfirm2FA).
func PromotePendingTOTPSecret(db *sql.DB, customerID uuid.UUID) error {
	_, err := db.Exec(`
		UPDATE customers
		SET totp_secret_enc=totp_pending_secret_enc, totp_pending_secret_enc=NULL, totp_enabled=true, updated_at=NOW()
		WHERE id=$1`, customerID)
	return err
}

// ClearPendingTOTPSecret descarta un intento de activación/sustitución sin
// confirmar (ej. si el usuario cancela, o la sesión expira) — el secreto
// activo (si había uno) sigue intacto.
func ClearPendingTOTPSecret(db *sql.DB, customerID uuid.UUID) error {
	_, err := db.Exec(`UPDATE customers SET totp_pending_secret_enc=NULL, updated_at=NOW() WHERE id=$1`, customerID)
	return err
}

// DisableTOTP apaga el 2FA y borra tanto el secreto como los códigos de
// respaldo — si se vuelve a activar más adelante, empieza de cero.
func DisableTOTP(db *sql.DB, customerID uuid.UUID) error {
	tx, err := db.Begin()
	if err != nil { return err }
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE customers SET totp_enabled=false, totp_secret_enc=NULL, totp_pending_secret_enc=NULL, updated_at=NOW() WHERE id=$1`, customerID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM admin_backup_codes WHERE customer_id=$1`, customerID); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateBackupCodes reemplaza los códigos de respaldo existentes (si los
// había) por un lote nuevo — se llama una sola vez, justo al confirmar la
// activación del 2FA.
func CreateBackupCodes(db *sql.DB, customerID uuid.UUID, hashes []string) error {
	tx, err := db.Begin()
	if err != nil { return err }
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM admin_backup_codes WHERE customer_id=$1`, customerID); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.Exec(`INSERT INTO admin_backup_codes (id, customer_id, code_hash, created_at) VALUES ($1,$2,$3,NOW())`,
			uuid.New(), customerID, h); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ConsumeBackupCode marca un código de respaldo como usado, atómicamente —
// solo funciona una vez por código. Devuelve false si el código no existe,
// no le pertenece a este cliente, o ya se usó antes.
func ConsumeBackupCode(db *sql.DB, customerID uuid.UUID, codeHash string) (bool, error) {
	result, err := db.Exec(`
		UPDATE admin_backup_codes SET used_at=NOW()
		WHERE customer_id=$1 AND code_hash=$2 AND used_at IS NULL`, customerID, codeHash)
	if err != nil { return false, err }
	n, err := result.RowsAffected()
	return n > 0, err
}

func CountUnusedBackupCodes(db *sql.DB, customerID uuid.UUID) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM admin_backup_codes WHERE customer_id=$1 AND used_at IS NULL`, customerID).Scan(&count)
	return count, err
}

// ==================== ELIMINAR CUENTA PROPIA ====================

// DeleteOwnAccount "elimina" la cuenta de un cliente a petición propia. No
// hace un DELETE físico de la fila: la tabla orders referencia customers
// SIN cascada (a propósito — son el registro de compras reales ya
// entregadas), así que un DELETE directo fallaría por la restricción de
// llave foránea en cuanto el cliente tuviera algún pedido. En cambio, se
// anonimiza: se borran todos los datos personales identificables (email,
// usuario Epic, vínculos de OAuth, teléfono, avatar, contraseña, 2FA) y se
// desactiva la cuenta, dejando intacto el historial de pedidos/pagos para
// fines contables, pero ya sin poder asociarlo a una persona identificable
// ni volver a iniciar sesión con esos datos.
func DeleteOwnAccount(db *sql.DB, customerID uuid.UUID) error {
	suffix := customerID.String()[:8]
	anonEmail := fmt.Sprintf("eliminado-%s@kidstoreperu.invalid", suffix)
	anonUsername := "usuario_eliminado_" + suffix

	tx, err := db.Begin()
	if err != nil { return err }
	defer tx.Rollback()

	_, err = tx.Exec(`
		UPDATE customers SET
			email=$1, epic_username=$2, password_hash='', has_password=false,
			google_id=NULL, discord_id=NULL, discord_username=NULL,
			avatar_url=NULL, phone=NULL, totp_secret_enc=NULL, totp_enabled=false,
			is_active=false, updated_at=NOW()
		WHERE id=$3`,
		anonEmail, anonUsername, customerID)
	if err != nil { return err }

	if _, err := tx.Exec(`DELETE FROM refresh_tokens WHERE customer_id=$1`, customerID); err != nil { return err }
	if _, err := tx.Exec(`DELETE FROM admin_backup_codes WHERE customer_id=$1`, customerID); err != nil { return err }

	return tx.Commit()
}

// ==================== LIBRO DE RECLAMACIONES ====================

// generateComplaintReference crea un código corto y humano-legible para que
// el consumidor pueda identificar y hacer seguimiento a su reclamo (ej:
// "KS-260908-A1B2C3"). No es secreto — solo un identificador de seguimiento.
func generateComplaintReference() (string, error) {
	b := make([]byte, 3)
	if _, err := cryptorand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("KS-%s-%s", time.Now().Format("060102"), strings.ToUpper(hex.EncodeToString(b))), nil
}

// CreateComplaint inserta un nuevo reclamo/queja del Libro de Reclamaciones
// Virtual y le asigna un código de seguimiento único.
func CreateComplaint(db *sql.DB, c types.ConsumerComplaint, ip string) (types.ConsumerComplaint, error) {
	for attempt := 0; attempt < 5; attempt++ {
		ref, err := generateComplaintReference()
		if err != nil {
			return types.ConsumerComplaint{}, err
		}
		id := uuid.New()
		var createdAt time.Time
		err = db.QueryRow(`
			INSERT INTO consumer_complaints
				(id, reference, kind, full_name, document_type, document_number, email, phone, address,
				 is_minor, guardian_name, order_id, amount_involved, product_description, detail, consumer_request,
				 status, ip_address, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'pendiente',$17,NOW())
			RETURNING created_at`,
			id, ref, c.Kind, c.FullName, c.DocumentType, c.DocumentNumber, c.Email, c.Phone, c.Address,
			c.IsMinor, c.GuardianName, c.OrderID, c.AmountInvolved, c.ProductDescription, c.Detail, c.ConsumerRequest,
			ip).Scan(&createdAt)
		if err != nil {
			// Colisión de código único (extremadamente improbable) → reintentar con uno nuevo
			if strings.Contains(err.Error(), "consumer_complaints_reference_key") {
				continue
			}
			return types.ConsumerComplaint{}, err
		}
		c.ID = id
		c.Reference = ref
		c.Status = "pendiente"
		c.CreatedAt = createdAt
		return c, nil
	}
	return types.ConsumerComplaint{}, fmt.Errorf("no se pudo generar un código de reclamo único")
}

func scanComplaint(row interface{ Scan(dest ...interface{}) error }) (types.ConsumerComplaint, error) {
	var c types.ConsumerComplaint
	err := row.Scan(&c.ID, &c.Reference, &c.Kind, &c.FullName, &c.DocumentType, &c.DocumentNumber,
		&c.Email, &c.Phone, &c.Address, &c.IsMinor, &c.GuardianName, &c.OrderID, &c.AmountInvolved,
		&c.ProductDescription, &c.Detail, &c.ConsumerRequest, &c.Status, &c.AdminResponse, &c.RespondedAt, &c.CreatedAt)
	return c, err
}

const complaintSelectCols = `id, reference, kind, full_name, document_type, document_number,
	email, phone, address, is_minor, guardian_name, order_id, amount_involved,
	product_description, detail, consumer_request, status, admin_response, responded_at, created_at`

// GetComplaintByReference permite a un consumidor consultar el estado de su
// reclamo con el código que se le entregó al presentarlo — no requiere cuenta.
func GetComplaintByReference(db *sql.DB, reference string) (types.ConsumerComplaint, error) {
	row := db.QueryRow(`SELECT `+complaintSelectCols+` FROM consumer_complaints WHERE reference=$1`, reference)
	return scanComplaint(row)
}

func GetComplaintByID(db *sql.DB, id uuid.UUID) (types.ConsumerComplaint, error) {
	row := db.QueryRow(`SELECT `+complaintSelectCols+` FROM consumer_complaints WHERE id=$1`, id)
	return scanComplaint(row)
}

// GetAllComplaints — listado paginado para el panel admin.
func GetAllComplaints(db *sql.DB, page, limit int) ([]types.ConsumerComplaint, int, error) {
	if page < 1 { page = 1 }
	if limit < 1 || limit > 200 { limit = 50 }
	offset := (page - 1) * limit

	var total int
	db.QueryRow(`SELECT COUNT(*) FROM consumer_complaints`).Scan(&total)

	rows, err := db.Query(`SELECT `+complaintSelectCols+`
		FROM consumer_complaints ORDER BY created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil { return nil, 0, err }
	defer rows.Close()

	var complaints []types.ConsumerComplaint
	for rows.Next() {
		c, err := scanComplaint(rows)
		if err != nil { return nil, 0, err }
		complaints = append(complaints, c)
	}
	return complaints, total, nil
}

// RespondToComplaint registra la respuesta del negocio a un reclamo/queja.
// Por norma de INDECOPI, el plazo máximo de respuesta es de 30 días
// calendario desde la presentación del reclamo.
func RespondToComplaint(db *sql.DB, id uuid.UUID, response string) error {
	_, err := db.Exec(`UPDATE consumer_complaints SET status='respondido', admin_response=$1, responded_at=NOW() WHERE id=$2`,
		response, id)
	return err
}

func CloseComplaint(db *sql.DB, id uuid.UUID) error {
	_, err := db.Exec(`UPDATE consumer_complaints SET status='cerrado' WHERE id=$1`, id)
	return err
}
