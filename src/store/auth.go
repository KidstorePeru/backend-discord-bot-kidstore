package store

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/middleware"
	"KidStoreStore/src/types"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// ==================== REGISTER ====================
// La cuenta NO se crea hasta que el cliente verifica su correo.
// Se guarda un registro pendiente con los datos encriptados.

func HandlerRegister(database *sql.DB, secretKey string, cfg types.EnvConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req types.RegisterRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		req.EpicUsername = strings.TrimSpace(req.EpicUsername)
		req.Email = strings.ToLower(strings.TrimSpace(req.Email))

		// Verificar si ya existe una cuenta activa con ese email o usuario
		if db.EmailExists(database, req.Email) {
			c.JSON(http.StatusConflict, gin.H{"success": false, "error": "email o usuario Epic ya registrado"})
			return
		}
		if db.EpicUsernameExists(database, req.EpicUsername) {
			c.JSON(http.StatusConflict, gin.H{"success": false, "error": "email o usuario Epic ya registrado"})
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error interno"})
			return
		}

		// Generar token de verificación
		tokenBytes := make([]byte, 32)
		rand.Read(tokenBytes)
		verificationToken := hex.EncodeToString(tokenBytes)

		// Guardar registro pendiente (no crea la cuenta real)
		lang := c.GetHeader("X-Lang")
		if lang == "" { lang = "es" }
		if err := db.CreatePendingRegistration(database, req.EpicUsername, req.Email, string(hash), verificationToken, lang); err != nil {
			if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
				// Ya hay un registro pendiente — reenviar el email
				db.UpdatePendingRegistrationToken(database, req.Email, verificationToken)
			} else {
				c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error al iniciar registro"})
				return
			}
		}

		go sendVerificationEmail(cfg, req.Email, verificationToken, req.EpicUsername, lang)

		c.JSON(http.StatusOK, gin.H{
			"success":               true,
			"requires_verification": true,
			"message":               "Te enviamos un enlace de verificación. Activa tu cuenta para continuar.",
		})
	}
}

// ==================== VERIFY EMAIL ====================
// Cuando el cliente verifica, SE CREA la cuenta real.

func HandlerVerifyEmail(database *sql.DB, secretKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.Query("token")
		if token == "" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "token requerido"})
			return
		}

		// Buscar registro pendiente con ese token
		pending, err := db.GetPendingRegistration(database, token)
		if err != nil {
			// Puede ser que ya se verificó antes — buscar cuenta ya creada
			verToken, err2 := db.GetEmailVerificationToken(database, token)
			if err2 != nil {
				c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "token inválido o expirado"})
				return
			}
			// Token de cuenta existente (reenvío) — solo marcar como verificado
			db.VerifyCustomerEmail(database, verToken.CustomerID)
			db.MarkVerificationTokenUsed(database, token)
			customer, err3 := db.GetCustomerByID(database, verToken.CustomerID)
			if err3 != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo cliente"})
				return
			}
			jwtToken, _ := middleware.GenerateCustomerToken(customer, secretKey)
			db.AddAuditLog(database, &verToken.CustomerID, "EMAIL_VERIFIED", "email verificado", c.ClientIP())
			c.JSON(http.StatusOK, gin.H{
				"success": true,
				"message": "¡Cuenta verificada correctamente!",
				"token":   jwtToken,
				"customer": types.CustomerPublic{
					ID: customer.ID, EpicUsername: customer.EpicUsername,
					Email: customer.Email, KCBalance: customer.KCBalance,
					IsVerified: true, CreatedAt: customer.CreatedAt,
				},
			})
			return
		}

		// Crear la cuenta real ahora que el email fue verificado
		customerID := uuid.New()
		customer := types.Customer{
			ID:           customerID,
			EpicUsername: pending.EpicUsername,
			Email:        &pending.Email,
			PasswordHash: pending.PasswordHash,
			IsVerified:   true,
		}

		if err := db.CreateVerifiedCustomer(database, customer); err != nil {
			if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
				// La cuenta ya fue creada (doble clic en enlace) — buscar y devolver token
				existing, err2 := db.GetCustomerByEmail(database, pending.Email)
				if err2 != nil {
					c.JSON(http.StatusConflict, gin.H{"success": false, "error": "esta cuenta ya fue verificada. Inicia sesión."})
					return
				}
				jwtToken, _ := middleware.GenerateCustomerToken(existing, secretKey)
				db.DeletePendingRegistration(database, token)
				c.JSON(http.StatusOK, gin.H{
					"success": true,
					"message": "¡Cuenta ya verificada! Iniciando sesión...",
					"token":   jwtToken,
					"customer": types.CustomerPublic{
						ID: existing.ID, EpicUsername: existing.EpicUsername,
						Email: existing.Email, KCBalance: existing.KCBalance,
						IsVerified: true, CreatedAt: existing.CreatedAt,
					},
				})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error creando cuenta"})
			return
		}

		// Limpiar registro pendiente
		db.DeletePendingRegistration(database, token)
		db.AddAuditLog(database, &customerID, "REGISTER", "cuenta creada via verificación: "+pending.EpicUsername, c.ClientIP())
		db.AddAuditLog(database, &customerID, "EMAIL_VERIFIED", "email verificado en registro", c.ClientIP())

		jwtToken, err := middleware.GenerateCustomerToken(customer, secretKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error generando token"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": "¡Cuenta creada y verificada! Bienvenido a KidStorePeru 🎮",
			"token":   jwtToken,
			"customer": types.CustomerPublic{
				ID: customerID, EpicUsername: pending.EpicUsername,
				Email: &pending.Email, KCBalance: 0,
				IsVerified: true, CreatedAt: customer.CreatedAt,
			},
		})
	}
}

