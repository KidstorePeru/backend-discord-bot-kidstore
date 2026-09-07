package oauth

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"
	"KidStoreStore/src/middleware"
	"KidStoreStore/src/types"
	"crypto/rand"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// HandlerGetPendingRegistration devuelve los datos publicos de un registro
// OAuth pendiente (para que el frontend salude al usuario / muestre el
// proveedor) sin exponer nada sensible.
// GET /auth/pending/:token
func HandlerGetPendingRegistration(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		pending, err := db.GetPendingOAuthRegistration(database, c.Param("token"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"success": false, "error": "enlace invalido o expirado"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"success":      true,
			"provider":     pending.Provider,
			"display_name": pending.DisplayName,
			"email":        pending.Email,
		})
	}
}

// HandlerCompleteRegistration crea la cuenta definitiva una vez que el
// cliente eligio su usuario Epic tras autenticarse con Google/Discord.
// POST /auth/complete-registration
func HandlerCompleteRegistration(database *sql.DB, cfg Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req types.CompleteOAuthRegistrationRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}
		epicUsername := strings.TrimSpace(req.EpicUsername)

		pending, err := db.GetPendingOAuthRegistration(database, req.Token)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "el enlace expiro o es invalido, inicia sesion de nuevo"})
			return
		}

		if db.EpicUsernameExists(database, epicUsername) {
			c.JSON(http.StatusConflict, gin.H{"success": false, "error": "ese usuario Epic ya esta en uso"})
			return
		}
		if pending.Email != nil && db.EmailExists(database, *pending.Email) {
			c.JSON(http.StatusConflict, gin.H{"success": false, "error": "ese correo ya esta registrado, inicia sesion en su lugar"})
			return
		}

		// Password aleatoria e inutilizable — la cuenta solo entra por OAuth
		// hasta que el cliente configure una contrasena propia desde su perfil.
		randomBytes := make([]byte, 32)
		if _, err := rand.Read(randomBytes); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error interno"})
			return
		}
		hash, err := bcrypt.GenerateFromPassword(randomBytes, bcrypt.DefaultCost)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error interno"})
			return
		}

		displayName := ""
		if pending.DisplayName != nil {
			displayName = *pending.DisplayName
		}

		customer, err := db.CreateOAuthCustomer(database, epicUsername, pending.Email, string(hash), pending.Provider, pending.ProviderID, displayName)
		if err != nil {
			if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
				c.JSON(http.StatusConflict, gin.H{"success": false, "error": "usuario Epic o correo ya en uso"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error creando la cuenta"})
			return
		}
		db.DeletePendingOAuthRegistration(database, req.Token)
		db.AddAuditLog(database, &customer.ID, "REGISTER_OAUTH", "cuenta creada via "+pending.Provider, c.ClientIP())
		discordbot.NotifyWelcome(customer)

		token, err := middleware.GenerateCustomerToken(customer, cfg.SecretKey)
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

		c.JSON(http.StatusOK, gin.H{
			"success":       true,
			"token":         token,
			"refresh_token": refreshPlain,
			"customer": customer.Public(),
		})
	}
}
