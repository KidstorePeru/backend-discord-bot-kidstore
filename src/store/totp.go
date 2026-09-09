package store

import (
	"KidStoreStore/src/crypto"
	"KidStoreStore/src/db"
	"KidStoreStore/src/middleware"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

// ==================== 2FA (TOTP) PARA CUENTAS ADMIN ====================
//
// Segundo factor con Google Authenticator/Authy (TOTP estándar, RFC 6238)
// solo para cuentas is_admin=true — el panel admin controla dinero real
// (recargas de KC), tokens de las cuentas bot y datos de clientes, así que
// justifica el paso extra que a un cliente normal le sobraría.
//
// Flujo:
//  1. HandlerSetup2FA: genera un secreto nuevo (queda "pendiente", aún no
//     protege el login) y devuelve el QR/otpauth para escanear.
//  2. HandlerConfirm2FA: confirma con un código real generado por esa app,
//     recién ahí totp_enabled pasa a true. Entrega 8 códigos de respaldo
//     de un solo uso (para cuando no tengas el teléfono a mano).
//  3. A partir de ahí, HandlerLogin (auth.go) ya NO entrega el token
//     directo para esta cuenta — entrega un token temporal de 5 minutos
//     que solo sirve para HandlerLoginVerify2FA.
//  4. HandlerDisable2FA: apaga el 2FA (pide la contraseña actual).

func hashBackupCode(code string) string {
	h := sha256.Sum256([]byte(strings.ToUpper(strings.ReplaceAll(code, "-", ""))))
	return hex.EncodeToString(h[:])
}

// generateBackupCodes crea 8 códigos de un solo uso, formato "XXXX-XXXX"
// (mayúsculas + dígitos, sin caracteres ambiguos como 0/O o 1/I).
func generateBackupCodes() (plain []string, hashes []string, err error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	for i := 0; i < 8; i++ {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return nil, nil, err
		}
		code := make([]byte, 8)
		for j, v := range b {
			code[j] = alphabet[int(v)%len(alphabet)]
		}
		formatted := string(code[:4]) + "-" + string(code[4:])
		plain = append(plain, formatted)
		hashes = append(hashes, hashBackupCode(formatted))
	}
	return plain, hashes, nil
}

// HandlerSetup2FA genera un secreto TOTP nuevo para la cuenta autenticada.
// Solo cuentas admin pueden usarlo. El secreto queda cifrado en DB y
// "pendiente" — no protege nada todavía hasta que se confirma con
// HandlerConfirm2FA.
func HandlerSetup2FA(database *sql.DB) gin.HandlerFunc {
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
		if !customer.IsAdmin {
			c.JSON(http.StatusForbidden, gin.H{"success": false, "error": "el 2FA solo está disponible para cuentas admin"})
			return
		}

		accountName := customer.EpicUsername
		if customer.Email != nil && *customer.Email != "" {
			accountName = *customer.Email
		}
		key, err := totp.Generate(totp.GenerateOpts{
			Issuer:      "KidStorePeru",
			AccountName: accountName,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error generando secreto 2FA"})
			return
		}

		encSecret, err := crypto.Encrypt(key.Secret(), encryptionKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error cifrando secreto"})
			return
		}
		if err := db.SetPendingTOTPSecret(database, customerID, encSecret); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error guardando secreto"})
			return
		}

		db.AddAuditLog(database, &customerID, "2FA_SETUP_STARTED", "generó un nuevo secreto 2FA (aún sin confirmar)", c.ClientIP())

		c.JSON(http.StatusOK, gin.H{
			"success":     true,
			"secret":      key.Secret(), // para ingreso manual si no puede escanear el QR
			"otpauth_url": key.String(), // el frontend genera el QR localmente a partir de esto
		})
	}
}

