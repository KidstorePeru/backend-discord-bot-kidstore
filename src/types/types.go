package types

import (
	"time"

	"github.com/google/uuid"
)

// ==================== CONFIG ====================

type EnvConfig struct {
	DBHost     string `envconfig:"DB_HOST" default:"localhost"`
	DBPort     int    `envconfig:"DB_PORT" default:"5432"`
	DBUser     string `envconfig:"DB_USER"`
	DBPassword string `envconfig:"DB_PASSWORD"`
	DBName     string `envconfig:"DB_NAME"`

	Port      string `envconfig:"PORT" default:"8081"`
	SecretKey string `envconfig:"SECRET_KEY"`

	// Epic Games OAuth credentials
	EpicClient string `envconfig:"EPIC_CLIENT"`
	EpicSecret string `envconfig:"EPIC_SECRET"`

	// Discord
	DiscordClientID     string `envconfig:"DISCORD_CLIENT_ID"`
	DiscordClientSecret string `envconfig:"DISCORD_CLIENT_SECRET"`
	DiscordRedirectURL  string `envconfig:"DISCORD_REDIRECT_URL"`
	DiscordBotToken     string `envconfig:"DISCORD_BOT_TOKEN"`
	DiscordGuildID      string `envconfig:"DISCORD_GUILD_ID"`

	// App
	FrontendURL      string `envconfig:"FRONTEND_URL" default:"http://localhost:5173"`
	AdminAPIKey      string `envconfig:"ADMIN_API_KEY"`
	BotCheckInterval int    `envconfig:"BOT_CHECK_INTERVAL" default:"3"`
	EncryptionKey    string `envconfig:"ENCRYPTION_KEY"`

	// Exchange Rate
	ExchangeRateAPIKey string `envconfig:"EXCHANGE_RATE_API_KEY"`

	// Payment Info (JSON string)
	PaymentInfoJSON string `envconfig:"PAYMENT_INFO_JSON"`

	// Autobuyer
	AutobuyerURL    string `envconfig:"AUTOBUYER_URL"`
	AutobuyerAPIKey string `envconfig:"AUTOBUYER_API_KEY"`

	// Payment Gateways
	MercadoPagoAccessToken string `envconfig:"MERCADOPAGO_ACCESS_TOKEN"`
	PayPalClientID         string `envconfig:"PAYPAL_CLIENT_ID"`
	PayPalClientSecret     string `envconfig:"PAYPAL_CLIENT_SECRET"`
	PayPalMode             string `envconfig:"PAYPAL_MODE" default:"sandbox"`
	NOWPaymentsAPIKey      string `envconfig:"NOWPAYMENTS_API_KEY"`

	// SMTP
	SMTPHost     string `envconfig:"SMTP_HOST"`
	SMTPPort     int    `envconfig:"SMTP_PORT" default:"587"`
	SMTPUser     string `envconfig:"SMTP_USER"`
	SMTPPassword string `envconfig:"SMTP_PASSWORD"`
	SMTPFrom     string `envconfig:"SMTP_FROM" default:"no-reply@kidstoreperu.com"`
}

// ==================== CUSTOMER ====================

