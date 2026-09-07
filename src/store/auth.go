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
	"sync"
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
//
// El límite de intentos que ya existe (authLimiter, 5/min) es por IP — no
// alcanza contra alguien con varias IPs (proxies, botnet) probando
// contraseñas contra UNA sola cuenta puntual. Este segundo control es por
// CUENTA, sin importar desde cuántas IPs distintas vengan los intentos.

const (
	maxLoginFailures    = 8
	loginFailureWindow  = 15 * time.Minute
	accountLockDuration = 15 * time.Minute
)

// dummyPasswordHash es un hash bcrypt fijo (de una contraseña que no le
// pertenece a nadie) que se usa solo para "gastar" el mismo tiempo de CPU
// que gastaría una comparación real, cuando el correo ni siquiera existe —
// ver el comentario en HandlerLogin.
const dummyPasswordHash = "$2a$10$VEnXUQTbaN67NtB5RkYs9ekbic9gO9wCPMo2.4ce/u2/wt91MWI0a"

var (
	loginFailuresMu sync.Mutex
	loginFailures   = map[uuid.UUID][]time.Time{}
)

// recordLoginFailure suma un intento fallido para esta cuenta. Devuelve
// true si con este intento se alcanzó el máximo y la cuenta queda
// bloqueada temporalmente.
func recordLoginFailure(customerID uuid.UUID) bool {
	loginFailuresMu.Lock()
	defer loginFailuresMu.Unlock()
	now := time.Now()
	var recent []time.Time
	for _, t := range loginFailures[customerID] {
		if now.Sub(t) < loginFailureWindow {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	loginFailures[customerID] = recent
	return len(recent) >= maxLoginFailures
}

// accountLockStatus dice si la cuenta está bloqueada ahora mismo y cuánto
// falta para que se libere.
func accountLockStatus(customerID uuid.UUID) (locked bool, retryAfter time.Duration) {
	loginFailuresMu.Lock()
	defer loginFailuresMu.Unlock()
	times := loginFailures[customerID]
	if len(times) < maxLoginFailures {
		return false, 0
	}
	elapsed := time.Since(times[len(times)-1])
	if elapsed >= accountLockDuration {
		return false, 0
	}
	return true, accountLockDuration - elapsed
}

func clearLoginFailures(customerID uuid.UUID) {
	loginFailuresMu.Lock()
	defer loginFailuresMu.Unlock()
	delete(loginFailures, customerID)
}

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
			// Comparación bcrypt "de mentira" contra un hash fijo — sin esto,
			// un correo inexistente responde casi instantáneo mientras que uno
			// real (con contraseña incorrecta) tarda lo que tarda bcrypt. Esa
			// diferencia de tiempo es suficiente para que alguien adivine, uno
			// por uno, qué correos SÍ están registrados en el sitio, sin
			// necesitar acertar ninguna contraseña.
			bcrypt.CompareHashAndPassword([]byte(dummyPasswordHash), []byte(req.Password))
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "credenciales inválidas"})
			return
		}

		if locked, retryAfter := accountLockStatus(customer.ID); locked {
			db.AddAuditLog(database, &customer.ID, "LOGIN_BLOCKED", "cuenta bloqueada temporalmente por intentos fallidos", c.ClientIP())
			c.JSON(http.StatusTooManyRequests, gin.H{
				"success": false,
				"error":   fmt.Sprintf("Demasiados intentos fallidos. Intenta de nuevo en %d minutos.", int(retryAfter.Minutes())+1),
				"code":    "ACCOUNT_LOCKED",
			})
			return
		}

		if err := bcrypt.CompareHashAndPassword([]byte(customer.PasswordHash), []byte(req.Password)); err != nil {
			db.AddAuditLog(database, &customer.ID, "LOGIN_FAILED", "intento fallido", c.ClientIP())
			if recordLoginFailure(customer.ID) {
				slog.Warn("Cuenta bloqueada temporalmente por demasiados intentos fallidos", "customer", customer.ID, "ip", c.ClientIP())
			}
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "credenciales inválidas"})
			return
		}
		clearLoginFailures(customer.ID)

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

// ==================== LOGOUT ====================

