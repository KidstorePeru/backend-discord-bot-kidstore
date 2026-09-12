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

	// Google OAuth (login/registro)
	GoogleClientID     string `envconfig:"GOOGLE_CLIENT_ID"`
	GoogleClientSecret string `envconfig:"GOOGLE_CLIENT_SECRET"`
	GoogleRedirectURL  string `envconfig:"GOOGLE_REDIRECT_URL"`

	// Discord OAuth (login/registro)
	DiscordClientID     string `envconfig:"DISCORD_CLIENT_ID"`
	DiscordClientSecret string `envconfig:"DISCORD_CLIENT_SECRET"`
	DiscordRedirectURL  string `envconfig:"DISCORD_REDIRECT_URL"`

	// Discord Bot (notificaciones + /slot)
	DiscordBotToken          string `envconfig:"DISCORD_BOT_TOKEN"`
	DiscordGuildID           string `envconfig:"DISCORD_GUILD_ID"`
	DiscordWelcomeChannelID  string `envconfig:"DISCORD_WELCOME_CHANNEL_ID"`
	DiscordRechargeChannelID string `envconfig:"DISCORD_RECHARGE_CHANNEL_ID"`
	DiscordPurchaseChannelID string `envconfig:"DISCORD_PURCHASE_CHANNEL_ID"`
	DiscordSlotChannelID     string `envconfig:"DISCORD_SLOT_CHANNEL_ID"`
	DiscordFriend48hChannelID string `envconfig:"DISCORD_FRIEND_48H_CHANNEL_ID"`
	DiscordAdminUserID        string `envconfig:"DISCORD_ADMIN_USER_ID"`

	// App
	FrontendURL      string `envconfig:"FRONTEND_URL" default:"http://localhost:5173"`
	AdminAPIKey      string `envconfig:"ADMIN_API_KEY"`
	BotCheckInterval int    `envconfig:"BOT_CHECK_INTERVAL" default:"3"`
	EncryptionKey    string `envconfig:"ENCRYPTION_KEY"`

	// Exchange Rate
	ExchangeRateAPIKey string `envconfig:"EXCHANGE_RATE_API_KEY"`

	// Payment Info (JSON string)
	PaymentInfoJSON string `envconfig:"PAYMENT_INFO_JSON"`

	// Payment Gateways
	MercadoPagoAccessToken string `envconfig:"MERCADOPAGO_ACCESS_TOKEN"`
	PayPalClientID         string `envconfig:"PAYPAL_CLIENT_ID"`
	PayPalClientSecret     string `envconfig:"PAYPAL_CLIENT_SECRET"`
	PayPalMode             string `envconfig:"PAYPAL_MODE" default:"sandbox"`
	NOWPaymentsAPIKey      string `envconfig:"NOWPAYMENTS_API_KEY"`

	// dLocal Go (tarjetas y metodos locales fuera de Peru)
	DLocalGoAPIKey    string `envconfig:"DLOCALGO_API_KEY"`
	DLocalGoSecretKey string `envconfig:"DLOCALGO_SECRET_KEY"`
	DLocalGoSandbox   bool   `envconfig:"DLOCALGO_SANDBOX" default:"true"`

	// Email (Resend API preferred, SMTP fallback for local dev)
	ResendAPIKey string `envconfig:"RESEND_API_KEY"`
	SMTPHost     string `envconfig:"SMTP_HOST"`
	SMTPPort     int    `envconfig:"SMTP_PORT" default:"587"`
	SMTPUser     string `envconfig:"SMTP_USER"`
	SMTPPassword string `envconfig:"SMTP_PASSWORD"`
	SMTPFrom     string `envconfig:"SMTP_FROM" default:"no-reply@kidstoreperu.com"`
}

// ==================== CUSTOMER ====================