type Customer struct {
	ID              uuid.UUID `json:"id"`
	EpicUsername    string    `json:"epic_username"`
	Email           *string   `json:"email,omitempty"`
	PasswordHash    string    `json:"-"`
	KCBalance       int       `json:"kc_balance"`
	DiscordID       *string   `json:"discord_id,omitempty"`
	DiscordUsername *string   `json:"discord_username,omitempty"`
	IsActive        bool      `json:"is_active"`
	IsVerified      bool      `json:"is_verified"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ==================== GAME ACCOUNT (Bot) ====================

type GameAccount struct {
	ID                  uuid.UUID `json:"id"`
	DisplayName         string    `json:"display_name"`
	RemainingGifts      int       `json:"remaining_gifts"`
	VBucks              int       `json:"vbucks"`
	AccessToken         string    `json:"-"`
	AccessTokenExpDate  time.Time `json:"-"`
	RefreshToken        string    `json:"-"`
	RefreshTokenExpDate time.Time `json:"-"`
	IsActive            bool      `json:"is_active"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type GameAccountSecrets struct {
	ID        uuid.UUID `json:"id"`
	AccountID uuid.UUID `json:"account_id"`
	DeviceID  string    `json:"-"`
	Secret    string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

// ==================== KC RECHARGE ====================

type KCRecharge struct {
	ID          uuid.UUID `json:"id"`
	CustomerID  uuid.UUID `json:"customer_id"`
	AmountKC    int       `json:"amount_kc"`
	AmountSoles *float64  `json:"amount_soles,omitempty"`
	Method      string    `json:"method"`
	Note        *string   `json:"note,omitempty"`
	ApprovedBy  *string   `json:"approved_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// ==================== ORDER ====================

type Order struct {
	ID            uuid.UUID  `json:"id"`
	CustomerID    uuid.UUID  `json:"customer_id"`
	EpicUsername  string     `json:"epic_username"`
	ItemOfferID   string     `json:"item_offer_id"`
	ItemName      string     `json:"item_name"`
	ItemImage     *string    `json:"item_image,omitempty"`
	PriceKC       int        `json:"price_kc"`
	PriceVBucks   int        `json:"price_vbucks"`
	Status        string     `json:"status"`
	GameAccountID *uuid.UUID `json:"game_account_id,omitempty"`
	ErrorMsg      *string    `json:"error_msg,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// ==================== PASSWORD RESET ====================

type PasswordResetToken struct {
	ID         uuid.UUID  `json:"id"`
	CustomerID uuid.UUID  `json:"customer_id"`
	Token      string     `json:"token"`
	ExpiresAt  time.Time  `json:"expires_at"`
	UsedAt     *time.Time `json:"used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ==================== EMAIL VERIFICATION ====================

type EmailVerificationToken struct {
	ID         uuid.UUID  `json:"id"`
	CustomerID uuid.UUID  `json:"customer_id"`
	Token      string     `json:"token"`
	ExpiresAt  time.Time  `json:"expires_at"`
	UsedAt     *time.Time `json:"used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ==================== AUDIT LOG ====================

type AuditLog struct {
	ID         uuid.UUID  `json:"id"`
	CustomerID *uuid.UUID `json:"customer_id,omitempty"`
	Action     string     `json:"action"`
	Details    string     `json:"details"`
	IPAddress  string     `json:"ip_address"`
	CreatedAt  time.Time  `json:"created_at"`
}

// ==================== BOT SCHEDULE ====================

type BotSchedule struct {
	ID        int       `json:"id"`
	Enabled   bool      `json:"enabled"`
	StartHour int       `json:"start_hour"`
	EndHour   int       `json:"end_hour"`
	Timezone  string    `json:"timezone"`
	UpdatedAt time.Time `json:"updated_at"`
}

type BotScheduleRequest struct {
	Enabled   bool   `json:"enabled"`
	StartHour int    `json:"start_hour" binding:"min=0,max=23"`
	EndHour   int    `json:"end_hour"   binding:"min=0,max=23"`
	Timezone  string `json:"timezone"`
}

// ==================== REQUESTS ====================

type RegisterRequest struct {
	EpicUsername string `json:"epic_username" binding:"required,min=3,max=50"`
	Email        string `json:"email" binding:"required,email"`
	Password     string `json:"password" binding:"required,min=8"`
}

type LoginRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

type RechargeRequest struct {
	CustomerID  string   `json:"customer_id" binding:"required"`
	AmountKC    int      `json:"amount_kc" binding:"required,min=1"`
	AmountSoles *float64 `json:"amount_soles"`
	Note        *string  `json:"note"`
}

type CreateOrderRequest struct {
	ItemOfferID string `json:"item_offer_id" binding:"required"`
	ItemName    string `json:"item_name" binding:"required"`
	ItemImage   string `json:"item_image"`
	PriceKC     int    `json:"price_kc" binding:"required,min=1"`
	PriceVBucks int    `json:"price_vbucks" binding:"required,min=1"`
}

type ForgotPasswordRequest struct {
	Email string `json:"email" binding:"required,email"`
}

type ResetPasswordRequest struct {
	Token    string `json:"token" binding:"required"`
	Password string `json:"password" binding:"required,min=8"`
}

type UpdateProfileRequest struct {
	EpicUsername    string `json:"epic_username" binding:"omitempty,min=3,max=50"`
	Email           string `json:"email" binding:"omitempty,email"`
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password" binding:"omitempty,min=8"`
}

// ==================== EPIC GAMES API RESPONSES ====================

type EpicAccessTokenResult struct {
	AccessToken string `json:"access_token"`
}

type EpicDeviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationUriComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
}

type EpicLoginResult struct {
	AccessToken      string `json:"access_token"`
	ExpiresIn        int    `json:"expires_in"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresIn int    `json:"refresh_expires"`
	AccountId        string `json:"account_id"`
	DisplayName      string `json:"displayName"`
}

type EpicDeviceSecretsResult struct {
	DeviceId  string `json:"deviceId"`
	AccountId string `json:"accountId"`
	Secret    string `json:"secret"`
}

type EpicPublicAccount struct {
	AccountId   string `json:"id"`
	DisplayName string `json:"displayName"`
}

type EpicFriendEntry struct {
	AccountId string `json:"accountId"`
	Created   string `json:"created"`
}

// ==================== RESPONSES ====================

type CustomerPublic struct {
	ID              uuid.UUID `json:"id"`
	EpicUsername    string    `json:"epic_username"`
	Email           *string   `json:"email,omitempty"`
	KCBalance       int       `json:"kc_balance"`
	DiscordID       *string   `json:"discord_id,omitempty"`
	DiscordUsername *string   `json:"discord_username,omitempty"`
	IsVerified      bool      `json:"is_verified"`
	CreatedAt       time.Time `json:"created_at"`
}

type AuthResponse struct {
	Token    string         `json:"token"`
	Customer CustomerPublic `json:"customer"`
}

// ==================== PAYMENT TRANSACTION ====================

type PaymentTransaction struct {
	ID          uuid.UUID `json:"id"`
	CustomerID  uuid.UUID `json:"customer_id"`
	Gateway     string    `json:"gateway"`      // mercadopago, paypal, binance_pay
	PaymentType string    `json:"payment_type"` // kc_recharge, product_purchase
	ProductID   string    `json:"product_id"`
	ProductName string    `json:"product_name"`
	AmountPEN   float64   `json:"amount_pen"`
	AmountUSD   float64   `json:"amount_usd"`
	KCAmount    int       `json:"kc_amount"`
	ExternalID      string    `json:"external_id"`
	Status          string    `json:"status"` // pending, approved, failed, expired, fulfilled
	ActivationCode  string    `json:"activation_code,omitempty"`
	AutobuyerTaskID string    `json:"autobuyer_task_id,omitempty"`
	Progress        string    `json:"progress,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type CreatePaymentRequest struct {
	Gateway     string `json:"gateway" binding:"required"`      // mercadopago, paypal, binance_pay
	PaymentType string `json:"payment_type" binding:"required"` // kc_recharge, product_purchase
	ProductID   string `json:"product_id" binding:"required"`   // vb-800, pack-koi, starter, etc.
}

// ==================== REFRESH TOKEN ====================

type RefreshToken struct {
	ID         uuid.UUID `json:"id"`
	CustomerID uuid.UUID `json:"customer_id"`
	TokenHash  string    `json:"-"`
	ExpiresAt  time.Time `json:"expires_at"`
	CreatedAt  time.Time `json:"created_at"`
}

type RefreshTokenRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

// ==================== JWT CLAIMS ====================

type CustomerClaims struct {
	CustomerID   string `json:"customer_id"`
	EpicUsername string `json:"epic_username"`
	Email        string `json:"email"`
	IsCustomer   bool   `json:"is_customer"`
}
