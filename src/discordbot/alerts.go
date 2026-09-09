package discordbot

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/google/uuid"
)

// ==================== ALERTAS OPERATIVAS AL ADMIN ====================
//
// A diferencia de las notificaciones normales (bienvenida, compra, recarga),
// estas van por DM directo a DISCORD_ADMIN_USER_ID — son avisos de "algo
// necesita tu atención" (un bot se cayó, se quedó sin fondos, o ya no hay
// ninguno disponible para procesar pedidos). Antes de esto, la única forma
// de enterarse era revisar el panel admin manualmente.

const (
	colorAlert    = 0xEF4444 // rojo — algo dejó de funcionar
	colorWarnSoft = 0xF59E0B // ámbar — advertencia, todavía funciona
)

// alertState evita mandar el mismo aviso una y otra vez mientras la
// condición sigue activa — se manda una vez, y no se repite hasta que la
// condición se resuelva (y vuelva a ocurrir). Vive solo en memoria: un
// reinicio del servidor resetea los avisos ya mandados, lo cual está bien —
// peor es quedarse callado para siempre por un bug.
var (
	alertState   = map[string]bool{}
	alertStateMu sync.Mutex
	// Cooldown aparte para la alerta de "cero bots disponibles", que puede
	// dispararse una vez por cada pedido en cola durante una caída — sin
	// esto, un solo bache de 10 pedidos mandaría 10 DMs idénticos seguidos.
	lastNoActiveBotsAlert time.Time
)

func shouldAlert(key string) bool {
	alertStateMu.Lock()
	defer alertStateMu.Unlock()
	if alertState[key] {
		return false
	}
	alertState[key] = true
	return true
}

func clearAlert(key string) {
	alertStateMu.Lock()
	defer alertStateMu.Unlock()
	delete(alertState, key)
}

func sendAdminAlert(title, description string, color int) {
	if !Enabled() || session == nil || cfg.DiscordAdminUserID == "" {
		return
	}
	embed := &discordgo.MessageEmbed{
		Title:       title,
		Description: description,
		Color:       color,
	}
	sendDM(cfg.DiscordAdminUserID, embed)
}

// AlertBotDeactivated se llama cuando una cuenta bot se marca inactiva (el
// health-check periódico no pudo refrescar su token varias veces seguidas,
// o falló al enviar un regalo por un error de autenticación). Es la señal
// más urgente: ese bot dejó de poder cumplir pedidos hasta que alguien
// vuelva a vincularlo.
func AlertBotDeactivated(botID uuid.UUID, displayName, reason string) {
	key := "deactivated_" + botID.String()
	if !shouldAlert(key) {
		return
	}
	sendAdminAlert(
		"🔴 Bot desconectado: "+displayName,
		fmt.Sprintf("La cuenta **%s** se marcó como inactiva y ya no puede enviar regalos.\n\n**Motivo:** %s\n\nVuelve a vincularla desde el panel admin (Cuentas Bot) cuando puedas.", displayName, reason),
		colorAlert,
	)
	slog.Info("Discord alert: bot desactivado", "bot", displayName)
}

// ClearBotDeactivatedAlert se llama cuando una cuenta vuelve a vincularse
// correctamente — así, si se vuelve a desactivar más adelante, el aviso se
// manda de nuevo en vez de quedar silenciado para siempre por la vez anterior.
func ClearBotDeactivatedAlert(botID uuid.UUID) {
	clearAlert("deactivated_" + botID.String())
}

// vbucksLowThreshold: por debajo de esto se avisa "quedan pocos pavos" (a
// tiempo de recargar antes de que llegue a 0). La mayoría de items de la
// tienda cuestan entre 300 y 3000 V-Bucks.
const vbucksLowThreshold = 500

