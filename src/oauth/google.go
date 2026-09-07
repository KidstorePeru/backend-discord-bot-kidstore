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
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	googleUserURL  = "https://www.googleapis.com/oauth2/v2/userinfo"
)

// HandlerGoogleAuth redirige al usuario a la pantalla de consentimiento de Google.
// GET /auth/google
func HandlerGoogleAuth(cfg Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		if cfg.GoogleClientID == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "error": "login con Google no configurado"})
			return
		}
		if linkToken := c.Query("link_token"); linkToken != "" {
			if _, err := parseLinkToken(cfg, linkToken, "google"); err != nil {
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
		params.Set("client_id", cfg.GoogleClientID)
		params.Set("redirect_uri", cfg.GoogleRedirectURL)
		params.Set("response_type", "code")
		params.Set("scope", "openid email profile")
		params.Set("state", state)
		params.Set("prompt", "select_account")

		c.Redirect(http.StatusFound, googleAuthURL+"?"+params.Encode())
	}
}

type googleTokenResponse struct {
	AccessToken string `json:"access_token"`
}

type googleUserInfo struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Name          string `json:"name"`
}

// HandlerGoogleCallback procesa el "code" que Google devuelve tras el consentimiento.
// GET /auth/google/callback
func HandlerGoogleCallback(database *sql.DB, cfg Config) gin.HandlerFunc {
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

		accessToken, err := exchangeGoogleCode(code, cfg)
		if err != nil {
			slog.Error("Google OAuth: intercambio de token fallo", "error", err)
			redirectError(c, cfg.FrontendURL, "exchange_failed")
			return
		}

		user, err := getGoogleUser(accessToken)
		if err != nil {
			slog.Error("Google OAuth: error obteniendo usuario", "error", err)
			redirectError(c, cfg.FrontendURL, "user_fetch_failed")
			return
		}
		if user.ID == "" {
			redirectError(c, cfg.FrontendURL, "no_user_id")
			return
		}

		if linkToken := getAndClearLinkCookie(c, cfg); linkToken != "" {
			customerIDStr, err := parseLinkToken(cfg, linkToken, "google")
			if err != nil {
				redirectError(c, cfg.FrontendURL, "invalid_link_token")
				return
			}
			finishOAuthLink(c, database, cfg, "google", user.ID, customerIDStr, user.Name)
			return
		}

		finishOAuthLogin(c, database, cfg, "google", user.ID, emailPtr(user.Email, user.VerifiedEmail), user.Name)
	}
}

func exchangeGoogleCode(code string, cfg Config) (string, error) {
	data := url.Values{}
	data.Set("client_id", cfg.GoogleClientID)
	data.Set("client_secret", cfg.GoogleClientSecret)
	data.Set("code", code)
	data.Set("grant_type", "authorization_code")
	data.Set("redirect_uri", cfg.GoogleRedirectURL)

	resp, err := http.PostForm(googleTokenURL, data)
	if err != nil {
		return "", fmt.Errorf("error solicitando token: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("google token error: %s", string(body))
	}

	var tr googleTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("error parseando token: %w", err)
	}
	if tr.AccessToken == "" {
		return "", fmt.Errorf("access_token vacio")
	}
	return tr.AccessToken, nil
}

func getGoogleUser(accessToken string) (*googleUserInfo, error) {
	req, _ := http.NewRequest("GET", googleUserURL, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error obteniendo usuario: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google userinfo error: %s", string(body))
	}

	var u googleUserInfo
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, fmt.Errorf("error parseando usuario: %w", err)
	}
	return &u, nil
}
