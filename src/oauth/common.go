package oauth

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"
	"KidStoreStore/src/middleware"
	"KidStoreStore/src/types"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// finishOAuthLogin resuelve que hacer despues de un login exitoso con un
// proveedor OAuth (Google o Discord):
//  1. Si esta cuenta ya esta vinculada a un cliente -> inicia sesion.
//  2. Si el correo (verificado por el proveedor) coincide con un cliente
//     existente -> vincula el proveedor a esa cuenta e inicia sesion.
//  3. Si no existe ningun cliente -> crea un registro pendiente y redirige
//     al frontend para pedir el usuario Epic antes de crear la cuenta.
func finishOAuthLogin(c *gin.Context, database *sql.DB, cfg Config, provider, providerID string, email *string, displayName string) {
	var existing types.Customer
	var err error
	if provider == "google" {
		existing, err = db.GetCustomerByGoogleID(database, providerID)
	} else {
		existing, err = db.GetCustomerByDiscordID(database, providerID)
	}
	if err == nil {
		issueLoginRedirect(c, database, cfg, existing)
		return
	}

	if email != nil {
		if byEmail, err2 := db.GetCustomerByEmail(database, *email); err2 == nil {
			if provider == "google" {
				db.LinkGoogleID(database, byEmail.ID, providerID)
			} else {
				db.LinkDiscordID(database, byEmail.ID, providerID, displayName)
			}
			db.AddAuditLog(database, &byEmail.ID, "OAUTH_LINKED", fmt.Sprintf("%s vinculado a cuenta existente", provider), c.ClientIP())
			if provider == "discord" {
				if updated, err3 := db.GetCustomerByID(database, byEmail.ID); err3 == nil {
					discordbot.NotifyWelcome(updated)
				}
			}
			issueLoginRedirect(c, database, cfg, byEmail)
			return
		}
	}

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		redirectError(c, cfg.FrontendURL, "internal_error")
		return
	}
	pendingToken := hex.EncodeToString(tokenBytes)
	if err := db.CreatePendingOAuthRegistration(database, provider, providerID, email, &displayName, pendingToken); err != nil {
		slog.Error("OAuth: error creando registro pendiente", "provider", provider, "error", err)
		redirectError(c, cfg.FrontendURL, "internal_error")
		return
	}
	c.Redirect(http.StatusFound, cfg.FrontendURL+"/auth/complete?token="+pendingToken)
}

// finishOAuthLink vincula un proveedor OAuth a una cuenta ya autenticada
// (el cliente lo pidió explícitamente desde Seguridad, no es un login).
func finishOAuthLink(c *gin.Context, database *sql.DB, cfg Config, provider, providerID, customerIDStr, displayName string) {
	customerID, err := uuid.Parse(customerIDStr)
	if err != nil {
		redirectLinkError(c, cfg.FrontendURL, provider, "internal_error")
		return
	}

	var existing types.Customer
	if provider == "google" {
		existing, err = db.GetCustomerByGoogleID(database, providerID)
	} else {
		existing, err = db.GetCustomerByDiscordID(database, providerID)
	}
	if err == nil && existing.ID != customerID {
		redirectLinkError(c, cfg.FrontendURL, provider, "already_linked_elsewhere")
		return
	}

	if provider == "google" {
		err = db.LinkGoogleID(database, customerID, providerID)
	} else {
		err = db.LinkDiscordID(database, customerID, providerID, displayName)
	}
	if err != nil {
		redirectLinkError(c, cfg.FrontendURL, provider, "link_failed")
		return
	}
	db.AddAuditLog(database, &customerID, "OAUTH_LINKED", fmt.Sprintf("%s vinculado desde Seguridad", provider), c.ClientIP())
	if provider == "discord" {
		if updated, err3 := db.GetCustomerByID(database, customerID); err3 == nil {
			discordbot.NotifyWelcome(updated)
		}
	}
	c.Redirect(http.StatusFound, cfg.FrontendURL+"/account/security?linked="+provider)
}

func redirectLinkError(c *gin.Context, frontendURL, provider, reason string) {
	c.Redirect(http.StatusFound, frontendURL+"/account/security?link_error="+url.QueryEscape(reason)+"&provider="+url.QueryEscape(provider))
}

func issueLoginRedirect(c *gin.Context, database *sql.DB, cfg Config, customer types.Customer) {
	token, err := middleware.GenerateCustomerToken(customer, cfg.SecretKey)
	if err != nil {
		redirectError(c, cfg.FrontendURL, "token_error")
		return
	}
	refreshPlain, refreshHash, err := middleware.GenerateRefreshToken()
	if err != nil {
		redirectError(c, cfg.FrontendURL, "token_error")
		return
	}
	db.CreateRefreshToken(database, customer.ID, refreshHash, time.Now().Add(7*24*time.Hour))
	db.AddAuditLog(database, &customer.ID, "LOGIN_OAUTH", "login via OAuth", c.ClientIP())

	redirectURL := fmt.Sprintf("%s/auth/callback?token=%s&refresh_token=%s",
		cfg.FrontendURL, url.QueryEscape(token), url.QueryEscape(refreshPlain))
	c.Redirect(http.StatusFound, redirectURL)
}
