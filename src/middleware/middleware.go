package middleware

import (
	"KidStoreStore/src/types"
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
		"is_customer":   true, // CRÍTICO: distingue de tokens admin
		"exp":           time.Now().Add(72 * time.Hour).Unix(),
		"iat":           time.Now().Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secretKey))
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
	return &types.CustomerClaims{
		CustomerID:   claims["customer_id"].(string),
		EpicUsername: claims["epic_username"].(string),
		Email:        claims["email"].(string),
		IsCustomer:   true,
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

// AdminAuthMiddleware valida la API Key del admin SOLO por header X-Admin-Key.
// Se eliminó la aceptación por query param para evitar que la clave quede en logs del servidor.
func AdminAuthMiddleware(adminAPIKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.GetHeader("X-Admin-Key")
		if key == "" || key != adminAPIKey || adminAPIKey == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"success": false, "error": "acceso denegado"})
			c.Abort()
			return
		}
		c.Next()
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
