package oauth

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/middleware"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const linkTokenPurpose = "oauth_link"

// generateLinkToken crea un token firmado y de corta duración que prueba que
// el cliente autenticado (via JWT normal) pidió vincular `provider` a su
// cuenta. Viaja en una cookie durante el ida-y-vuelta con el proveedor OAuth
// porque esa navegación es de página completa y no puede llevar el header
// Authorization.
func generateLinkToken(cfg Config, customerID, provider string) (string, error) {
	claims := jwt.MapClaims{
		"purpose":     linkTokenPurpose,
		"customer_id": customerID,
		"provider":    provider,
		"exp":         time.Now().Add(10 * time.Minute).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(cfg.SecretKey))
}

func parseLinkToken(cfg Config, tokenStr, expectedProvider string) (string, error) {
	if tokenStr == "" {
		return "", fmt.Errorf("token de vinculación vacío")
	}
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		return []byte(cfg.SecretKey), nil
	})
	if err != nil || !token.Valid {
		return "", fmt.Errorf("token de vinculación inválido o expirado")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || claims["purpose"] != linkTokenPurpose || claims["provider"] != expectedProvider {
		return "", fmt.Errorf("token de vinculación inválido")
	}
	customerID, ok := claims["customer_id"].(string)
	if !ok || customerID == "" {
		return "", fmt.Errorf("token de vinculación inválido")
	}
	return customerID, nil
}

// HandlerStartLink genera el token de vinculación para el cliente autenticado.
// POST /store/link/:provider/start
func HandlerStartLink(cfg Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		provider := c.Param("provider")
		if provider != "google" && provider != "discord" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "proveedor inválido"})
			return
		}
		if (provider == "google" && cfg.GoogleClientID == "") || (provider == "discord" && cfg.DiscordClientID == "") {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "error": "proveedor no configurado"})
			return
		}
		customerIDStr, ok := middleware.GetCustomerID(c)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "no autorizado"})
			return
		}
		linkToken, err := generateLinkToken(cfg, customerIDStr, provider)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error interno"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "link_token": linkToken})
	}
}

// HandlerUnlinkProvider desvincula Google o Discord de la cuenta autenticada,
// siempre que le quede al menos otro método de acceso (contraseña u otro
// proveedor) — para no dejar al cliente sin forma de iniciar sesión.
// DELETE /store/link/:provider
func HandlerUnlinkProvider(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		provider := c.Param("provider")
		if provider != "google" && provider != "discord" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "proveedor inválido"})
			return
		}
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

		if provider == "google" && customer.GoogleID == nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "no tienes Google vinculado"})
			return
		}
		if provider == "discord" && customer.DiscordID == nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "no tienes Discord vinculado"})
			return
		}

		remainingMethods := 0
		if customer.HasPassword {
			remainingMethods++
		}
		if customer.GoogleID != nil {
			remainingMethods++
		}
		if customer.DiscordID != nil {
			remainingMethods++
		}
		if remainingMethods <= 1 {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "no puedes desvincular tu único método de acceso — configura una contraseña o vincula otra cuenta primero"})
			return
		}

		if provider == "google" {
			err = db.UnlinkGoogleID(database, customerID)
		} else {
			err = db.UnlinkDiscordID(database, customerID)
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error desvinculando cuenta"})
			return
		}
		db.AddAuditLog(database, &customerID, "OAUTH_UNLINKED", provider+" desvinculado", c.ClientIP())

		updatedCustomer, err := db.GetCustomerByID(database, customerID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error obteniendo cliente"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "customer": updatedCustomer.Public()})
	}
}