// HandlerLogout revoca el refresh token del dispositivo actual — antes
// "cerrar sesión" solo borraba el token del navegador, pero el refresh
// token seguía siendo válido en el servidor hasta sus 7 días completos. Si
// alguien lo hubiera copiado (dispositivo compartido, malware, backup del
// navegador), "cerrar sesión" no le quitaba el acceso. Ahora sí lo revoca
// de verdad. No requiere que el refresh token sea de quien llama — poseer
// el valor exacto (256 bits al azar) ya es la prueba de que es su propia
// sesión la que se está cerrando.
func HandlerLogout(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req types.RefreshTokenRequest
		if err := c.ShouldBindJSON(&req); err != nil || req.RefreshToken == "" {
			// Sin refresh token no hay nada que revocar del lado del servidor
			// (p. ej. una sesión que ya expiró) — no es un error real.
			c.JSON(http.StatusOK, gin.H{"success": true})
			return
		}
		tokenHash := middleware.HashRefreshToken(req.RefreshToken)
		db.DeleteRefreshToken(database, tokenHash)
		c.JSON(http.StatusOK, gin.H{"success": true})
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

		if newHash != "" {
			// Si cambió la contraseña, se revocan TODAS las sesiones activas
			// (refresh tokens) de la cuenta — si alguien más tenía un token
			// robado de antes del cambio, no debe poder seguir renovando su
			// sesión indefinidamente solo porque el dueño cambió la contraseña
			// desde otro lado. El propio dispositivo actual sigue funcionando
			// hasta que su token de acceso (1h) expire, y ahí tendrá que
			// volver a iniciar sesión con la contraseña nueva — como cualquier
			// otro.
			db.DeleteAllRefreshTokensForCustomer(database, customerID)

			// Alerta de seguridad: si no fue el dueño real quien la cambió,
			// esta es la única forma de que se entere a tiempo.
			if updatedCustomer.Email != nil && *updatedCustomer.Email != "" {
				lang := c.GetHeader("X-Lang")
				if lang == "" { lang = "es" }
				go sendPasswordChangedEmail(smtpConfig, *updatedCustomer.Email, updatedCustomer.EpicUsername, lang)
			}
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
		// El "data:image/..." del principio es solo una etiqueta que pone el
		// propio navegador — no prueba que los bytes decodificados sean
		// realmente una imagen. Sin este chequeo, alguien podría subir un SVG
		// (que sí puede llevar <script>) u otro archivo cualquiera disfrazado
		// de imagen. Se valida el contenido real por sus bytes, aceptando solo
		// formatos de imagen rasterizada — un SVG nunca pasa este chequeo.
		switch http.DetectContentType(decoded) {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
			// ok
		default:
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "el archivo no es una imagen válida (solo PNG, JPEG, GIF o WEBP)"})
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

		// Capturar el correo VIEJO antes de sobreescribirlo — es a donde se
		// manda el aviso de seguridad, no al nuevo (que ya recibió su propio
		// código OTP).
		previousCustomer, _ := db.GetCustomerByID(database, customerID)

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

		// Alerta de seguridad al correo ANTERIOR — si alguien más cambió el
		// correo de acceso, es la única forma de que el dueño real se entere
		// mientras todavía tenga esa bandeja vieja a mano.
		if previousCustomer.Email != nil && *previousCustomer.Email != "" {
			go SendEmailChangedNoticeEmail(smtpConfig, *previousCustomer.Email, updatedCustomer.EpicUsername, maskEmail(pending.NewEmail), "es")
		}

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

		// Mismo motivo que en HandlerUpdateProfile: revocar todas las
		// sesiones activas y avisar por correo.
		db.DeleteAllRefreshTokensForCustomer(database, resetToken.CustomerID)
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
	sendVerificationEmailNew(cfg, toEmail, username, verifyURL, lang)
}

func sendResetEmail(cfg types.EnvConfig, toEmail, token, username, lang string) {
	resetURL := fmt.Sprintf("%s/reset-password?token=%s", cfg.FrontendURL, token)
	sendResetEmailNew(cfg, toEmail, username, resetURL, lang)
}

// maskEmail oculta la mayor parte de un correo para mostrarlo en avisos de
// seguridad sin exponerlo completo — "kidplayer123@gmail.com" -> "k•••••••••3@gmail.com".
func maskEmail(email string) string {
	at := strings.Index(email, "@")
	if at <= 1 {
		return email
	}
	local, domain := email[:at], email[at:]
	if len(local) <= 2 {
		return local[:1] + "•••" + domain
	}
	masked := local[:1]
	for i := 1; i < len(local)-1; i++ {
		masked += "•"
	}
	masked += local[len(local)-1:]
	return masked + domain
}

func sendEmailChangeOTP(cfg types.EnvConfig, toEmail, code, username, lang string) {
	sendEmailChangeOTPNew(cfg, toEmail, username, code, lang)
}
