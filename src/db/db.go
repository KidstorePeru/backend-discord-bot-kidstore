package db

import (
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
		// ── Cuentas bot de Fortnite ──
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
		// ── Horario de bots ──
		// enabled: si el worker procesa pedidos o no
		// start_hour / end_hour: rango de horas en zona horaria de Lima (UTC-5)
		// timezone: zona horaria (siempre "America/Lima" por defecto)
		`CREATE TABLE IF NOT EXISTS bot_schedule (
			id INTEGER PRIMARY KEY DEFAULT 1,
			enabled BOOLEAN NOT NULL DEFAULT true,
			start_hour INTEGER NOT NULL DEFAULT 0  CHECK (start_hour >= 0 AND start_hour <= 23),
			end_hour   INTEGER NOT NULL DEFAULT 9  CHECK (end_hour   >= 0 AND end_hour   <= 23),
			timezone   VARCHAR(64) NOT NULL DEFAULT 'America/Lima',
			updated_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		// Insertar fila por defecto si no existe (00:00 - 09:00 Lima)
		`INSERT INTO bot_schedule (id, enabled, start_hour, end_hour, timezone)
			VALUES (1, true, 0, 9, 'America/Lima')
			ON CONFLICT (id) DO NOTHING`,
		// Índices
		`CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status)`,
		`CREATE INDEX IF NOT EXISTS idx_orders_customer ON orders(customer_id)`,
		`CREATE INDEX IF NOT EXISTS idx_kc_recharges_customer ON kc_recharges(customer_id)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_customer ON audit_logs(customer_id)`,
		`CREATE INDEX IF NOT EXISTS idx_reset_token ON password_reset_tokens(token)`,
		`CREATE INDEX IF NOT EXISTS idx_game_accounts_active ON game_accounts(is_active)`,
		// Migración: agregar price_vbucks si no existe
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='orders' AND column_name='price_vbucks') THEN
				ALTER TABLE orders ADD COLUMN price_vbucks INTEGER NOT NULL DEFAULT 0;
			END IF;
		END $$`,
		// Migración: agregar discord_lang si no existe
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='customers' AND column_name='discord_lang') THEN
				ALTER TABLE customers ADD COLUMN discord_lang VARCHAR(2) DEFAULT 'es';
			END IF;
		END $$`,

		`CREATE TABLE IF NOT EXISTS bot_config (
			key   VARCHAR(50) NOT NULL,
			value VARCHAR(255) NOT NULL,
			UNIQUE(key, value)
		)`,
	}

	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("error creating tables: %w", err)
		}
	}
	return nil
}

// ==================== BOT SCHEDULE ====================

// GetBotSchedule devuelve la configuración de horario activa.
func GetBotSchedule(db *sql.DB) (types.BotSchedule, error) {
	var s types.BotSchedule
	err := db.QueryRow(`
		SELECT id, enabled, start_hour, end_hour, timezone, updated_at
		FROM bot_schedule WHERE id=1`).
		Scan(&s.ID, &s.Enabled, &s.StartHour, &s.EndHour, &s.Timezone, &s.UpdatedAt)
	return s, err
}

// UpdateBotSchedule actualiza la configuración de horario.
func UpdateBotSchedule(db *sql.DB, enabled bool, startHour, endHour int, timezone string) error {
	if startHour < 0 || startHour > 23 || endHour < 0 || endHour > 23 {
		return fmt.Errorf("start_hour y end_hour deben estar entre 0 y 23")
	}
	if timezone == "" {
		timezone = "America/Lima"
	}
	// Validar que la zona horaria es válida
	if _, err := time.LoadLocation(timezone); err != nil {
		return fmt.Errorf("timezone inválida: %s", timezone)
	}
	_, err := db.Exec(`
		UPDATE bot_schedule
		SET enabled=$1, start_hour=$2, end_hour=$3, timezone=$4, updated_at=NOW()
		WHERE id=1`,
		enabled, startHour, endHour, timezone)
	return err
}

// IsWithinSchedule devuelve true si el momento actual está dentro del horario
// configurado y el worker está habilitado.
func IsWithinSchedule(db *sql.DB) (bool, string) {
	s, err := GetBotSchedule(db)
	if err != nil {
		// Si no puede leer el horario, permite el procesamiento por seguridad
		return true, ""
	}
	if !s.Enabled {
		return false, "worker deshabilitado por el administrador"
	}

	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	hour := now.Hour()

	// Soporte para rangos que cruzan medianoche (ej: 22 → 6)
	var inRange bool
	if s.StartHour <= s.EndHour {
		inRange = hour >= s.StartHour && hour < s.EndHour
	} else {
		inRange = hour >= s.StartHour || hour < s.EndHour
	}

	if !inRange {
		return false, fmt.Sprintf(
			"fuera de horario de operación (%02d:00 - %02d:00 %s) — hora actual: %02d:00",
			s.StartHour, s.EndHour, s.Timezone, hour,
		)
	}
	return true, ""
}

// ==================== CUSTOMER ====================

func CreateCustomer(db *sql.DB, c types.Customer) error {
	_, err := db.Exec(`
		INSERT INTO customers (id, epic_username, email, password_hash, kc_balance, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 0, NOW(), NOW())`,
		c.ID, c.EpicUsername, c.Email, c.PasswordHash)
	return err
}

func GetCustomerByEmail(db *sql.DB, email string) (types.Customer, error) {
	var c types.Customer
	err := db.QueryRow(`
		SELECT id, epic_username, email, password_hash, kc_balance,
		       discord_id, discord_username, is_active, created_at, updated_at
		FROM customers WHERE email = $1 AND is_active = true`, email).
		Scan(&c.ID, &c.EpicUsername, &c.Email, &c.PasswordHash, &c.KCBalance,
			&c.DiscordID, &c.DiscordUsername, &c.IsActive, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func GetCustomerByID(db *sql.DB, id uuid.UUID) (types.Customer, error) {
	var c types.Customer
	err := db.QueryRow(`
		SELECT id, epic_username, email, password_hash, kc_balance,
		       discord_id, discord_username, is_active, created_at, updated_at
		FROM customers WHERE id = $1 AND is_active = true`, id).
		Scan(&c.ID, &c.EpicUsername, &c.Email, &c.PasswordHash, &c.KCBalance,
			&c.DiscordID, &c.DiscordUsername, &c.IsActive, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func GetAllCustomers(db *sql.DB) ([]types.Customer, error) {
	rows, err := db.Query(`
		SELECT id, epic_username, email, kc_balance,
		       discord_id, discord_username, is_active, created_at, updated_at
		FROM customers ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var customers []types.Customer
	for rows.Next() {
		var c types.Customer
		if err := rows.Scan(&c.ID, &c.EpicUsername, &c.Email, &c.KCBalance,
			&c.DiscordID, &c.DiscordUsername, &c.IsActive, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		customers = append(customers, c)
	}
	return customers, nil
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

// ==================== PASSWORD RESET ====================

func CreatePasswordResetToken(db *sql.DB, customerID uuid.UUID, token string) error {
	_, err := db.Exec(`
		INSERT INTO password_reset_tokens (id, customer_id, token, expires_at, created_at)
		VALUES ($1, $2, $3, $4, NOW())`,
		uuid.New(), customerID, token, time.Now().Add(1*time.Hour))
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

func RechargeKC(db *sql.DB, customerID uuid.UUID, amountKC int, amountSoles *float64, note *string, approvedBy string) error {
	if amountKC <= 0 {
		return fmt.Errorf("amount_kc must be positive")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	result, err := tx.Exec(`UPDATE customers SET kc_balance=kc_balance+$1, updated_at=NOW() WHERE id=$2 AND is_active=true`, amountKC, customerID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("customer not found or inactive")
	}
	_, err = tx.Exec(`INSERT INTO kc_recharges (id, customer_id, amount_kc, amount_soles, method, note, approved_by, created_at)
		VALUES ($1, $2, $3, $4, 'manual', $5, $6, NOW())`,
		uuid.New(), customerID, amountKC, amountSoles, note, approvedBy)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func DeductKCAndCreateOrder(db *sql.DB, customerID uuid.UUID, epicUsername string, req types.CreateOrderRequest) (types.Order, error) {
	tx, err := db.Begin()
	if err != nil {
		return types.Order{}, err
	}
	defer tx.Rollback()

	var currentBalance int
	err = tx.QueryRow(`SELECT kc_balance FROM customers WHERE id=$1 AND is_active=true FOR UPDATE`, customerID).Scan(&currentBalance)
	if err != nil {
		return types.Order{}, fmt.Errorf("customer not found")
	}
	if currentBalance < req.PriceKC {
		return types.Order{}, fmt.Errorf("insufficient KC balance: have %d, need %d", currentBalance, req.PriceKC)
	}

	_, err = tx.Exec(`UPDATE customers SET kc_balance=kc_balance-$1, updated_at=NOW() WHERE id=$2`, req.PriceKC, customerID)
	if err != nil {
		return types.Order{}, err
	}

	orderID := uuid.New()
	var imgPtr *string
	if req.ItemImage != "" {
		imgPtr = &req.ItemImage
	}

	_, err = tx.Exec(`
		INSERT INTO orders (id, customer_id, epic_username, item_offer_id, item_name, item_image, price_kc, price_vbucks, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', NOW(), NOW())`,
		orderID, customerID, epicUsername, req.ItemOfferID, req.ItemName, imgPtr, req.PriceKC, req.PriceVBucks)
	if err != nil {
		return types.Order{}, err
	}
	if err := tx.Commit(); err != nil {
		return types.Order{}, err
	}
	return types.Order{
		ID: orderID, CustomerID: customerID, EpicUsername: epicUsername,
		ItemOfferID: req.ItemOfferID, ItemName: req.ItemName, ItemImage: imgPtr,
		PriceKC: req.PriceKC, PriceVBucks: req.PriceVBucks, Status: "pending",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, nil
}

func RefundOrder(db *sql.DB, orderID uuid.UUID) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var customerID uuid.UUID
	var priceKC int
	var status string
	err = tx.QueryRow(`SELECT customer_id, price_kc, status FROM orders WHERE id=$1 FOR UPDATE`, orderID).
		Scan(&customerID, &priceKC, &status)
	if err != nil {
		return fmt.Errorf("order not found")
	}
	if status == "refunded" || status == "sent" {
		return fmt.Errorf("order cannot be refunded: status is %s", status)
	}
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
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

func UpdateOrderStatus(db *sql.DB, orderID uuid.UUID, status string, gameAccountID *uuid.UUID, errMsg *string) error {
	_, err := db.Exec(`UPDATE orders SET status=$1, game_account_id=$2, error_msg=$3, updated_at=NOW() WHERE id=$4`,
		status, gameAccountID, errMsg, orderID)
	return err
}

func GetOrdersByCustomer(db *sql.DB, customerID uuid.UUID) ([]types.Order, error) {
	rows, err := db.Query(`
		SELECT id, customer_id, epic_username, item_offer_id, item_name,
		       item_image, price_kc, price_vbucks, status, game_account_id, error_msg, created_at, updated_at
		FROM orders WHERE customer_id=$1 ORDER BY created_at DESC LIMIT 50`, customerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
}

func GetAllOrders(db *sql.DB) ([]types.Order, error) {
	rows, err := db.Query(`
		SELECT id, customer_id, epic_username, item_offer_id, item_name,
		       item_image, price_kc, price_vbucks, status, game_account_id, error_msg, created_at, updated_at
		FROM orders ORDER BY created_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanOrders(rows)
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

func UpsertGameAccount(db *sql.DB, a types.GameAccount) error {
	_, err := db.Exec(`
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
		a.AccessToken, a.AccessTokenExpDate, a.RefreshToken, a.RefreshTokenExpDate,
		a.CreatedAt)
	return err
}

func GetAllGameAccounts(db *sql.DB) ([]types.GameAccount, error) {
	rows, err := db.Query(`
		SELECT id, display_name, remaining_gifts, vbucks,
		       access_token, access_token_exp_date, refresh_token, refresh_token_exp_date,
		       is_active, created_at, updated_at
		FROM game_accounts ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGameAccounts(rows)
}

func GetActiveGameAccounts(db *sql.DB) ([]types.GameAccount, error) {
	rows, err := db.Query(`
		SELECT id, display_name, remaining_gifts, vbucks,
		       access_token, access_token_exp_date, refresh_token, refresh_token_exp_date,
		       is_active, created_at, updated_at
		FROM game_accounts WHERE is_active=true ORDER BY remaining_gifts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanGameAccounts(rows)
}

func scanGameAccounts(rows *sql.Rows) ([]types.GameAccount, error) {
	var accounts []types.GameAccount
	for rows.Next() {
		var a types.GameAccount
		if err := rows.Scan(&a.ID, &a.DisplayName, &a.RemainingGifts, &a.VBucks,
			&a.AccessToken, &a.AccessTokenExpDate, &a.RefreshToken, &a.RefreshTokenExpDate,
			&a.IsActive, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
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
	_, err := db.Exec(`
		UPDATE game_accounts
		SET vbucks = GREATEST(0, vbucks - $1), updated_at = NOW()
		WHERE id = $2`, amount, accountID)
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

func UpsertGameAccountSecrets(db *sql.DB, s types.GameAccountSecrets) error {
	_, err := db.Exec(`
		INSERT INTO game_account_secrets (id, account_id, device_id, secret, created_at)
		VALUES ($1,$2,$3,$4,NOW())
		ON CONFLICT (account_id) DO UPDATE SET device_id=EXCLUDED.device_id, secret=EXCLUDED.secret`,
		s.ID, s.AccountID, s.DeviceID, s.Secret)
	return err
}

func GetGameAccountSecrets(db *sql.DB, accountID uuid.UUID) (types.GameAccountSecrets, error) {
	var s types.GameAccountSecrets
	err := db.QueryRow(`SELECT id, account_id, device_id, secret, created_at FROM game_account_secrets WHERE account_id=$1`, accountID).
		Scan(&s.ID, &s.AccountID, &s.DeviceID, &s.Secret, &s.CreatedAt)
	return s, err
}

// ==================== AUDIT LOG ====================

func AddAuditLog(db *sql.DB, customerID *uuid.UUID, action, details, ip string) {
	go func() {
		db.Exec(`INSERT INTO audit_logs (id, customer_id, action, details, ip_address, created_at)
			VALUES ($1,$2,$3,$4,$5,NOW())`,
			uuid.New(), customerID, action, details, ip)
	}()
}

// ==================== KC RECHARGES ====================

func GetRechargesByCustomer(db *sql.DB, customerID uuid.UUID) ([]types.KCRecharge, error) {
	rows, err := db.Query(`
		SELECT id, customer_id, amount_kc, amount_soles, method, note, approved_by, created_at
		FROM kc_recharges WHERE customer_id=$1 ORDER BY created_at DESC`, customerID)
	if err != nil {
		return nil, err
	}
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
               discord_id, discord_username, is_active, created_at, updated_at
        FROM customers WHERE discord_id = $1 AND is_active = true`, discordID).
        Scan(&c.ID, &c.EpicUsername, &c.Email, &c.PasswordHash, &c.KCBalance,
            &c.DiscordID, &c.DiscordUsername, &c.IsActive, &c.CreatedAt, &c.UpdatedAt)
    return c, err
}

// ==================== DISCORD LANG ====================
 
// GetDiscordLang devuelve el idioma preferido del usuario de Discord.
func GetDiscordLang(db *sql.DB, discordID string) (string, error) {
	var lang string
	err := db.QueryRow(`SELECT discord_lang FROM customers WHERE discord_id = $1`, discordID).Scan(&lang)
	return lang, err
}
 
// SetDiscordLang guarda el idioma preferido del usuario de Discord.
func SetDiscordLang(db *sql.DB, discordID string, lang string) {
	db.Exec(`UPDATE customers SET discord_lang = $1 WHERE discord_id = $2`, lang, discordID)
}

// GetBotPrefix devuelve el prefijo actual del bot
func GetBotPrefix(db *sql.DB) (string, error) {
	var prefix string
	err := db.QueryRow(`SELECT value FROM bot_config WHERE key = 'prefix'`).Scan(&prefix)
	return prefix, err
}
 
// SetBotPrefix guarda el prefijo del bot
func SetBotPrefix(db *sql.DB, prefix string) {
	db.Exec(`INSERT INTO bot_config (key, value) VALUES ('prefix', $1) ON CONFLICT (key) DO UPDATE SET value = $1`, prefix)
}
 
// GetBotAdmins devuelve los Discord IDs de los administradores del bot
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
 
// AddBotAdmin añade un Discord ID como administrador del bot
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
    tx.Exec(`INSERT INTO kc_recharges (id, customer_id, amount_kc, method, note, approved_by, created_at) VALUES ($1,$2,$3,'admin_deduct',$4,'discord-admin',NOW())`, uuid.New(), customerID, -amount, note)
    return tx.Commit()
}

func UnlinkDiscord(db *sql.DB, customerID uuid.UUID) error {
	_, err := db.Exec(`UPDATE customers SET discord_id=NULL, discord_username=NULL, discord_lang='es', updated_at=NOW() WHERE id=$1`, customerID)
	return err
}
