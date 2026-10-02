package store

import (
	"KidStoreStore/src/db"
	"database/sql"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// HandlerBotsStatus devuelve el estado de las cuentas bot SIN exponer tokens.
// Incluye el horario configurado para que el frontend pueda mostrar
// si el servicio está dentro o fuera del horario de operación.
// Endpoint público — lo usan los clientes para ver si hay bots disponibles.
func HandlerBotsStatus(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		accounts, err := db.GetAllGameAccounts(database, encryptionKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "error"})
			return
		}

		// Solo lo que muestra la página Bots: el saldo de V-Bucks de cada
		// cuenta y su antigüedad son datos internos (están en el panel admin).
		type SafeAccount struct {
			ID             string `json:"id"`
			DisplayName    string `json:"display_name"`
			RemainingGifts int    `json:"remaining_gifts"`
			IsActive       bool   `json:"is_active"`
			// Amigos actuales en Epic (nil = sin sincronizar todavía) y el
			// límite: a una cuenta llena no se le puede mandar solicitud.
			FriendsCount *int `json:"friends_count"`
			FriendsLimit int  `json:"friends_limit"`
		}

		safe := make([]SafeAccount, 0, len(accounts))
		for _, a := range accounts {
			safe = append(safe, SafeAccount{
				ID:             a.ID.String(),
				DisplayName:    a.DisplayName,
				RemainingGifts: a.RemainingGifts,
				IsActive:       a.IsActive,
				FriendsCount:   a.FriendsCount,
				FriendsLimit:   db.BotFriendsLimit,
			})
		}

		// Horario de operación
		inSchedule, reason := db.IsWithinSchedule(database)
		schedule, _ := db.GetBotSchedule(database)

		// Hora actual en Lima
		loc, _ := time.LoadLocation(schedule.Timezone)
		if loc == nil {
			loc = time.UTC
		}
		nowLima := time.Now().In(loc).Format("15:04")

		c.JSON(http.StatusOK, gin.H{
			"success":     true,
			"accounts":    safe,
			"in_schedule": inSchedule,
			"reason":      reason,
			"schedule": gin.H{
				"enabled":    schedule.Enabled,
				"start_hour": schedule.StartHour,
				"end_hour":   schedule.EndHour,
				"timezone":   schedule.Timezone,
			},
			"current_time": nowLima,
		})
	}
}
