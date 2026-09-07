package oauth

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
)

// Config agrupa las credenciales de los proveedores OAuth soportados
// (Google y Discord) usados para login/registro de clientes.
type Config struct {
	GoogleClientID     string
	GoogleClientSecret string
	GoogleRedirectURL  string

	DiscordClientID     string
	DiscordClientSecret string
	DiscordRedirectURL  string

	FrontendURL string
	SecretKey   string
}

const stateCookieName = "oauth_state"
const linkCookieName = "oauth_link_token"

// generateState crea un valor aleatorio para proteger el flujo OAuth contra CSRF.
func generateState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// isLocalFrontend determina si estamos en desarrollo local, para no exigir
// cookies "Secure" cuando el backend corre por HTTP plano.
func isLocalFrontend(frontendURL string) bool {
	return strings.Contains(frontendURL, "localhost")
}

func setStateCookie(c *gin.Context, cfg Config, state string) {
	secure := !isLocalFrontend(cfg.FrontendURL)
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(stateCookieName, state, 600, "/", "", secure, true)
}

// validateAndClearState compara el state devuelto por el proveedor OAuth
// contra la cookie que pusimos al iniciar el flujo, y siempre limpia la cookie.
func validateAndClearState(c *gin.Context, cfg Config) bool {
	cookieVal, cookieErr := c.Cookie(stateCookieName)
	state := c.Query("state")

	secure := !isLocalFrontend(cfg.FrontendURL)
	c.SetCookie(stateCookieName, "", -1, "/", "", secure, true)

	if cookieErr != nil || cookieVal == "" || state == "" || cookieVal != state {
		return false
	}
	return true
}

func setLinkCookie(c *gin.Context, cfg Config, linkToken string) {
	secure := !isLocalFrontend(cfg.FrontendURL)
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(linkCookieName, linkToken, 600, "/", "", secure, true)
}

// getAndClearLinkCookie devuelve el link_token de la cookie (si existe) y
// siempre la limpia — se usa una sola vez, en el callback del proveedor.
func getAndClearLinkCookie(c *gin.Context, cfg Config) string {
	val, err := c.Cookie(linkCookieName)
	secure := !isLocalFrontend(cfg.FrontendURL)
	c.SetCookie(linkCookieName, "", -1, "/", "", secure, true)
	if err != nil {
		return ""
	}
	return val
}

func redirectError(c *gin.Context, frontendURL, reason string) {
	c.Redirect(http.StatusFound, frontendURL+"/login?oauth_error="+url.QueryEscape(reason))
}

// emailPtr solo devuelve el correo si el proveedor lo marco como verificado —
// de lo contrario no podemos usarlo para vincular cuentas existentes por email.
func emailPtr(email string, verified bool) *string {
	if email == "" || !verified {
		return nil
	}
	e := email
	return &e
}