// ==================== RESEND VERIFICATION ====================

func HandlerResendVerification(database *sql.DB, cfg types.EnvConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		var body struct {
			Email string `json:"email" binding:"required,email"`
			Lang  string `json:"lang"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}
		body.Email = strings.ToLower(strings.TrimSpace(body.Email))
		if body.Lang == "" { body.Lang = "es" }

		// Buscar registro pendiente primero
		pending, err := db.GetPendingRegistrationByEmail(database, body.Email)
		if err == nil {
			tokenBytes := make([]byte, 32)
			rand.Read(tokenBytes)
			newToken := hex.EncodeToString(tokenBytes)
			db.UpdatePendingRegistrationToken(database, body.Email, newToken)
			go sendVerificationEmail(cfg, body.Email, newToken, pending.EpicUsername, body.Lang)
			c.JSON(http.StatusOK, gin.H{"success": true, "message": "Se envió un nuevo enlace de verificación."})
			return
		}

		// Buscar cuenta ya existente no verificada
		customer, err := db.GetCustomerByEmail(database, body.Email)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"success": true, "message": "Si el correo existe, recibirás un nuevo enlace."})
			return
		}
		if customer.IsVerified {
			c.JSON(http.StatusOK, gin.H{"success": true, "message": "Este correo ya está verificado. Puedes iniciar sesión."})
			return
		}

		tokenBytes := make([]byte, 32)
		rand.Read(tokenBytes)
		verificationToken := hex.EncodeToString(tokenBytes)
		db.CreateEmailVerificationToken(database, customer.ID, verificationToken)
		go sendVerificationEmail(cfg, body.Email, verificationToken, customer.EpicUsername, body.Lang)

		c.JSON(http.StatusOK, gin.H{"success": true, "message": "Se envió un nuevo enlace de verificación."})
	}
}

// ==================== LOGIN ====================

func HandlerLogin(database *sql.DB, secretKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req types.LoginRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}
		req.Email = strings.ToLower(strings.TrimSpace(req.Email))

		customer, err := db.GetCustomerByEmail(database, req.Email)
		if err != nil {
			if db.PendingRegistrationExists(database, req.Email) {
				c.JSON(http.StatusForbidden, gin.H{
					"success":               false,
					"error":                 "Debes verificar tu correo electrónico antes de iniciar sesión. Revisa tu bandeja de entrada.",
					"code":                  "EMAIL_NOT_VERIFIED",
					"requires_verification": true,
				})
				return
			}
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "credenciales inválidas"})
			return
		}

		if err := bcrypt.CompareHashAndPassword([]byte(customer.PasswordHash), []byte(req.Password)); err != nil {
			db.AddAuditLog(database, &customer.ID, "LOGIN_FAILED", "intento fallido", c.ClientIP())
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "credenciales inválidas"})
			return
		}

		if !customer.IsVerified {
			c.JSON(http.StatusForbidden, gin.H{
				"success":               false,
				"error":                 "Debes verificar tu correo electrónico antes de iniciar sesión.",
				"code":                  "EMAIL_NOT_VERIFIED",
				"requires_verification": true,
			})
			return
		}

		token, err := middleware.GenerateCustomerToken(customer, secretKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error generando token"})
			return
		}

		refreshPlain, refreshHash, err := middleware.GenerateRefreshToken()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error generando refresh token"})
			return
		}
		db.CreateRefreshToken(database, customer.ID, refreshHash, time.Now().Add(7*24*time.Hour))

		db.AddAuditLog(database, &customer.ID, "LOGIN", "login exitoso", c.ClientIP())

		c.JSON(http.StatusOK, gin.H{
			"success":       true,
			"token":         token,
			"refresh_token": refreshPlain,
			"customer": types.CustomerPublic{
				ID: customer.ID, EpicUsername: customer.EpicUsername,
				Email: customer.Email, KCBalance: customer.KCBalance,
				DiscordID: customer.DiscordID, DiscordUsername: customer.DiscordUsername,
				IsVerified: customer.IsVerified, CreatedAt: customer.CreatedAt,
			},
		})
	}
}

// ==================== REFRESH TOKEN ====================

func HandlerRefreshToken(database *sql.DB, secretKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req types.RefreshTokenRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		tokenHash := middleware.HashRefreshToken(req.RefreshToken)
		storedToken, err := db.GetRefreshToken(database, tokenHash)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "refresh token inválido o expirado", "code": "INVALID_REFRESH_TOKEN"})
			return
		}

		// Delete the used token (rotation)
		db.DeleteRefreshToken(database, tokenHash)

		customer, err := db.GetCustomerByID(database, storedToken.CustomerID)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "cliente no encontrado"})
			return
		}

		// Generate new token pair
		newAccessToken, err := middleware.GenerateCustomerToken(customer, secretKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error generando token"})
			return
		}

		newRefreshPlain, newRefreshHash, err := middleware.GenerateRefreshToken()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error generando refresh token"})
			return
		}
		db.CreateRefreshToken(database, customer.ID, newRefreshHash, time.Now().Add(7*24*time.Hour))

		c.JSON(http.StatusOK, gin.H{
			"success":       true,
			"token":         newAccessToken,
			"refresh_token": newRefreshPlain,
			"customer": types.CustomerPublic{
				ID: customer.ID, EpicUsername: customer.EpicUsername,
				Email: customer.Email, KCBalance: customer.KCBalance,
				DiscordID: customer.DiscordID, DiscordUsername: customer.DiscordUsername,
				IsVerified: customer.IsVerified, CreatedAt: customer.CreatedAt,
			},
		})
	}
}

// ==================== ME ====================

func HandlerMe(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, err := uuid.Parse(customerIDStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id inválido"})
			return
		}
		customer, err := db.GetCustomerByID(database, customerID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "cliente no encontrado"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"customer": types.CustomerPublic{
				ID: customer.ID, EpicUsername: customer.EpicUsername,
				Email: customer.Email, KCBalance: customer.KCBalance,
				DiscordID: customer.DiscordID, DiscordUsername: customer.DiscordUsername,
				IsVerified: customer.IsVerified, CreatedAt: customer.CreatedAt,
			},
		})
	}
}

// ==================== RECHARGE HISTORY ====================

func HandlerGetMyRecharges(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, _ := uuid.Parse(customerIDStr)

		recharges, err := db.GetRechargesByCustomer(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo recargas"})
			return
		}
		if recharges == nil { recharges = []types.KCRecharge{} }

		payments, err := db.GetPaymentsByCustomer(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo pagos"})
			return
		}
		if payments == nil { payments = []types.PaymentTransaction{} }

		c.JSON(http.StatusOK, gin.H{
			"success":   true,
			"recharges": recharges,
			"payments":  payments,
		})
	}
}

// ==================== UPDATE PROFILE ====================

func HandlerUpdateProfile(database *sql.DB, secretKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, _ := uuid.Parse(customerIDStr)

		var req types.UpdateProfileRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		customer, err := db.GetCustomerByID(database, customerID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "cliente no encontrado"})
			return
		}

		if req.Email != "" || req.NewPassword != "" {
			if req.CurrentPassword == "" {
				c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "se requiere la contraseña actual para cambiar email o contraseña"})
				return
			}
			if err := bcrypt.CompareHashAndPassword([]byte(customer.PasswordHash), []byte(req.CurrentPassword)); err != nil {
				c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "contraseña actual incorrecta"})
				return
			}
		}

		var newHash string
		if req.NewPassword != "" {
			h, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error generando contraseña"})
				return
			}
			newHash = string(h)
		}

		newEmail := strings.ToLower(strings.TrimSpace(req.Email))
		newEpic := strings.TrimSpace(req.EpicUsername)

		if err := db.UpdateProfile(database, customerID, newEpic, newEmail, newHash); err != nil {
			if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
				c.JSON(http.StatusConflict, gin.H{"success": false, "error": "email o usuario Epic ya en uso"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error actualizando perfil"})
			return
		}

		updatedCustomer, _ := db.GetCustomerByID(database, customerID)
		token, _ := middleware.GenerateCustomerToken(updatedCustomer, secretKey)
		db.AddAuditLog(database, &customerID, "PROFILE_UPDATED", "perfil actualizado", c.ClientIP())

		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": "perfil actualizado",
			"token":   token,
			"customer": types.CustomerPublic{
				ID: updatedCustomer.ID, EpicUsername: updatedCustomer.EpicUsername,
				Email: updatedCustomer.Email, KCBalance: updatedCustomer.KCBalance,
				DiscordID: updatedCustomer.DiscordID, DiscordUsername: updatedCustomer.DiscordUsername,
				IsVerified: updatedCustomer.IsVerified, CreatedAt: updatedCustomer.CreatedAt,
			},
		})
	}
}

// ==================== FORGOT PASSWORD ====================

func HandlerForgotPassword(database *sql.DB, cfg types.EnvConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req types.ForgotPasswordRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}
		req.Email = strings.ToLower(strings.TrimSpace(req.Email))

		lang := c.GetHeader("X-Lang")
		if lang == "" { lang = "es" }

		customer, err := db.GetCustomerByEmail(database, req.Email)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"success": true, "message": "Si el correo existe, recibirás un enlace de recuperación"})
			return
		}

		tokenBytes := make([]byte, 32)
		rand.Read(tokenBytes)
		token := hex.EncodeToString(tokenBytes)

		if err := db.CreatePasswordResetToken(database, customer.ID, token); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error interno"})
			return
		}

		go sendResetEmail(cfg, req.Email, token, customer.EpicUsername, lang)

		c.JSON(http.StatusOK, gin.H{"success": true, "message": "Si el correo existe, recibirás un enlace de recuperación"})
	}
}

// ==================== RESET PASSWORD ====================

func HandlerResetPassword(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req types.ResetPasswordRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		resetToken, err := db.GetPasswordResetToken(database, req.Token)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "token inválido o expirado"})
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error interno"})
			return
		}

		if err := db.UpdateProfile(database, resetToken.CustomerID, "", "", string(hash)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error actualizando contraseña"})
			return
		}

		db.MarkResetTokenUsed(database, req.Token)
		db.AddAuditLog(database, &resetToken.CustomerID, "PASSWORD_RESET", "contraseña restablecida", c.ClientIP())

		c.JSON(http.StatusOK, gin.H{"success": true, "message": "contraseña actualizada correctamente"})
	}
}

// ==================== LINK DISCORD ====================

func HandlerLinkDiscord(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, _ := uuid.Parse(customerIDStr)

		var body struct {
			DiscordID       string `json:"discord_id" binding:"required"`
			DiscordUsername string `json:"discord_username" binding:"required"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		if err := db.LinkDiscord(database, customerID, body.DiscordID, body.DiscordUsername); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error vinculando Discord"})
			return
		}

		db.AddAuditLog(database, &customerID, "DISCORD_LINKED", "discord: "+body.DiscordUsername, c.ClientIP())
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "Discord vinculado correctamente"})
	}
}

