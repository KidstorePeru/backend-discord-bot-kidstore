package store

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"
	"KidStoreStore/src/middleware"
	"KidStoreStore/src/types"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/big"
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
				"customer": customer.Public(),
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
			HasPassword:  true,
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
					"customer": existing.Public(),
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
		discordbot.NotifyWelcome(customer)

		jwtToken, err := middleware.GenerateCustomerToken(customer, secretKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error generando token"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": "¡Cuenta creada y verificada! Bienvenido a KidStorePeru 🎮",
			"token":   jwtToken,
			"customer": customer.Public(),
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
			"customer": customer.Public(),
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
			"customer": customer.Public(),
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
			"customer": customer.Public(),
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

		newEpic := strings.TrimSpace(req.EpicUsername)

		// Cambiar el usuario Epic requiere confirmar identidad con la
		// contraseña actual — salvo que la cuenta no tenga una (registrada por
		// Google/Discord), en cuyo caso la sesion OAuth ya es suficiente prueba.
		if newEpic != "" && customer.HasPassword {
			if req.CurrentPassword == "" {
				c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "se requiere la contraseña actual para cambiar el usuario Epic"})
				return
			}
			if err := bcrypt.CompareHashAndPassword([]byte(customer.PasswordHash), []byte(req.CurrentPassword)); err != nil {
				c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "contraseña actual incorrecta"})
				return
			}
		}

		var newHash string
		if req.NewPassword != "" {
			// Cambiar una contraseña existente exige la actual; si la cuenta
			// no tiene una (OAuth), esto es "configurar contraseña" por primera vez.
			if customer.HasPassword {
				if req.CurrentPassword == "" {
					c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "se requiere la contraseña actual para cambiarla"})
					return
				}
				if err := bcrypt.CompareHashAndPassword([]byte(customer.PasswordHash), []byte(req.CurrentPassword)); err != nil {
					c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "contraseña actual incorrecta"})
					return
				}
			}
			h, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error generando contraseña"})
				return
			}
			newHash = string(h)
		}

		if err := db.UpdateProfile(database, customerID, newEpic, newHash, req.Phone); err != nil {
			if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
				c.JSON(http.StatusConflict, gin.H{"success": false, "error": "usuario Epic ya en uso"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error actualizando perfil"})
			return
		}

		updatedCustomer, _ := db.GetCustomerByID(database, customerID)
		token, _ := middleware.GenerateCustomerToken(updatedCustomer, secretKey)
		db.AddAuditLog(database, &customerID, "PROFILE_UPDATED", "perfil actualizado", c.ClientIP())

		// Alerta de seguridad: si cambió la contraseña, avisar por correo. Si
		// no fue el dueño real quien la cambió, esta es la única forma de que
		// se entere a tiempo.
		if newHash != "" && updatedCustomer.Email != nil && *updatedCustomer.Email != "" {
			lang := c.GetHeader("X-Lang")
			if lang == "" { lang = "es" }
			go sendPasswordChangedEmail(smtpConfig, *updatedCustomer.Email, updatedCustomer.EpicUsername, lang)
		}

		c.JSON(http.StatusOK, gin.H{
			"success":  true,
			"message":  "perfil actualizado",
			"token":    token,
			"customer": updatedCustomer.Public(),
		})
	}
}

// ==================== UPDATE AVATAR ====================

const maxAvatarBytes = 400 * 1024 // ~400KB decoded — el cliente redimensiona antes de subir

func HandlerUpdateAvatar(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, _ := uuid.Parse(customerIDStr)

		var req types.UpdateAvatarRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		if !strings.HasPrefix(req.Avatar, "data:image/") {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "formato de imagen inválido"})
			return
		}
		commaIdx := strings.Index(req.Avatar, ",")
		if commaIdx == -1 {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "formato de imagen inválido"})
			return
		}
		decoded, err := base64.StdEncoding.DecodeString(req.Avatar[commaIdx+1:])
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "no se pudo decodificar la imagen"})
			return
		}
		if len(decoded) > maxAvatarBytes {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"success": false, "error": "la imagen es muy grande (máx. 400KB)"})
			return
		}

		if err := db.SetAvatar(database, customerID, req.Avatar); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error guardando la foto"})
			return
		}
		db.AddAuditLog(database, &customerID, "AVATAR_UPDATED", "foto de perfil actualizada", c.ClientIP())

		updatedCustomer, err := db.GetCustomerByID(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo cliente"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "customer": updatedCustomer.Public()})
	}
}

