package discord

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/middleware"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	discordAPIBase   = "https://discord.com/api/v10"
	discordTokenURL  = "https://discord.com/api/oauth2/token"
	discordAuthURL   = "https://discord.com/api/oauth2/authorize"
)

// Config holds Discord OAuth credentials
type Config struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	FrontendURL  string
	BotToken     string
	SecretKey    string
}

// HandlerGetAuthURL devuelve la URL de autorización de Discord.
// El cliente incluye su JWT en el parámetro state para identificarlo en el callback.
// GET /discord/auth
func HandlerGetAuthURL(cfg Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		// El JWT del cliente viaja en el state para recuperarlo en el callback
		token := c.Query("token")
		if token == "" {
			// Intentar desde header Authorization
			auth := c.GetHeader("Authorization")
			if strings.HasPrefix(auth, "Bearer ") {
				token = strings.TrimPrefix(auth, "Bearer ")
			}
		}
		if token == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "token requerido"})
			return
		}

		params := url.Values{}
		params.Set("client_id", cfg.ClientID)
		params.Set("redirect_uri", cfg.RedirectURL)
		params.Set("response_type", "code")
		params.Set("scope", "identify")
		params.Set("state", token) // JWT del cliente como state

		authURL := discordAuthURL + "?" + params.Encode()
		c.JSON(http.StatusOK, gin.H{"success": true, "url": authURL})
	}
}

// HandlerCallback maneja el callback de Discord OAuth.
// Discord redirige aquí con ?code=...&state=JWT_del_cliente
// GET /discord/callback
func HandlerCallback(database *sql.DB, cfg Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		code := c.Query("code")
		state := c.Query("state") // JWT del cliente

		if code == "" || state == "" {
			redirectWithError(c, cfg.FrontendURL, "missing_params")
			return
		}

		// Validar el JWT del cliente (viene en state)
		claims, err := middleware.ParseCustomerToken(state, cfg.SecretKey)
		if err != nil {
			redirectWithError(c, cfg.FrontendURL, "invalid_token")
			return
		}

		customerID, err := uuid.Parse(claims.CustomerID)
		if err != nil {
			redirectWithError(c, cfg.FrontendURL, "invalid_customer")
			return
		}

		// Intercambiar el código por un access token de Discord
		discordToken, err := exchangeCode(code, cfg)
		if err != nil {
			redirectWithError(c, cfg.FrontendURL, "exchange_failed")
			return
		}

		// Obtener info del usuario de Discord
		discordUser, err := getDiscordUser(discordToken)
		if err != nil {
			redirectWithError(c, cfg.FrontendURL, "user_fetch_failed")
			return
		}

		// Vincular Discord a la cuenta KidStorePeru
		discordUsername := discordUser.Username
		if discordUser.Discriminator != "" && discordUser.Discriminator != "0" {
			discordUsername = discordUser.Username + "#" + discordUser.Discriminator
		}

		if err := db.LinkDiscord(database, customerID, discordUser.ID, discordUsername); err != nil {
			redirectWithError(c, cfg.FrontendURL, "link_failed")
			return
		}

		db.AddAuditLog(database, &customerID, "DISCORD_OAUTH_LINKED",
			fmt.Sprintf("discord OAuth: %s (%s)", discordUsername, discordUser.ID), c.ClientIP())

		// Redirigir al frontend con éxito
		c.Redirect(http.StatusFound, cfg.FrontendURL+"/profile?discord=success")
	}
}

// ── Helpers ──

type discordTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
}

type discordUserResponse struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	Discriminator string `json:"discriminator"`
	Avatar        string `json:"avatar"`
}

func exchangeCode(code string, cfg Config) (string, error) {
	data := url.Values{}
	data.Set("client_id", cfg.ClientID)
	data.Set("client_secret", cfg.ClientSecret)
	data.Set("grant_type", "authorization_code")
	data.Set("code", code)
	data.Set("redirect_uri", cfg.RedirectURL)

	resp, err := http.PostForm(discordTokenURL, data)
	if err != nil {
		return "", fmt.Errorf("error requesting token: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discord token error: %s", string(body))
	}

	var tokenResp discordTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("error parsing token: %w", err)
	}
	return tokenResp.AccessToken, nil
}

func getDiscordUser(accessToken string) (*discordUserResponse, error) {
	req, _ := http.NewRequest("GET", discordAPIBase+"/users/@me", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error fetching user: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discord user error: %s", string(body))
	}

	var user discordUserResponse
	if err := json.Unmarshal(body, &user); err != nil {
		return nil, fmt.Errorf("error parsing user: %w", err)
	}
	return &user, nil
}

func redirectWithError(c *gin.Context, frontendURL, errCode string) {
	c.Redirect(http.StatusFound, frontendURL+"/profile?discord=error&reason="+errCode)
}