// HandlerConfirm2FA valida el primer código real generado con el secreto
// pendiente y, si es correcto, activa el 2FA de verdad y entrega los
// códigos de respaldo (una sola vez — no se pueden volver a consultar).
func HandlerConfirm2FA(database *sql.DB) gin.HandlerFunc {
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
		var req struct {
			Code string `json:"code" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		customer, err := db.GetCustomerByID(database, customerID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "cliente no encontrado"})
			return
		}
		if !customer.IsAdmin {
			c.JSON(http.StatusForbidden, gin.H{"success": false, "error": "el 2FA solo está disponible para cuentas admin"})
			return
		}
		if customer.TOTPSecretEnc == nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "primero genera un secreto con /2fa/setup"})
			return
		}

		secret, err := crypto.Decrypt(*customer.TOTPSecretEnc, encryptionKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error leyendo secreto"})
			return
		}
		if !validateTOTPCode(customerID, strings.TrimSpace(req.Code), secret) {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "código incorrecto, verifica la hora de tu teléfono e intenta de nuevo"})
			return
		}

		if err := db.EnableTOTP(database, customerID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error activando 2FA"})
			return
		}
		plainCodes, hashes, err := generateBackupCodes()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error generando códigos de respaldo"})
			return
		}
		if err := db.CreateBackupCodes(database, customerID, hashes); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error guardando códigos de respaldo"})
			return
		}

		db.AddAuditLog(database, &customerID, "2FA_ENABLED", "activó verificación en dos pasos", c.ClientIP())
		if customer.Email != nil && *customer.Email != "" {
			lang := c.GetHeader("X-Lang")
			if lang == "" { lang = "es" }
			go sendTwoFactorEnabledEmail(smtpConfig, *customer.Email, customer.EpicUsername, lang)
		}

		c.JSON(http.StatusOK, gin.H{
			"success":      true,
			"message":      "2FA activado correctamente",
			"backup_codes": plainCodes,
		})
	}
}

// HandlerDisable2FA apaga el 2FA — exige la contraseña actual para
// confirmar que es realmente el dueño de la cuenta quien lo desactiva (y no
// alguien que ya tenía una sesión abierta en un dispositivo ajeno).
func HandlerDisable2FA(database *sql.DB) gin.HandlerFunc {
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
		var req struct {
			Password string `json:"password" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}
		customer, err := db.GetCustomerByID(database, customerID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "cliente no encontrado"})
			return
		}
		if err := bcrypt.CompareHashAndPassword([]byte(customer.PasswordHash), []byte(req.Password)); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "contraseña incorrecta"})
			return
		}
		if err := db.DisableTOTP(database, customerID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error desactivando 2FA"})
			return
		}
		db.AddAuditLog(database, &customerID, "2FA_DISABLED", "desactivó verificación en dos pasos", c.ClientIP())
		if customer.Email != nil && *customer.Email != "" {
			lang := c.GetHeader("X-Lang")
			if lang == "" { lang = "es" }
			go sendTwoFactorDisabledEmail(smtpConfig, *customer.Email, customer.EpicUsername, lang)
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "2FA desactivado"})
	}
}

// HandlerGet2FAStatus — cuántos códigos de respaldo le quedan sin usar
// (útil para que el frontend le sugiera regenerarlos si se están acabando).
func HandlerGet2FAStatus(database *sql.DB) gin.HandlerFunc {
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
		remaining := 0
		if customer.TOTPEnabled {
			remaining, _ = db.CountUnusedBackupCodes(database, customerID)
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "enabled": customer.TOTPEnabled, "backup_codes_remaining": remaining})
	}
}