// ==================== CAMBIO DE EMAIL (2FA / OTP) ====================

func generateOTPCode() string {
	max := big.NewInt(1000000)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		n = big.NewInt(0)
	}
	return fmt.Sprintf("%06d", n.Int64())
}

// HandlerRequestEmailChange envia un codigo OTP al nuevo correo. El email
// del cliente solo se actualiza cuando el codigo es confirmado.
func HandlerRequestEmailChange(database *sql.DB, cfg types.EnvConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, _ := uuid.Parse(customerIDStr)

		var req types.RequestEmailChangeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}
		newEmail := strings.ToLower(strings.TrimSpace(req.NewEmail))

		customer, err := db.GetCustomerByID(database, customerID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "cliente no encontrado"})
			return
		}

		if customer.Email != nil && newEmail == *customer.Email {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "ese ya es tu correo actual"})
			return
		}

		if next := customer.NextEmailChangeAt(); next != nil {
			days := int(time.Until(*next).Hours()/24) + 1
			c.JSON(http.StatusForbidden, gin.H{
				"success":               false,
				"error":                 fmt.Sprintf("solo puedes cambiar tu email cada 90 días — inténtalo de nuevo en %d día(s)", days),
				"code":                  "EMAIL_COOLDOWN",
				"next_email_change_at":  next,
			})
			return
		}

		// Confirmar identidad con la contraseña actual — salvo que la cuenta
		// no tenga una (registrada por Google/Discord).
		if customer.HasPassword {
			if req.CurrentPassword == "" {
				c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "se requiere la contraseña actual para cambiar el correo"})
				return
			}
			if err := bcrypt.CompareHashAndPassword([]byte(customer.PasswordHash), []byte(req.CurrentPassword)); err != nil {
				c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "contraseña actual incorrecta"})
				return
			}
		}

		if db.EmailExists(database, newEmail) {
			c.JSON(http.StatusConflict, gin.H{"success": false, "error": "ese correo ya está en uso"})
			return
		}

		code := generateOTPCode()
		codeHash, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error interno"})
			return
		}

		if err := db.CreateEmailChangeRequest(database, customerID, newEmail, string(codeHash)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error creando solicitud"})
			return
		}

		lang := c.GetHeader("X-Lang")
		if lang == "" { lang = "es" }
		go sendEmailChangeOTP(cfg, newEmail, code, customer.EpicUsername, lang)
		db.AddAuditLog(database, &customerID, "EMAIL_CHANGE_REQUESTED", "solicitud de cambio de correo: "+newEmail, c.ClientIP())

		c.JSON(http.StatusOK, gin.H{"success": true, "message": "código enviado al nuevo correo"})
	}
}

func HandlerConfirmEmailChange(database *sql.DB, secretKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		customerID, _ := uuid.Parse(customerIDStr)

		var req types.ConfirmEmailChangeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		pending, err := db.GetEmailChangeRequest(database, customerID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "no hay una solicitud de cambio de correo activa"})
			return
		}

		if pending.Attempts >= types.MaxEmailChangeAttempts {
			db.DeleteEmailChangeRequest(database, customerID)
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "demasiados intentos fallidos — solicita un nuevo código"})
			return
		}

		if err := bcrypt.CompareHashAndPassword([]byte(pending.CodeHash), []byte(req.Code)); err != nil {
			db.IncrementEmailChangeAttempts(database, pending.ID)
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "código incorrecto"})
			return
		}

		if db.EmailExists(database, pending.NewEmail) {
			db.DeleteEmailChangeRequest(database, customerID)
			c.JSON(http.StatusConflict, gin.H{"success": false, "error": "ese correo ya está en uso"})
			return
		}

		if err := db.ConfirmEmailChange(database, customerID, pending.NewEmail); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error actualizando el correo"})
			return
		}
		db.DeleteEmailChangeRequest(database, customerID)
		db.AddAuditLog(database, &customerID, "EMAIL_CHANGED", "correo actualizado a "+pending.NewEmail, c.ClientIP())

		updatedCustomer, err := db.GetCustomerByID(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo cliente"})
			return
		}
		token, _ := middleware.GenerateCustomerToken(updatedCustomer, secretKey)

		c.JSON(http.StatusOK, gin.H{
			"success":  true,
			"message":  "correo actualizado",
			"token":    token,
			"customer": updatedCustomer.Public(),
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

		if err := db.UpdateProfile(database, resetToken.CustomerID, "", string(hash), nil); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error actualizando contraseña"})
			return
		}

		db.MarkResetTokenUsed(database, req.Token)
		db.AddAuditLog(database, &resetToken.CustomerID, "PASSWORD_RESET", "contraseña restablecida", c.ClientIP())

		// Alerta de seguridad — mismo motivo que en HandlerUpdateProfile.
		if customer, err := db.GetCustomerByID(database, resetToken.CustomerID); err == nil && customer.Email != nil && *customer.Email != "" {
			lang := c.GetHeader("X-Lang")
			if lang == "" { lang = "es" }
			go sendPasswordChangedEmail(smtpConfig, *customer.Email, customer.EpicUsername, lang)
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "message": "contraseña actualizada correctamente"})
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

