package fortnite

import (
	"KidStoreStore/src/db"
	"database/sql"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// StartTokenHealthCheck verifica periódicamente que los tokens de cada cuenta
// bot sigan siendo válidos. Si Epic devuelve 401/403, intenta refrescar.
// Si el refresco también falla, marca la cuenta como inactiva.
// Esto detecta cuando el dueño inició sesión directamente en el juego,
// lo que invalida todos los tokens existentes.
func StartTokenHealthCheck(database *sql.DB, intervalMinutes int) {
	go func() {
		// Primera verificación al iniciar (esperar 30s para que el servidor arranque)
		time.Sleep(10 * time.Second)
		checkAllTokens(database)

		ticker := time.NewTicker(time.Duration(intervalMinutes) * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			checkAllTokens(database)
		}
	}()
	slog.Info("HealthCheck: verificacion de tokens periodica", "intervalMinutes", intervalMinutes)
}

func checkAllTokens(database *sql.DB) {
	accounts, err := db.GetActiveGameAccounts(database, encryptionKey)
	if err != nil || len(accounts) == 0 {
		return
	}

	slog.Info("HealthCheck: verificando tokens", "accounts", len(accounts))

	for _, account := range accounts {
		// Llamada ligera a Epic: obtener perfil propio
		// Si el token es válido responde 200, si expiró responde 401
		botIDClean := strings.ReplaceAll(account.ID.String(), "-", "")
		req, err := http.NewRequest("GET",
			"https://account-public-service-prod.ol.epicgames.com/account/api/public/account/"+botIDClean,
			nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+account.AccessToken)

		client := &http.Client{Timeout: 8 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			slog.Warn("HealthCheck: error de red verificando token", "bot", account.DisplayName, "error", err)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == 200 {
			slog.Info("HealthCheck: token OK", "bot", account.DisplayName)
			continue
		}

		// Token inválido (401/403) — intentar refresh
		slog.Info("HealthCheck: token expirado, intentando refresh", "bot", account.DisplayName, "status", resp.StatusCode)

		_, err = refreshAccessToken(database, account)
		if err != nil {
			// Refresh también falló → cuenta inutilizable
			// (el dueño inició sesión directamente en el juego)
			slog.Warn("HealthCheck: refresh fallo, marcando como inactiva", "bot", account.DisplayName, "error", err)
			db.DeactivateGameAccount(database, account.ID)
		} else {
			slog.Info("HealthCheck: token refrescado correctamente", "bot", account.DisplayName)
		}

		// Pequeña pausa entre cuentas para no saturar la API de Epic
		time.Sleep(2 * time.Second)
	}
}
