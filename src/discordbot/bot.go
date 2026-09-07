// Package discordbot integra el bot de Discord de KidStorePeru: notificaciones
// de bienvenida, recargas de KC, compras exitosas y 48h de amistad cumplidas
// en canales del servidor; comandos para clientes (/perfil, /bots,
// /misordenes, /ayuda, /slot) y comandos de administrador (/kc add|remove).
// No tiene relación con las cuentas bot de Epic Games (paquete fortnite) —
// son bots completamente distintos.
package discordbot

import (
	"KidStoreStore/src/types"
	"database/sql"
	"log/slog"

	"github.com/bwmarrin/discordgo"
)

var (
	session *discordgo.Session
	cfg     types.EnvConfig
	database *sql.DB
)

// Enabled indica si el bot esta configurado (token presente). Si no, todas
// las funciones Notify* y el arranque no hacen nada — el resto de la web
// sigue funcionando normal sin el bot.
func Enabled() bool {
	return cfg.DiscordBotToken != ""
}

// Start programa la conexión a Discord y el registro de comandos. Se llama
// una vez desde main.go al arrancar el servidor. La conexión real ocurre en
// segundo plano (goroutine) para que un Discord lento, caído o con rate
// limit nunca bloquee el arranque del servidor HTTP — el resto de la web
// sigue funcionando normal mientras el bot se conecta (o si nunca lo logra).
func Start(envCfg types.EnvConfig, db *sql.DB) {
	cfg = envCfg
	database = db

	if !Enabled() {
		slog.Info("Discord bot: DISCORD_BOT_TOKEN no configurado, bot deshabilitado")
		return
	}

	go connect()
}

func connect() {
	s, err := discordgo.New("Bot " + cfg.DiscordBotToken)
	if err != nil {
		slog.Error("Discord bot: error creando sesión", "error", err)
		return
	}
	s.Identify.Intents = discordgo.IntentsGuilds
	s.AddHandler(onInteraction)

	if err := s.Open(); err != nil {
		slog.Error("Discord bot: error conectando a Discord", "error", err)
		return
	}
	session = s

	registerSlashCommands()
	slog.Info("Discord bot: conectado y listo")
}

// Stop cierra la conexión con Discord — se llama en el shutdown ordenado del servidor.
func Stop() {
	if session != nil {
		session.Close()
	}
}

func registerSlashCommands() {
	if session == nil || cfg.DiscordGuildID == "" {
		return
	}
	cmd := &discordgo.ApplicationCommand{
		Name:        "slot",
		Description: "Apuesta KidCoins en la tragamonedas",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionInteger,
				Name:        "amount",
				Description: "Cantidad de KC a apostar (10-10000)",
				Required:    true,
				MinValue:    floatPtr(float64(minBet)),
				MaxValue:    float64(maxBet),
			},
		},
	}
	if _, err := session.ApplicationCommandCreate(session.State.User.ID, cfg.DiscordGuildID, cmd); err != nil {
		slog.Error("Discord bot: error registrando /slot", "error", err)
	}

	registerKCCommand()
	registerCustomerCommands()
}

func floatPtr(f float64) *float64 { return &f }

// interactionUser devuelve quién disparó la interacción, ya sea en un
// servidor (i.Member) o en un DM (i.User) — evita repetir este chequeo en
// cada handler.
func interactionUser(i *discordgo.InteractionCreate) *discordgo.User {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User
	}
	return i.User
}

func onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	data := i.ApplicationCommandData()
	switch data.Name {
	case "slot":
		handleSlotCommand(s, i, data)
	case "kc":
		handleKCCommand(s, i, data)
	case "perfil":
		handlePerfilCommand(s, i, data)
	case "bots":
		handleBotsCommand(s, i)
	case "misordenes":
		handleMisOrdenesCommand(s, i)
	case "ayuda":
		handleAyudaCommand(s, i)
	}
}