// CheckVBucksAlert se llama tras sincronizar el balance real de una cuenta
// (ver healthcheck.go). Avisa una vez al cruzar por debajo del umbral, y
// limpia el aviso apenas vuelve a estar por encima (para poder avisar de
// nuevo si vuelve a bajar más adelante).
func CheckVBucksAlert(botID uuid.UUID, displayName string, vbucks int) {
	lowKey := "vbucks_low_" + botID.String()
	zeroKey := "vbucks_zero_" + botID.String()

	if vbucks <= 0 {
		if shouldAlert(zeroKey) {
			sendAdminAlert(
				"🔴 Bot sin V-Bucks: "+displayName,
				fmt.Sprintf("**%s** se quedó en **0 V-Bucks**. No puede enviar ningún regalo pagado hasta que le cargues más.\n\nLos pedidos que le toquen fallarán automáticamente (el cliente recibe su KC de vuelta), pero mejor evitarlo cargándole pavos ahora.", displayName),
				colorAlert,
			)
			slog.Info("Discord alert: bot sin V-Bucks", "bot", displayName)
		}
		return
	}

	// Se recuperó de 0 — permitir que la alerta crítica vuelva a dispararse
	// si en el futuro llega a 0 de nuevo.
	clearAlert(zeroKey)

	if vbucks < vbucksLowThreshold {
		if shouldAlert(lowKey) {
			sendAdminAlert(
				"🟡 Pocos V-Bucks: "+displayName,
				fmt.Sprintf("**%s** tiene **%d V-Bucks** — por debajo de %d. Todavía puede cumplir pedidos baratos, pero conviene recargarle pronto.", displayName, vbucks, vbucksLowThreshold),
				colorWarnSoft,
			)
			slog.Info("Discord alert: bot con pocos V-Bucks", "bot", displayName, "vbucks", vbucks)
		}
		return
	}

	// Volvió a estar cómodo — permitir que el aviso de "pocos" se repita si
	// vuelve a bajar más adelante.
	clearAlert(lowKey)
}

// AlertNoGiftSlots avisa cuando una cuenta agota sus regalos del día. No es
// grave (se resetea solo al día siguiente) pero ayuda a entender por qué la
// capacidad bajó de golpe si hay varios pedidos pendientes.
func AlertNoGiftSlots(botID uuid.UUID, displayName string) {
	key := "no_slots_" + botID.String()
	if !shouldAlert(key) {
		return
	}
	sendAdminAlert(
		"🟡 Bot sin regalos disponibles: "+displayName,
		fmt.Sprintf("**%s** agotó sus regalos del día. Se resetea automáticamente mañana — no requiere que hagas nada, es solo un aviso informativo.", displayName),
		colorWarnSoft,
	)
}

// ClearNoGiftSlotsAlert se llama cuando se le restablecen los slots a una
// cuenta (reset diario), para que el aviso pueda repetirse si se agotan otra vez.
func ClearNoGiftSlotsAlert(botID uuid.UUID) {
	clearAlert("no_slots_" + botID.String())
}

// ClearAllNoGiftSlotsAlerts se llama después del reset diario masivo de
// gifts (ResetDailyGifts actualiza todas las cuentas en una sola consulta,
// sin devolver IDs individuales) — así el aviso de "sin regalos" puede
// volver a dispararse si alguna cuenta se queda sin slots otra vez más
// adelante, en vez de quedar silenciado para siempre tras la primera vez.
func ClearAllNoGiftSlotsAlerts() {
	alertStateMu.Lock()
	defer alertStateMu.Unlock()
	for key := range alertState {
		if strings.HasPrefix(key, "no_slots_") {
			delete(alertState, key)
		}
	}
}

// AlertNoActiveBots es la más urgente de todas: el worker de pedidos no
// encontró NINGÚN bot disponible para procesar la cola. Los pedidos quedan
// pendientes hasta que se resuelva. Tiene cooldown propio en vez de
// dispararse una vez y quedar callada, porque mientras el problema persista
// vale la pena que te recuerde cada cierto tiempo — pero sin saturar el DM
// con un mensaje por cada pedido en cola.
const noActiveBotsCooldown = 20 * time.Minute

func AlertNoActiveBots(pendingOrders int) {
	alertStateMu.Lock()
	elapsed := time.Since(lastNoActiveBotsAlert)
	if elapsed < noActiveBotsCooldown {
		alertStateMu.Unlock()
		return
	}
	lastNoActiveBotsAlert = time.Now()
	alertStateMu.Unlock()

	sendAdminAlert(
		"🔴 Sin bots disponibles para enviar regalos",
		fmt.Sprintf("No hay ninguna cuenta bot activa con capacidad para procesar pedidos ahora mismo. Hay **%d pedido(s)** esperando.\n\nRevisa el panel admin (Cuentas Bot): probablemente todas están desactivadas, sin V-Bucks, o sin regalos disponibles hoy.", pendingOrders),
		colorAlert,
	)
	slog.Info("Discord alert: sin bots activos", "pedidos_pendientes", pendingOrders)
}