func sendEmailChangeOTP(cfg types.EnvConfig, toEmail, code, username, lang string) {
	if cfg.ResendAPIKey == "" && cfg.SMTPHost == "" {
		slog.Info("Email: codigo de cambio de correo (sin proveedor de email configurado)", "to", toEmail, "code", code)
		return
	}

	es := lang != "en"
	subject := "Tu código de verificación — KidStorePeru"
	if !es { subject = "Your verification code — KidStorePeru" }

	var htmlBody string
	if es {
		htmlBody = buildOTPEmailES(username, code)
	} else {
		htmlBody = buildOTPEmailEN(username, code)
	}

	if err := sendEmail(cfg, toEmail, subject, htmlBody); err != nil {
		slog.Error("Email: error enviando codigo de cambio de correo", "to", toEmail, "error", err)
	} else {
		slog.Info("Email: codigo de cambio de correo enviado", "to", toEmail)
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

func buildOTPEmailES(username, code string) string {
	content := fmt.Sprintf(`
<h1 style="margin:0 0 8px;font-size:24px;font-weight:800;color:#ffffff;">Verifica tu nuevo correo</h1>
<p style="margin:0 0 24px;font-size:15px;color:#8b8ba7;line-height:1.6;">Hola <strong style="color:#ffffff;">%s</strong>, usa este código para confirmar el cambio de correo en tu cuenta de KidStorePeru.</p>
<div style="background:linear-gradient(135deg,#1a0a2e,#0d1117);border:1px solid #2d1f4e;border-radius:14px;padding:28px;margin:0 0 24px;text-align:center;">
  <p style="margin:0 0 14px;font-size:13px;color:#6b6b8a;text-transform:uppercase;letter-spacing:1px;font-weight:600;">Tu código de verificación</p>
  <span style="display:inline-block;font-size:34px;font-weight:900;letter-spacing:8px;color:#ffffff;font-family:monospace;">%s</span>
</div>
<div style="border-top:1px solid #1e1e3a;padding-top:20px;">
  <p style="margin:0;font-size:12px;color:#4a4a6a;">⏰ Este código expira en <strong style="color:#6b6b8a;">15 minutos</strong>. Si no solicitaste este cambio, ignora este correo — tu email no cambiará.</p>
</div>`, username, code)
	return emailBase("Tu código de verificación — KidStorePeru", "Confirma el cambio de correo de tu cuenta", content)
}

func buildOTPEmailEN(username, code string) string {
	content := fmt.Sprintf(`
<h1 style="margin:0 0 8px;font-size:24px;font-weight:800;color:#ffffff;">Verify your new email</h1>
<p style="margin:0 0 24px;font-size:15px;color:#8b8ba7;line-height:1.6;">Hi <strong style="color:#ffffff;">%s</strong>, use this code to confirm the email change on your KidStorePeru account.</p>
<div style="background:linear-gradient(135deg,#1a0a2e,#0d1117);border:1px solid #2d1f4e;border-radius:14px;padding:28px;margin:0 0 24px;text-align:center;">
  <p style="margin:0 0 14px;font-size:13px;color:#6b6b8a;text-transform:uppercase;letter-spacing:1px;font-weight:600;">Your verification code</p>
  <span style="display:inline-block;font-size:34px;font-weight:900;letter-spacing:8px;color:#ffffff;font-family:monospace;">%s</span>
</div>
<div style="border-top:1px solid #1e1e3a;padding-top:20px;">
  <p style="margin:0;font-size:12px;color:#4a4a6a;">⏰ This code expires in <strong style="color:#6b6b8a;">15 minutes</strong>. If you didn't request this change, ignore this email — your email won't change.</p>
</div>`, username, code)
	return emailBase("Your verification code — KidStorePeru", "Confirm your account's email change", content)
}
