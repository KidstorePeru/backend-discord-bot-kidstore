package store

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/middleware"
	"KidStoreStore/src/types"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/smtp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// ==================== REGISTER ====================

func HandlerRegister(database *sql.DB, secretKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req types.RegisterRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		req.EpicUsername = strings.TrimSpace(req.EpicUsername)
		req.Email = strings.ToLower(strings.TrimSpace(req.Email))

		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error interno"})
			return
		}

		customer := types.Customer{
			ID:           uuid.New(),
			EpicUsername: req.EpicUsername,
			Email:        &req.Email,
			PasswordHash: string(hash),
		}

		if err := db.CreateCustomer(database, customer); err != nil {
			if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
				c.JSON(http.StatusConflict, gin.H{"success": false, "error": "email o usuario Epic ya registrado"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error al crear cuenta"})
			return
		}

		token, err := middleware.GenerateCustomerToken(customer, secretKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error generando token"})
			return
		}

		db.AddAuditLog(database, &customer.ID, "REGISTER", "nuevo cliente: "+req.EpicUsername, c.ClientIP())

		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"token":   token,
			"customer": types.CustomerPublic{
				ID: customer.ID, EpicUsername: customer.EpicUsername,
				Email: customer.Email, KCBalance: 0, CreatedAt: customer.CreatedAt,
			},
		})
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
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "credenciales inválidas"})
			return
		}

		if err := bcrypt.CompareHashAndPassword([]byte(customer.PasswordHash), []byte(req.Password)); err != nil {
			db.AddAuditLog(database, &customer.ID, "LOGIN_FAILED", "intento fallido", c.ClientIP())
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "credenciales inválidas"})
			return
		}

		token, err := middleware.GenerateCustomerToken(customer, secretKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error generando token"})
			return
		}

		db.AddAuditLog(database, &customer.ID, "LOGIN", "login exitoso", c.ClientIP())

		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"token":   token,
			"customer": types.CustomerPublic{
				ID: customer.ID, EpicUsername: customer.EpicUsername,
				Email: customer.Email, KCBalance: customer.KCBalance,
				DiscordID: customer.DiscordID, DiscordUsername: customer.DiscordUsername,
				CreatedAt: customer.CreatedAt,
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
				CreatedAt: customer.CreatedAt,
			},
		})
	}
}

// ==================== UPDATE PROFILE ====================

// HandlerUpdateProfile permite al cliente actualizar su epic_username, email y/o contraseña.
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

		// Si quiere cambiar email o contraseña, validar la contraseña actual
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

		// Generar nuevo token con datos actualizados
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
				CreatedAt: updatedCustomer.CreatedAt,
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

		// Siempre responder OK para no revelar si el email existe
		customer, err := db.GetCustomerByEmail(database, req.Email)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"success": true, "message": "Si el correo existe, recibirás un enlace"})
			return
		}

		// Generar token aleatorio
		tokenBytes := make([]byte, 32)
		rand.Read(tokenBytes)
		token := hex.EncodeToString(tokenBytes)

		if err := db.CreatePasswordResetToken(database, customer.ID, token); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error interno"})
			return
		}

		// Enviar email (fire and forget)
		go sendResetEmail(cfg, req.Email, token, customer.EpicUsername)

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

// ==================== HELPERS ====================

func sendResetEmail(cfg types.EnvConfig, toEmail, token, username string) {
	if cfg.SMTPHost == "" {
		fmt.Printf("[Email] Reset token para %s: %s\n", toEmail, token)
		return
	}
	resetURL := fmt.Sprintf("%s/reset-password?token=%s", cfg.FrontendURL, token)
	body := fmt.Sprintf(
		"Hola %s,\r\n\r\nRecibimos una solicitud para restablecer tu contraseña en KidStorePeru.\r\n\r\nHaz clic aquí para resetear tu contraseña (válido por 1 hora):\r\n%s\r\n\r\nSi no solicitaste esto, ignora este email.\r\n\r\n— KidStorePeru",
		username, resetURL,
	)
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: Recuperar contraseña - KidStorePeru\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		cfg.SMTPFrom, toEmail, body)

	auth := smtp.PlainAuth("", cfg.SMTPUser, cfg.SMTPPassword, cfg.SMTPHost)
	smtp.SendMail(fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort), auth, cfg.SMTPFrom, []string{toEmail}, []byte(msg))
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
