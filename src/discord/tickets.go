package discord

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/bwmarrin/discordgo"
)

// ==================== TICKET SYSTEM ====================
// Creates private channels per activation so the user can see
// live updates and submit PINs/codes without DMs.

var (
	ticketChannels   = map[string]string{} // taskID → channelID
	ticketChannelsMu sync.RWMutex
	ticketCategoryID string // set via env or auto-created
)

func SetTicketCategoryID(id string) {
	ticketCategoryID = id
}

func RegisterTicket(taskID, channelID string) {
	ticketChannelsMu.Lock()
	ticketChannels[taskID] = channelID
	ticketChannelsMu.Unlock()
}

func GetTicketChannelID(taskID string) string {
	ticketChannelsMu.RLock()
	defer ticketChannelsMu.RUnlock()
	return ticketChannels[taskID]
}

func RemoveTicket(taskID string) {
	ticketChannelsMu.Lock()
	delete(ticketChannels, taskID)
	ticketChannelsMu.Unlock()
}

// CreateTicketChannel creates a private text channel for a user's activation.
// Only the user, the bot, and admin roles can see it.
func CreateTicketChannel(s *discordgo.Session, guildID, userID, taskID, productName string) (string, error) {
	if s == nil || guildID == "" {
		return "", fmt.Errorf("bot session or guild not available")
	}

	// Find or create category
	categoryID := ticketCategoryID
	if categoryID == "" {
		// Try to find existing "Pedidos" category
		channels, _ := s.GuildChannels(guildID)
		for _, ch := range channels {
			if ch.Type == discordgo.ChannelTypeGuildCategory && ch.Name == "Pedidos" {
				categoryID = ch.ID
				break
			}
		}
		// Create if not found
		if categoryID == "" {
			cat, err := s.GuildChannelCreate(guildID, "Pedidos", discordgo.ChannelTypeGuildCategory)
			if err != nil {
				return "", fmt.Errorf("creating category: %w", err)
			}
			categoryID = cat.ID
		}
		ticketCategoryID = categoryID
	}

	// Channel name
	channelName := fmt.Sprintf("pedido-%s", taskID[:8])

	// Permissions: deny everyone, allow user + bot
	perms := []*discordgo.PermissionOverwrite{
		// Deny @everyone
		{ID: guildID, Type: discordgo.PermissionOverwriteTypeRole,
			Deny: discordgo.PermissionViewChannel},
		// Allow the user
		{ID: userID, Type: discordgo.PermissionOverwriteTypeMember,
			Allow: discordgo.PermissionViewChannel | discordgo.PermissionSendMessages | discordgo.PermissionReadMessageHistory},
	}

	ch, err := s.GuildChannelCreateComplex(guildID, discordgo.GuildChannelCreateData{
		Name:                 channelName,
		Type:                 discordgo.ChannelTypeGuildText,
		ParentID:             categoryID,
		Topic:                fmt.Sprintf("Pedido %s | %s", taskID[:8], productName),
		PermissionOverwrites: perms,
	})
	if err != nil {
		return "", fmt.Errorf("creating channel: %w", err)
	}

	RegisterTicket(taskID, ch.ID)
	slog.Info("Ticket channel created", "channel", channelName, "task", taskID[:8])
	return ch.ID, nil
}

// CloseTicketChannel deletes a ticket channel
func CloseTicketChannel(s *discordgo.Session, taskID string) {
	channelID := GetTicketChannelID(taskID)
	if channelID == "" || s == nil { return }

	if _, err := s.ChannelDelete(channelID); err != nil {
		slog.Warn("Could not delete ticket channel", "channel", channelID, "error", err)
	}
	RemoveTicket(taskID)
}

// SendTicketMessage sends a message to a ticket channel
func SendTicketMessage(s *discordgo.Session, taskID, content string) {
	channelID := GetTicketChannelID(taskID)
	if channelID == "" || s == nil { return }
	s.ChannelMessageSend(channelID, content)
}

// SendTicketEmbed sends an embed to a ticket channel
func SendTicketEmbed(s *discordgo.Session, taskID string, embed *discordgo.MessageEmbed) {
	channelID := GetTicketChannelID(taskID)
	if channelID == "" || s == nil { return }
	s.ChannelMessageSendEmbed(channelID, embed)
}
