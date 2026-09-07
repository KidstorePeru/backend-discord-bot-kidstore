package oauth

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
)

const (
	discordAuthURL  = "https://discord.com/api/oauth2/authorize"
	discordTokenURL = "https://discord.com/api/oauth2/token"
	discordUserURL  = "https://discord.com/api/v10/users/@me"
)

// HandlerDiscordAuth redirige al usuario a la pantalla de consentimiento de Discord.
// GET /auth/discord
func HandlerDiscordAuth(cfg Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if cfg.DiscordClientID == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "error": "login con Discord no configurado"})
			return
		}
		if linkToken := c.Query("link_token"); linkToken != "" {
			if _, err := parseLinkToken(cfg, linkToken, "discord"); err != nil {
				redirectError(c, cfg.FrontendURL, "invalid_link_token")
				return
			}
			setLinkCookie(c, cfg, linkToken)
		}

		state, err := generateState()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error interno"})
			return
		}
		setStateCookie(c, cfg, state)

		params := url.Values{}
		params.Set("client_id", cfg.DiscordClientID)
		params.Set("redirect_uri", cfg.DiscordRedirectURL)
		params.Set("response_type", "code")
		params.Set("scope", "identify email")
		params.Set("state", state)

		c.Redirect(http.StatusFound, discordAuthURL+"?"+params.Encode())
	}
}

type discordTokenResponse struct {
	AccessToken string `json:"access_token"`
}

type discordUserInfo struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	Discriminator string `json:"discriminator"`
	Email         string `json:"email"`
	Verified      bool   `json:"verified"`
}

// HandlerDiscordCallback procesa el "code" que Discord devuelve tras el consentimiento.
// GET /auth/discord/callback
func HandlerDiscordCallback(database *sql.DB, cfg Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !validateAndClearState(c, cfg) {
			redirectError(c, cfg.FrontendURL, "invalid_state")
			return
		}
		code := c.Query("code")
		if code == "" {
			redirectError(c, cfg.FrontendURL, "missing_code")
			return
		}

		accessToken, err := exchangeDiscordCode(code, cfg)
		if err != nil {
			slog.Error("Discord OAuth: intercambio de token fallo", "error", err)
			redirectError(c, cfg.FrontendURL, "exchange_failed")
			return
		}

		user, err := getDiscordUser(accessToken)
		if err != nil {
			slog.Error("Discord OAuth: error obteniendo usuario", "error", err)
			redirectError(c, cfg.FrontendURL, "user_fetch_failed")
			return
		}
		if user.ID == "" {
			redirectError(c, cfg.FrontendURL, "no_user_id")
			return
		}

		username := user.Username
		if user.Discriminator != "" && user.Discriminator != "0" {
			username = user.Username + "#" + user.Discriminator
		}

		if linkToken := getAndClearLinkCookie(c, cfg); linkToken != "" {
			customerIDStr, err := parseLinkToken(cfg, linkToken, "discord")
			if err != nil {
				redirectError(c, cfg.FrontendURL, "invalid_link_token")
				return
			}
			finishOAuthLink(c, database, cfg, "discord", user.ID, customerIDStr, username)
			return
		}

		finishOAuthLogin(c, database, cfg, "discord", user.ID, emailPtr(user.Email, user.Verified), username)
	}
}

func exchangeDiscordCode(code string, cfg Config) (string, error) {
	data := url.Values{}
	data.Set("client_id", cfg.DiscordClientID)
	data.Set("client_secret", cfg.DiscordClientSecret)
	data.Set("grant_type", "authorization_code")
	data.Set("code", code)
	data.Set("redirect_uri", cfg.DiscordRedirectURL)

	resp, err := http.PostForm(discordTokenURL, data)
	if err != nil {
		return "", fmt.Errorf("error solicitando token: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discord token error: %s", string(body))
	}

	var tr discordTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("error parseando token: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("access_token vacio")
	}
	return tr.AccessToken, nil
}

func getDiscordUser(accessToken string) (*discordUserInfo, error) {
	req, _ := http.NewRequest("GET", discordUserURL, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error obteniendo usuario: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discord user error: %s", string(body))
	}

	var u discordUserInfo
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, fmt.Errorf("error parseando usuario: %w", err)
	}
	return &u, nil
}