// HandlerLoginVerify2FA completa el login de una cuenta admin con 2FA
// activado — recibe el token temporal de HandlerLogin y un código (TOTP de
// 6 dígitos, o un código de respaldo). Comparte el mismo bloqueo por
// intentos fallidos que el login por contraseña, para que no se pueda
// probar códigos al infinito.
func HandlerLoginVerify2FA(database *sql.DB, secretKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			TempToken string `json:"temp_token" binding:"required"`
			Code      string `json:"code" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		customerIDStr, err := middleware.Parse2FAPendingToken(req.TempToken, secretKey)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "sesión de verificación expirada, inicia sesión de nuevo"})
			return
		}
		customerID, err := uuid.Parse(customerIDStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "id inválido"})
			return
		}

		if locked, retryAfter := accountLockStatus(customerID); locked {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"success": false,
				"error":   fmt.Sprintf("Demasiados intentos fallidos. Intenta de nuevo en %d minutos.", int(retryAfter.Minutes())+1),
				"code":    "ACCOUNT_LOCKED",
			})
			return
		}

		customer, err := db.GetCustomerByID(database, customerID)
		if err != nil || !customer.IsAdmin || !customer.TOTPEnabled || customer.TOTPSecretEnc == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "cuenta inválida"})
			return
		}

		code := strings.TrimSpace(req.Code)
		valid := false

		if secret, err := crypto.Decrypt(*customer.TOTPSecretEnc, encryptionKey); err == nil {
			valid = validateTOTPCode(customerID, code, secret)
		}
		if !valid {
			// No era un código TOTP válido — probar como código de respaldo.
			if ok, _ := db.ConsumeBackupCode(database, customerID, hashBackupCode(code)); ok {
				valid = true
				db.AddAuditLog(database, &customerID, "2FA_BACKUP_CODE_USED", "inició sesión con un código de respaldo", c.ClientIP())
			}
		}

		if !valid {
			db.AddAuditLog(database, &customerID, "LOGIN_2FA_FAILED", "código 2FA incorrecto", c.ClientIP())
			if recordLoginFailure(customerID) {
				slog.Warn("Cuenta admin bloqueada temporalmente por intentos fallidos de 2FA", "customer", customerID, "ip", c.ClientIP())
			}
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "código incorrecto"})
			return
		}
		clearLoginFailures(customerID)

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
		db.AddAuditLog(database, &customer.ID, "LOGIN", "login exitoso (2FA)", c.ClientIP())

		c.JSON(http.StatusOK, gin.H{
			"success":       true,
			"token":         token,
			"refresh_token": refreshPlain,
			"customer":      customer.Public(),
		})
	}
}

// totpLastStep guarda, por cuenta, el último período TOTP (bloque de 30s)
// que se aceptó — así un código NUNCA se puede reutilizar dentro de su
// propia ventana de validez, aunque matemáticamente siga siendo "correcto".
// Sin esto, alguien que interceptara un código válido (red, malware, mirada
// indiscreta) podría reusarlo mientras siga vigente. Vive en memoria — un
// reinicio del servidor lo resetea, lo cual es aceptable: la ventana real
// de un código dura segundos, no algo que sobreviva a un redeploy de todos
// modos.
var (
	totpLastStep   = map[uuid.UUID]int64{}
	totpLastStepMu sync.Mutex
)

const totpPeriod = int64(30)

// validateTOTPCode acepta el código del período actual y un margen de ±30s
// (un período antes o después, para tolerar un teléfono con la hora
// levemente desincronizada), pero rechaza cualquier código de un período ya
// usado antes por esta misma cuenta.
func validateTOTPCode(customerID uuid.UUID, code, secret string) bool {
	now := time.Now().Unix()
	currentStep := now / totpPeriod

	totpLastStepMu.Lock()
	lastStep := totpLastStep[customerID]
	totpLastStepMu.Unlock()

	for _, step := range []int64{currentStep - 1, currentStep, currentStep + 1} {
		if step <= lastStep {
			continue // ya se usó este período (o uno más nuevo) antes — no se acepta de nuevo
		}
		expected, err := totp.GenerateCode(secret, time.Unix(step*totpPeriod, 0))
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(expected), []byte(code)) == 1 {
			totpLastStepMu.Lock()
			if step > totpLastStep[customerID] {
				totpLastStep[customerID] = step
			}
			totpLastStepMu.Unlock()
			return true
		}
	}
	return false
}
