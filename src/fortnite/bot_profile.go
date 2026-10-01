package fortnite

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/types"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ==================== PERFIL DE LA CUENTA BOT (V-Bucks y regalos) ====================

// BotProfile es lo que se lee del perfil "common_core" de una cuenta bot en
// una sola consulta a Epic: el balance real de V-Bucks y cuántos regalos
// envió en las últimas 24 horas — desde esta web o desde cualquier otro
// lado con la misma cuenta (por ejemplo, el propio juego).
type BotProfile struct {
	VBucks int
	// GiftsSentLast24h solo vale si GiftHistoryKnown es true: si Epic no
	// mandó el historial de regalos, o vino en un formato que no se
	// reconoce, no se adivina nada y remaining_gifts no se toca.
	GiftsSentLast24h int
	GiftHistoryKnown bool
}

// RemainingGifts devuelve cuántos regalos le quedan a la cuenta según Epic
// (db.DailyGiftLimit por cada 24 horas), y false si no se sabe.
func (p BotProfile) RemainingGifts() (int, bool) {
	if !p.GiftHistoryKnown {
		return 0, false
	}
	remaining := db.DailyGiftLimit - p.GiftsSentLast24h
	if remaining < 0 {
		remaining = 0
	}
	return remaining, true
}

// GetBotProfile consulta directamente el perfil "common_core" de Epic (el
// mismo endpoint MCP que usa el juego) — así ni los V-Bucks ni los regalos
// disponibles dependen de que alguien los edite a mano en el panel admin.
func GetBotProfile(database *sql.DB, account types.GameAccount) (BotProfile, error) {
	botIDClean := strings.ReplaceAll(account.ID.String(), "-", "")

	req, err := http.NewRequest("POST",
		fmt.Sprintf("%s/fortnite/api/game/v2/profile/%s/client/QueryProfile?profileId=common_core", McpGiftCatalogBaseURL, botIDClean),
		bytes.NewBufferString("{}"))
	if err != nil {
		return BotProfile{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, _, err := executeWithRefresh(database, account, req)
	if err != nil {
		return BotProfile{}, fmt.Errorf("error consultando perfil: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 204 {
		return BotProfile{}, fmt.Errorf("QueryProfile status %d: %s", resp.StatusCode, string(respBody))
	}
	return parseCommonCoreProfile(respBody, time.Now())
}

// GetRealVBucksBalance devuelve solo el balance real de V-Bucks de la
// cuenta (ver GetBotProfile).
func GetRealVBucksBalance(database *sql.DB, account types.GameAccount) (int, error) {
	profile, err := GetBotProfile(database, account)
	if err != nil {
		return 0, err
	}
	return profile.VBucks, nil
}

// parseCommonCoreProfile interpreta la respuesta de QueryProfile
// common_core. Separada de GetBotProfile para poder probarla con
// respuestas fijas.
func parseCommonCoreProfile(body []byte, now time.Time) (BotProfile, error) {
	var parsed struct {
		ProfileChanges []struct {
			Profile struct {
				Items map[string]struct {
					TemplateId string `json:"templateId"`
					Quantity   int    `json:"quantity"`
				} `json:"items"`
				Stats struct {
					Attributes struct {
						GiftHistory *struct {
							NumSent int               `json:"num_sent"`
							SentTo  map[string]string `json:"sentTo"`
							Gifts   []struct {
								Date string `json:"date"`
							} `json:"gifts"`
						} `json:"gift_history"`
					} `json:"attributes"`
				} `json:"stats"`
			} `json:"profile"`
		} `json:"profileChanges"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return BotProfile{}, fmt.Errorf("error parseando perfil: %w", err)
	}
	if len(parsed.ProfileChanges) == 0 {
		return BotProfile{}, fmt.Errorf("respuesta de perfil vacía")
	}
	profile := parsed.ProfileChanges[0].Profile

	// MtxPurchased = V-Bucks comprados; MtxGiveaway = V-Bucks regalados por
	// Epic (Crew, compensaciones). Ambos se gastan igual al enviar un regalo,
	// así que el balance real utilizable es la suma de los dos (una cuenta
	// puede tener ambos tipos a la vez). 0 es una respuesta válida.
	result := BotProfile{}
	for _, item := range profile.Items {
		if item.TemplateId == "Currency:MtxPurchased" || item.TemplateId == "Currency:MtxGiveaway" {
			result.VBucks += item.Quantity
		}
	}

	// Regalos enviados en las últimas 24 horas — el límite de Epic es por
	// ventana móvil de 24 horas, no por día calendario. "gifts" trae una
	// entrada por regalo; "sentTo" solo la fecha del último regalo a cada
	// destinatario, así que se usa únicamente si no viene "gifts".
	history := profile.Stats.Attributes.GiftHistory
	if history == nil {
		return result, nil
	}
	var dates []string
	switch {
	case history.Gifts != nil:
		for _, g := range history.Gifts {
			dates = append(dates, g.Date)
		}
	case history.SentTo != nil:
		for _, d := range history.SentTo {
			dates = append(dates, d)
		}
	case history.NumSent == 0:
		// Historial presente, pero la cuenta nunca envió un regalo.
	default:
		return result, nil // hay envíos pero sin fechas: no se adivina
	}
	since := now.Add(-24 * time.Hour)
	sent := 0
	for _, d := range dates {
		t, err := time.Parse(time.RFC3339Nano, d)
		if err != nil {
			return result, nil // formato desconocido: mejor no tocar nada
		}
		if t.After(since) {
			sent++
		}
	}
	result.GiftsSentLast24h = sent
	result.GiftHistoryKnown = true
	return result, nil
}
