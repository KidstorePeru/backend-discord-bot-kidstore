package middleware

import (
	"KidStoreStore/src/types"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// ==================== JWT ====================

// GenerateCustomerToken genera un JWT exclusivo para clientes.
// Incluye is_customer: true para distinguirlo de tokens admin.
func GenerateCustomerToken(customer types.Customer, secretKey string) (string, error) {
	claims := jwt.MapClaims{
		"customer_id":   customer.ID.String(),
		"epic_username": customer.EpicUsername,
		"email":         func() string { if customer.Email != nil { return *customer.Email }; return "" }(),
		"is_customer":   true,
		"is_admin":      customer.IsAdmin,
		"exp":           time.Now().Add(1 * time.Hour).Unix(),
		"iat":           time.Now().Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secretKey))
}

// GenerateRefreshToken generates a random refresh token.
// Returns the plaintext token (to send to client) and its SHA-256 hash (to store in DB).
func GenerateRefreshToken() (plaintext string, hash string, err error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", "", fmt.Errorf("generating refresh token: %w", err)
	}
	plaintext = hex.EncodeToString(tokenBytes)
	hash = HashRefreshToken(plaintext)
	return plaintext, hash, nil
}

// HashRefreshToken returns the SHA-256 hash of a refresh token.
func HashRefreshToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// ParseCustomerToken valida y parsea un JWT de cliente.
// Usado por el OAuth de Discord para verificar el state.
func ParseCustomerToken(tokenStr string, secretKey string) (*types.CustomerClaims, error) {
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("método de firma inválido")
		}
		return []byte(secretKey), nil
	})
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, fmt.Errorf("token inválido")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("claims inválidos")
	}
	isCustomer, _ := claims["is_customer"].(bool)
	if !isCustomer {
		return nil, fmt.Errorf("token no es de cliente")
	}
	isAdmin, _ := claims["is_admin"].(bool)
	return &types.CustomerClaims{
		CustomerID:   claims["customer_id"].(string),
		EpicUsername: claims["epic_username"].(string),
		Email:        claims["email"].(string),
		IsCustomer:   true,
		IsAdmin:      isAdmin,
	}, nil
}

// CustomerAuthMiddleware valida el JWT de clientes.
// Rechaza tokens admin aunque sean válidos.
// Si el token expiró devuelve 401 con mensaje claro para que el frontend redirija al login.
func CustomerAuthMiddleware(secretKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "token requerido"})
			c.Abort()
			return
		}

		tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
		token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("método de firma inválido")
			}
			return []byte(secretKey), nil
		})
		if err != nil {
			// Distinguir token expirado de token inválido
			if strings.Contains(err.Error(), "expired") {
				c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "token expirado", "code": "TOKEN_EXPIRED"})
			} else {
				c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "token inválido"})
			}
			c.Abort()
			return
		}
		if !token.Valid {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "token inválido"})
			c.Abort()
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "claims inválidos"})
			c.Abort()
			return
		}

		// SEGURIDAD: verificar que es un token de cliente, no de admin
		isCustomer, _ := claims["is_customer"].(bool)
		if !isCustomer {
			c.JSON(http.StatusForbidden, gin.H{"success": false, "error": "acceso denegado"})
			c.Abort()
			return
		}

		c.Set("customer_id", claims["customer_id"])
		c.Set("epic_username", claims["epic_username"])
		c.Set("email", claims["email"])
		c.Next()
	}
}

// AdminAuthMiddleware validates admin access via API Key (X-Admin-Key header)
// OR via JWT with is_admin=true claim. Either method grants full admin access.
func AdminAuthMiddleware(adminAPIKey, secretKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Method 1: API Key header (legacy, still works)
		apiKey := c.GetHeader("X-Admin-Key")
		if apiKey != "" && adminAPIKey != "" && apiKey == adminAPIKey {
			c.Next()
			return
		}

		// Method 2: JWT with is_admin=true
		authHeader := c.GetHeader("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
			claims, err := ParseCustomerToken(tokenStr, secretKey)
			if err == nil && claims.IsAdmin {
				c.Set("customer_id", claims.CustomerID)
				c.Set("is_admin", true)
				c.Next()
				return
			}
		}

		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "acceso denegado"})
		c.Abort()
	}
}

// ==================== RATE LIMITING ====================

// IPRateLimiter limita requests por IP usando un mapa en memoria con mutex para thread safety.
type IPRateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	limit    int
	window   time.Duration
}

func NewIPRateLimiter(limit int, window time.Duration) *IPRateLimiter {
	r := &IPRateLimiter{
		attempts: make(map[string][]time.Time),
		limit:    limit,
		window:   window,
	}
	// Limpiar entradas viejas cada minuto
	go func() {
		for range time.Tick(time.Minute) {
			now := time.Now()
			r.mu.Lock()
			for ip, times := range r.attempts {
				var valid []time.Time
				for _, t := range times {
					if now.Sub(t) < r.window {
						valid = append(valid, t)
					}
				}
				if len(valid) == 0 {
					delete(r.attempts, ip)
				} else {
					r.attempts[ip] = valid
				}
			}
			r.mu.Unlock()
		}
	}()
	return r
}

func (r *IPRateLimiter) Allow(ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	var valid []time.Time
	for _, t := range r.attempts[ip] {
		if now.Sub(t) < r.window {
			valid = append(valid, t)
		}
	}
	valid = append(valid, now)
	r.attempts[ip] = valid
	return len(valid) <= r.limit
}

// RateLimitMiddleware aplica rate limiting por IP.
func RateLimitMiddleware(limiter *IPRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()
		if !limiter.Allow(ip) {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"success": false,
				"error":   "demasiados intentos, espera un momento",
			})
			c.Abort()
			return
		}
		c.Next()
	}
}

// ==================== HELPERS ====================

// GetCustomerID extrae el customer_id del contexto Gin.
func GetCustomerID(c *gin.Context) (string, bool) {
	id, exists := c.Get("customer_id")
	if !exists {
		return "", false
	}
	idStr, ok := id.(string)
	return idStr, ok
}