// ==================== UNLINK DISCORD ====================

func HandlerUnlinkDiscord(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, _ := uuid.Parse(customerIDStr)

		if err := db.UnlinkDiscord(database, customerID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error desvinculando Discord"})
			return
		}

		db.AddAuditLog(database, &customerID, "DISCORD_UNLINKED", "discord desvinculado por el usuario", c.ClientIP())
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "Discord desvinculado correctamente"})
	}
}

// ==================== HELPERS ====================

func sendVerificationEmail(cfg types.EnvConfig, toEmail, token, username, lang string) {
	verifyURL := fmt.Sprintf("%s/verify-email?token=%s", cfg.FrontendURL, token)

	if cfg.SMTPHost == "" {
		slog.Info("Email: verification URL (SMTP not configured)", "to", toEmail, "url", verifyURL)
		return
	}

	es := lang != "en"
	subject := "Verifica tu cuenta — KidStorePeru"
	if !es { subject = "Verify your account — KidStorePeru" }

	var htmlBody string
	if es {
		htmlBody = buildVerificationEmailES(username, verifyURL)
	} else {
		htmlBody = buildVerificationEmailEN(username, verifyURL)
	}

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: error enviando verificacion", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: verificacion enviada", "to", toEmail)
	}
}

func sendResetEmail(cfg types.EnvConfig, toEmail, token, username, lang string) {
	if cfg.ResendAPIKey == "" && cfg.SMTPHost == "" {
		slog.Info("Email: reset token (no email provider configured)", "to", toEmail, "token", token)
		return
	}

	es := lang != "en"
	resetURL := fmt.Sprintf("%s/reset-password?token=%s", cfg.FrontendURL, token)
	subject := "Recuperar contraseña — KidStorePeru"
	if !es { subject = "Reset your password — KidStorePeru" }

	var htmlBody string
	if es {
		htmlBody = buildResetEmailES(username, resetURL)
	} else {
		htmlBody = buildResetEmailEN(username, resetURL)
	}

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: error enviando reset", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: reset enviado", "to", toEmail)
	}
}

