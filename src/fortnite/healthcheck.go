package fortnite

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// maxConsecutiveFailures — cuántas veces seguidas debe fallar la verificación
// (perfil + refresh) antes de desactivar la cuenta de verdad. Un solo fallo
// puede ser un hipo de red o un rate-limit momentáneo de Epic — no significa
// que la sesión realmente se invalidó. Solo se desactiva tras fallar varias
// veces SEGUIDAS, y el contador se resetea apenas una verificación funciona.
const maxConsecutiveFailures = 5

var (
	failureCounts   = map[uuid.UUID]int{}
	failureCountsMu sync.Mutex
)

// StartTokenHealthCheck verifica periódicamente que los tokens de cada cuenta
// bot sigan siendo válidos. Si Epic devuelve 401/403, intenta refrescar.
// Si el refresco también falla varias veces seguidas, marca la cuenta como
// inactiva. Esto detecta cuando el dueño inició sesión directamente en el
// juego, lo que invalida todos los tokens existentes.
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
			// Error de red — no significa que el token esté mal, no cuenta como fallo.
			slog.Warn("HealthCheck: error de red verificando token, se reintentará luego", "bot", account.DisplayName, "error", err)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == 200 {
			slog.Info("HealthCheck: token OK", "bot", account.DisplayName)
			failureCountsMu.Lock()
			delete(failureCounts, account.ID) // se recuperó — resetear contador
			failureCountsMu.Unlock()
			discordbot.ClearBotDeactivatedAlert(account.ID) // token OK → si se desactiva de nuevo, avisar otra vez

			// Sincronizar el balance real de V-Bucks desde Epic — así el panel
			// admin nunca queda desactualizado ni depende de que alguien lo
			// edite a mano tras cargar pavos a la cuenta.
			if realVbucks, err := GetRealVBucksBalance(database, account); err != nil {
				slog.Warn("HealthCheck: no se pudo sincronizar V-Bucks", "bot", account.DisplayName, "error", err)
			} else {
				if realVbucks != account.VBucks {
					if err := db.UpdateBotVbucks(database, account.ID, realVbucks); err != nil {
						slog.Warn("HealthCheck: error guardando V-Bucks sincronizados", "bot", account.DisplayName, "error", err)
					} else {
						slog.Info("HealthCheck: V-Bucks sincronizados", "bot", account.DisplayName, "anterior", account.VBucks, "real", realVbucks)
					}
				}
				discordbot.CheckVBucksAlert(account.ID, account.DisplayName, realVbucks)
			}
			continue
		}

		// Solo un 401/403 significa "el token realmente ya no sirve". Cualquier
		// otra cosa (429 rate-limit de Epic, 5xx, etc.) NO es un problema del
		// token — no hay que intentar refresh ni contar como fallo, porque un
		// refresh en ese momento probablemente también choque con el mismo
		// límite de tasa y termine desactivando una cuenta que en realidad
		// está perfectamente bien.
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
			slog.Warn("HealthCheck: respuesta inesperada de Epic, no se toca el token", "bot", account.DisplayName, "status", resp.StatusCode)
			continue
		}

		// Token inválido (401/403) — intentar refresh
		slog.Info("HealthCheck: token expirado, intentando refresh", "bot", account.DisplayName, "status", resp.StatusCode)

		_, err = refreshAccessToken(database, account)
		if err != nil {
			failureCountsMu.Lock()
			failureCounts[account.ID]++
			count := failureCounts[account.ID]
			failureCountsMu.Unlock()

			if count >= maxConsecutiveFailures {
				// Refresh falló varias veces seguidas → cuenta realmente inutilizable
				// (el dueño inició sesión directamente en el juego, o algo la invalidó)
				slog.Warn("HealthCheck: refresh falló repetidamente, marcando como inactiva", "bot", account.DisplayName, "error", err, "fallos_seguidos", count)
				db.DeactivateGameAccount(database, account.ID)
				discordbot.AlertBotDeactivated(account.ID, account.DisplayName, fmt.Sprintf("el refresco de su token falló %d veces seguidas — probablemente alguien inició sesión directamente en el juego con esta cuenta", count))
				failureCountsMu.Lock()
				delete(failureCounts, account.ID)
				failureCountsMu.Unlock()
			} else {
				slog.Warn("HealthCheck: refresh falló, se reintentará en la próxima verificación", "bot", account.DisplayName, "error", err, "fallos_seguidos", count, "max", maxConsecutiveFailures)
			}
		} else {
			slog.Info("HealthCheck: token refrescado correctamente", "bot", account.DisplayName)
			failureCountsMu.Lock()
			delete(failureCounts, account.ID)
			failureCountsMu.Unlock()
		}

		// Pequeña pausa entre cuentas para no saturar la API de Epic
		time.Sleep(2 * time.Second)
	}
}