type Customer struct {
	ID              uuid.UUID  `json:"id"`
	EpicUsername    string     `json:"epic_username"`
	Email           *string    `json:"email,omitempty"`
	PasswordHash    string     `json:"-"`
	HasPassword     bool       `json:"has_password"`
	KCBalance       int        `json:"kc_balance"`
	AvatarURL       *string    `json:"avatar_url,omitempty"`
	Phone           *string    `json:"phone,omitempty"`
	GoogleID        *string    `json:"google_id,omitempty"`
	DiscordID       *string    `json:"discord_id,omitempty"`
	DiscordUsername *string    `json:"discord_username,omitempty"`
	EmailChangedAt  *time.Time `json:"-"`
	IsActive        bool       `json:"is_active"`
	IsVerified      bool       `json:"is_verified"`
	IsAdmin         bool       `json:"is_admin"`
	// TOTPSecretEnc: secreto TOTP cifrado (igual que los tokens de las
	// cuentas bot, con crypto.Encrypt) — nunca se serializa a JSON.
	// TOTPEnabled: solo pasa a true después de confirmar el código una vez
	// durante la activación (mientras tanto el secreto queda "pendiente").
	TOTPSecretEnc        *string `json:"-"`
	TOTPEnabled          bool    `json:"-"`
	TOTPPendingSecretEnc *string `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// EmailChangeCooldown es el tiempo minimo que debe pasar entre dos cambios de email.
const EmailChangeCooldown = 90 * 24 * time.Hour

// MaxEmailChangeAttempts es el numero maximo de intentos de codigo OTP antes
// de invalidar la solicitud de cambio de email.
const MaxEmailChangeAttempts = 5

// NextEmailChangeAt devuelve la fecha en la que el cliente podra volver a
// cambiar su email, o nil si ya puede hacerlo ahora mismo. El cooldown solo
// aplica despues de un cambio real — la creacion de la cuenta no cuenta como
// un cambio de email.
func (c Customer) NextEmailChangeAt() *time.Time {
	if c.EmailChangedAt == nil {
		return nil
	}
	next := c.EmailChangedAt.Add(EmailChangeCooldown)
	if next.After(time.Now()) {
		return &next
	}
	return nil
}

// Public convierte un Customer (con campos privados) en la version segura
// para exponer al cliente por la API.
func (c Customer) Public() CustomerPublic {
	return CustomerPublic{
		ID: c.ID, EpicUsername: c.EpicUsername, Email: c.Email, KCBalance: c.KCBalance,
		AvatarURL: c.AvatarURL, Phone: c.Phone, HasPassword: c.HasPassword,
		GoogleLinked: c.GoogleID != nil, DiscordLinked: c.DiscordID != nil, DiscordUsername: c.DiscordUsername,
		NextEmailChangeAt: c.NextEmailChangeAt(),
		IsVerified:        c.IsVerified, IsAdmin: c.IsAdmin, TOTPEnabled: c.TOTPEnabled, CreatedAt: c.CreatedAt,
	}
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
	// DeliveryEvidence: respuesta cruda de Epic Games confirmando el envío
	// del regalo (JSON), guardada como prueba de entrega para disputas de
	// pago/contracargos. Solo se llena cuando status="sent".
	DeliveryEvidence *string   `json:"delivery_evidence,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// ==================== LIBRO DE RECLAMACIONES ====================

// ConsumerComplaint: un reclamo o queja presentado a través del Libro de
// Reclamaciones Virtual (requisito legal en Perú — Ley N° 29571). No está
// ligado a una cuenta de cliente: cualquier consumidor puede presentar uno.
type ConsumerComplaint struct {
	ID                  uuid.UUID  `json:"id"`
	Reference           string     `json:"reference"`
	Kind                string     `json:"kind"` // "reclamo" | "queja"
	FullName            string     `json:"full_name"`
	DocumentType        string     `json:"document_type"`
	DocumentNumber      string     `json:"document_number"`
	Email               string     `json:"email"`
	Phone               *string    `json:"phone,omitempty"`
	Address             *string    `json:"address,omitempty"`
	IsMinor             bool       `json:"is_minor"`
	GuardianName        *string    `json:"guardian_name,omitempty"`
	OrderID             *uuid.UUID `json:"order_id,omitempty"`
	AmountInvolved      *float64   `json:"amount_involved,omitempty"`
	ProductDescription  string     `json:"product_description"`
	Detail              string     `json:"detail"`
	ConsumerRequest     string     `json:"consumer_request"`
	Status              string     `json:"status"` // "pendiente" | "respondido" | "cerrado"
	AdminResponse       *string    `json:"admin_response,omitempty"`
	RespondedAt         *time.Time `json:"responded_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
}

// CreateComplaintRequest: cuerpo del formulario público del Libro de
// Reclamaciones. document_type: "DNI" | "CE" | "Pasaporte". kind: "reclamo" | "queja".
type CreateComplaintRequest struct {
	Kind               string   `json:"kind" binding:"required,oneof=reclamo queja"`
	FullName           string   `json:"full_name" binding:"required,min=3,max=255"`
	DocumentType       string   `json:"document_type" binding:"required,oneof=DNI CE Pasaporte"`
	DocumentNumber     string   `json:"document_number" binding:"required,min=6,max=20"`
	Email              string   `json:"email" binding:"required,email"`
	Phone              string   `json:"phone" binding:"omitempty,max=30"`
	Address            string   `json:"address" binding:"omitempty,max=500"`
	IsMinor            bool     `json:"is_minor"`
	GuardianName       string   `json:"guardian_name" binding:"omitempty,max=255"`
	OrderID            string   `json:"order_id" binding:"omitempty,uuid"`
	AmountInvolved     *float64 `json:"amount_involved" binding:"omitempty,gte=0"`
	ProductDescription string   `json:"product_description" binding:"required,min=3,max=1000"`
	Detail             string   `json:"detail" binding:"required,min=10,max=3000"`
	ConsumerRequest    string   `json:"consumer_request" binding:"required,min=3,max=1000"`
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
	// max=125000 — antes no había ningún tope: un cero de más al escribir
	// el monto acreditaba una cantidad arbitraria sin que el sistema lo
	// cuestionara. 125 000 es 10x el paquete más grande que se vende hoy
	// (ver maxManualKCAdjustment en admin/admin.go, mismo número).
	AmountKC    int      `json:"amount_kc" binding:"required,min=1,max=125000"`
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

type CompleteOAuthRegistrationRequest struct {
	Token        string `json:"token" binding:"required"`
	EpicUsername string `json:"epic_username" binding:"required,min=3,max=50"`
}

type ResetPasswordRequest struct {
	Token    string `json:"token" binding:"required"`
	Password string `json:"password" binding:"required,min=8"`
}

type UpdateProfileRequest struct {
	EpicUsername    string  `json:"epic_username" binding:"omitempty,min=3,max=50"`
	Phone           *string `json:"phone"`
	CurrentPassword string  `json:"current_password"`
	NewPassword     string  `json:"new_password" binding:"omitempty,min=8"`
}

type UpdateAvatarRequest struct {
	Avatar string `json:"avatar" binding:"required"`
}

// ==================== CAMBIO DE EMAIL (2FA / OTP) ====================

type EmailChangeRequest struct {
	ID         uuid.UUID `json:"id"`
	CustomerID uuid.UUID `json:"customer_id"`
	NewEmail   string    `json:"new_email"`
	CodeHash   string    `json:"-"`
	Attempts   int       `json:"-"`
	ExpiresAt  time.Time `json:"expires_at"`
	CreatedAt  time.Time `json:"created_at"`
}

type RequestEmailChangeRequest struct {
	NewEmail        string `json:"new_email" binding:"required,email"`
	CurrentPassword string `json:"current_password"`
}

type ConfirmEmailChangeRequest struct {
	Code string `json:"code" binding:"required,len=6"`
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
	ID                uuid.UUID  `json:"id"`
	EpicUsername      string     `json:"epic_username"`
	Email             *string    `json:"email,omitempty"`
	KCBalance         int        `json:"kc_balance"`
	AvatarURL         *string    `json:"avatar_url,omitempty"`
	Phone             *string    `json:"phone,omitempty"`
	HasPassword       bool       `json:"has_password"`
	GoogleLinked      bool       `json:"google_linked"`
	DiscordLinked     bool       `json:"discord_linked"`
	DiscordUsername   *string    `json:"discord_username,omitempty"`
	NextEmailChangeAt *time.Time `json:"next_email_change_at,omitempty"`
	IsVerified        bool       `json:"is_verified"`
	IsAdmin           bool       `json:"is_admin"`
	TOTPEnabled       bool       `json:"totp_enabled"`
	CreatedAt         time.Time  `json:"created_at"`
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
	CurrencyCode string   `json:"currency_code,omitempty"` // divisa real cobrada por dLocal Go (si no es PEN/USD)
	AmountLocal  float64  `json:"amount_local,omitempty"`  // monto en CurrencyCode
	KCAmount    int       `json:"kc_amount"`
	ExternalID      string    `json:"external_id"`
	// ProviderPaymentID: identificador REAL del pago en la pasarela, cuando es
	// distinto del external_id que guardamos al crear la sesión (hoy solo
	// aplica a NOWPayments: external_id es el ID de la FACTURA/invoice, pero
	// para consultar el estado del pago real hace falta el ID del PAGO, que
	// solo se conoce cuando llega el IPN). Se guarda apenas se conoce, para
	// que la reconciliación automática pueda seguir consultando el pago real
	// aunque el proceso se caiga justo después de recibir el webhook. Nunca
	// se expone al cliente — es un detalle interno de recuperación.
	ProviderPaymentID string `json:"-"`
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
	ProductID   string `json:"product_id" binding:"required"`   // starter, gamer, pro, legend
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
	IsAdmin      bool   `json:"is_admin"`
}