// ── Templates HTML de emails ──

func emailBase(title, preheader, content string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="es">
<head>
<meta charset="UTF-8"/>
<meta name="viewport" content="width=device-width,initial-scale=1"/>
<title>%s</title>
</head>
<body style="margin:0;padding:0;background:#0a0a0f;font-family:'Segoe UI',Arial,sans-serif;">
<span style="display:none;max-height:0;overflow:hidden;">%s</span>
<table width="100%%" cellpadding="0" cellspacing="0" style="background:#0a0a0f;padding:40px 20px;">
  <tr><td align="center">
    <table width="100%%" cellpadding="0" cellspacing="0" style="max-width:560px;">
      <!-- Header -->
      <tr><td align="center" style="background:linear-gradient(135deg,#1a0a2e 0%%,#0d1117 100%%);border-radius:20px 20px 0 0;padding:40px 40px 32px;">
        <img src="https://www.kidstoreperu.net/logotipo.png" alt="KidStorePeru" width="160" style="display:block;margin:0 auto 20px;max-width:160px;"/>
        <div style="width:48px;height:3px;background:linear-gradient(90deg,#7c3aed,#a855f7);border-radius:2px;margin:0 auto;"></div>
      </td></tr>
      <!-- Body -->
      <tr><td style="background:#0f0f1a;padding:40px;border-left:1px solid #1e1e3a;border-right:1px solid #1e1e3a;">
        %s
      </td></tr>
      <!-- Footer -->
      <tr><td align="center" style="background:#080810;border-radius:0 0 20px 20px;padding:24px 40px;border:1px solid #1e1e3a;border-top:none;">
        <p style="margin:0 0 8px;font-size:12px;color:#4a4a6a;">KidStorePeru — La tienda de Fortnite más confiable 🎮</p>
        <p style="margin:0;font-size:11px;color:#3a3a5a;">
          <a href="https://www.kidstoreperu.net" style="color:#7c3aed;text-decoration:none;">kidstoreperu.net</a>
          &nbsp;·&nbsp;
          <a href="https://www.kidstoreperu.net/privacy" style="color:#4a4a6a;text-decoration:none;">Privacidad</a>
        </p>
      </td></tr>
    </table>
  </td></tr>
</table>
</body>
</html>`, title, preheader, content)
}

func buildVerificationEmailES(username, verifyURL string) string {
	content := fmt.Sprintf(`
<h1 style="margin:0 0 8px;font-size:24px;font-weight:800;color:#ffffff;">¡Hola, %s! 👋</h1>
<p style="margin:0 0 24px;font-size:15px;color:#8b8ba7;line-height:1.6;">Gracias por registrarte en <strong style="color:#a855f7;">KidStorePeru</strong>. Para activar tu cuenta y empezar a comprar items de Fortnite, verifica tu correo electrónico.</p>
<div style="background:linear-gradient(135deg,#1a0a2e,#0d1117);border:1px solid #2d1f4e;border-radius:14px;padding:24px;margin:0 0 24px;text-align:center;">
  <p style="margin:0 0 6px;font-size:13px;color:#6b6b8a;text-transform:uppercase;letter-spacing:1px;font-weight:600;">Tu cuenta está lista</p>
  <p style="margin:0 0 20px;font-size:14px;color:#8b8ba7;">Un solo clic para activarla</p>
  <a href="%s" style="display:inline-block;background:linear-gradient(135deg,#7c3aed,#a855f7);color:#ffffff;text-decoration:none;padding:14px 36px;border-radius:12px;font-size:15px;font-weight:700;letter-spacing:0.3px;">✓ Verificar mi cuenta</a>
</div>
<p style="margin:0 0 12px;font-size:13px;color:#6b6b8a;">¿El botón no funciona? Copia y pega este enlace:</p>
<div style="background:#080810;border:1px solid #1e1e3a;border-radius:10px;padding:12px 16px;margin:0 0 24px;word-break:break-all;">
  <a href="%s" style="font-size:12px;color:#7c3aed;text-decoration:none;">%s</a>
</div>
<div style="border-top:1px solid #1e1e3a;padding-top:20px;">
  <p style="margin:0;font-size:12px;color:#4a4a6a;">⏰ Este enlace expira en <strong style="color:#6b6b8a;">24 horas</strong>. Si no creaste esta cuenta, puedes ignorar este correo.</p>
</div>`, username, verifyURL, verifyURL, verifyURL)
	return emailBase("Verifica tu cuenta — KidStorePeru", "Activa tu cuenta en KidStorePeru con un clic", content)
}

func buildVerificationEmailEN(username, verifyURL string) string {
	content := fmt.Sprintf(`
<h1 style="margin:0 0 8px;font-size:24px;font-weight:800;color:#ffffff;">Hey, %s! 👋</h1>
<p style="margin:0 0 24px;font-size:15px;color:#8b8ba7;line-height:1.6;">Thanks for signing up at <strong style="color:#a855f7;">KidStorePeru</strong>. To activate your account and start buying Fortnite items, please verify your email address.</p>
<div style="background:linear-gradient(135deg,#1a0a2e,#0d1117);border:1px solid #2d1f4e;border-radius:14px;padding:24px;margin:0 0 24px;text-align:center;">
  <p style="margin:0 0 6px;font-size:13px;color:#6b6b8a;text-transform:uppercase;letter-spacing:1px;font-weight:600;">Your account is ready</p>
  <p style="margin:0 0 20px;font-size:14px;color:#8b8ba7;">One click to activate it</p>
  <a href="%s" style="display:inline-block;background:linear-gradient(135deg,#7c3aed,#a855f7);color:#ffffff;text-decoration:none;padding:14px 36px;border-radius:12px;font-size:15px;font-weight:700;letter-spacing:0.3px;">✓ Verify my account</a>
</div>
<p style="margin:0 0 12px;font-size:13px;color:#6b6b8a;">Button not working? Copy and paste this link:</p>
<div style="background:#080810;border:1px solid #1e1e3a;border-radius:10px;padding:12px 16px;margin:0 0 24px;word-break:break-all;">
  <a href="%s" style="font-size:12px;color:#7c3aed;text-decoration:none;">%s</a>
</div>
<div style="border-top:1px solid #1e1e3a;padding-top:20px;">
  <p style="margin:0;font-size:12px;color:#4a4a6a;">⏰ This link expires in <strong style="color:#6b6b8a;">24 hours</strong>. If you didn't create this account, you can safely ignore this email.</p>
</div>`, username, verifyURL, verifyURL, verifyURL)
	return emailBase("Verify your account — KidStorePeru", "Activate your KidStorePeru account with one click", content)
}

func buildResetEmailES(username, resetURL string) string {
	content := fmt.Sprintf(`
<h1 style="margin:0 0 8px;font-size:24px;font-weight:800;color:#ffffff;">Recuperar contraseña</h1>
<p style="margin:0 0 24px;font-size:15px;color:#8b8ba7;line-height:1.6;">Hola <strong style="color:#ffffff;">%s</strong>, recibimos una solicitud para restablecer la contraseña de tu cuenta en KidStorePeru.</p>
<div style="background:linear-gradient(135deg,#1a0a2e,#0d1117);border:1px solid #2d1f4e;border-radius:14px;padding:24px;margin:0 0 24px;text-align:center;">
  <p style="margin:0 0 20px;font-size:14px;color:#8b8ba7;">Haz clic en el botón para crear una nueva contraseña</p>
  <a href="%s" style="display:inline-block;background:linear-gradient(135deg,#7c3aed,#a855f7);color:#ffffff;text-decoration:none;padding:14px 36px;border-radius:12px;font-size:15px;font-weight:700;letter-spacing:0.3px;">🔑 Restablecer contraseña</a>
</div>
<p style="margin:0 0 12px;font-size:13px;color:#6b6b8a;">¿El botón no funciona? Copia y pega este enlace:</p>
<div style="background:#080810;border:1px solid #1e1e3a;border-radius:10px;padding:12px 16px;margin:0 0 24px;word-break:break-all;">
  <a href="%s" style="font-size:12px;color:#7c3aed;text-decoration:none;">%s</a>
</div>
<div style="border-top:1px solid #1e1e3a;padding-top:20px;">
  <p style="margin:0;font-size:12px;color:#4a4a6a;">⏰ Este enlace expira en <strong style="color:#6b6b8a;">10 minutos</strong>. Si no solicitaste esto, ignora este correo — tu contraseña no cambiará.</p>
</div>`, username, resetURL, resetURL, resetURL)
	return emailBase("Recuperar contraseña — KidStorePeru", "Restablece tu contraseña de KidStorePeru", content)
}

func buildResetEmailEN(username, resetURL string) string {
	content := fmt.Sprintf(`
<h1 style="margin:0 0 8px;font-size:24px;font-weight:800;color:#ffffff;">Reset your password</h1>
<p style="margin:0 0 24px;font-size:15px;color:#8b8ba7;line-height:1.6;">Hi <strong style="color:#ffffff;">%s</strong>, we received a request to reset the password for your KidStorePeru account.</p>
<div style="background:linear-gradient(135deg,#1a0a2e,#0d1117);border:1px solid #2d1f4e;border-radius:14px;padding:24px;margin:0 0 24px;text-align:center;">
  <p style="margin:0 0 20px;font-size:14px;color:#8b8ba7;">Click the button below to create a new password</p>
  <a href="%s" style="display:inline-block;background:linear-gradient(135deg,#7c3aed,#a855f7);color:#ffffff;text-decoration:none;padding:14px 36px;border-radius:12px;font-size:15px;font-weight:700;letter-spacing:0.3px;">🔑 Reset password</a>
</div>
<p style="margin:0 0 12px;font-size:13px;color:#6b6b8a;">Button not working? Copy and paste this link:</p>
<div style="background:#080810;border:1px solid #1e1e3a;border-radius:10px;padding:12px 16px;margin:0 0 24px;word-break:break-all;">
  <a href="%s" style="font-size:12px;color:#7c3aed;text-decoration:none;">%s</a>
</div>
<div style="border-top:1px solid #1e1e3a;padding-top:20px;">
  <p style="margin:0;font-size:12px;color:#4a4a6a;">⏰ This link expires in <strong style="color:#6b6b8a;">10 minutes</strong>. If you didn't request this, ignore this email — your password won't change.</p>
</div>`, username, resetURL, resetURL, resetURL)
	return emailBase("Reset your password — KidStorePeru", "Reset your KidStorePeru password", content)
}
