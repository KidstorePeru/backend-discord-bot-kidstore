package discord

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/types"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/google/uuid"
)

// ── Constantes ──
const (
	kcCoin   = "<a:kc_coin:1485952905302507670>"
	kcKid    = "<a:kc_kid:1486005001783607399>"
	storeURL = "https://www.kidstoreperu.net"

	ratesAPIKey   = "b3b1e1bf6a9c0a14fd80e3fd"
	ratesAPIURL   = "https://v6.exchangerate-api.com/v6/" + ratesAPIKey + "/latest/PEN"
	defaultPrefix = "?"
)

// ── Prefijo dinámico ──
var (
	currentPrefix   = defaultPrefix
	currentPrefixMu sync.RWMutex
)

func getPrefix() string {
	currentPrefixMu.RLock()
	defer currentPrefixMu.RUnlock()
	return currentPrefix
}

func setPrefix(database *sql.DB, p string) {
	currentPrefixMu.Lock()
	currentPrefix = p
	currentPrefixMu.Unlock()
	db.SetBotPrefix(database, p)
}

// ── Paquetes KC ──
type kcPackage struct {
	Name     string
	KC       int
	PricePEN float64
	Emoji    string
}

var kcPackages = []kcPackage{
	{Name: "Starter", KC: 800,   PricePEN: 12.80,  Emoji: "⚡"},
	{Name: "Gamer",   KC: 2400,  PricePEN: 38.40,  Emoji: "🎮"},
	{Name: "Pro",     KC: 4800,  PricePEN: 76.80,  Emoji: "🔥"},
	{Name: "Legend",  KC: 12500, PricePEN: 200.00, Emoji: "👑"},
}

// ── Caché de tasas ──
type ratesCache struct {
	mu        sync.RWMutex
	USD       float64
	EUR       float64
	fetchedAt time.Time
}

var rates = &ratesCache{USD: 0.27, EUR: 0.25}

func (r *ratesCache) get() (float64, float64) {
	r.mu.RLock(); defer r.mu.RUnlock()
	return r.USD, r.EUR
}

func (r *ratesCache) refresh() {
	r.mu.Lock(); defer r.mu.Unlock()
	if time.Since(r.fetchedAt) < 24*time.Hour { return }
	resp, err := http.Get(ratesAPIURL)
	if err != nil { return }
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var data struct {
		Result          string             `json:"result"`
		ConversionRates map[string]float64 `json:"conversion_rates"`
	}
	if err := json.Unmarshal(body, &data); err != nil || data.Result != "success" { return }
	r.USD = data.ConversionRates["USD"]
	r.EUR = data.ConversionRates["EUR"]
	r.fetchedAt = time.Now()
}

// ── Sesión global ──
var BotSession *discordgo.Session

// botHandlersRegistered evita registrar handlers duplicados si StartBot se llama más de una vez
var botHandlersRegistered bool

// ── Compras pendientes ──
type pendingPurchase struct {
	CustomerID  string
	OfferID     string
	ItemName    string
	PriceKC     int
	PriceVBucks int
	Lang        string
	ExpiresAt   time.Time
}

var (
	pendingMu sync.Mutex
	pending   = map[string]*pendingPurchase{}
)

// ── Sesiones de tienda ──
type shopSession struct {
	Items     []shopItem
	Page      int
	Query     string
	Lang      string
	ExpiresAt time.Time
}

type shopItem struct {
	OfferID string
	Name    string
	VBucks  int
	KC      int
	Section string
}

var (
	shopSessionMu sync.Mutex
	shopSessions  = map[string]*shopSession{}
)

// ── Idioma por usuario ──
var (
	langMu    sync.RWMutex
	userLangs = map[string]string{}
)

func getUserLang(database *sql.DB, discordID string) string {
	langMu.RLock()
	if l, ok := userLangs[discordID]; ok { langMu.RUnlock(); return l }
	langMu.RUnlock()
	lang, err := db.GetDiscordLang(database, discordID)
	if err != nil || lang == "" { lang = "es" }
	langMu.Lock(); userLangs[discordID] = lang; langMu.Unlock()
	return lang
}

func setUserLang(database *sql.DB, discordID, lang string) {
	langMu.Lock(); userLangs[discordID] = lang; langMu.Unlock()
	db.SetDiscordLang(database, discordID, lang)
}

// ── StartBot ──
func StartBot(database *sql.DB, botToken string) (*discordgo.Session, error) {
	dg, err := discordgo.New("Bot " + botToken)
	if err != nil { return nil, fmt.Errorf("error creando sesión Discord: %w", err) }
	dg.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentMessageContent

	if p, err := db.GetBotPrefix(database); err == nil && p != "" {
		currentPrefixMu.Lock(); currentPrefix = p; currentPrefixMu.Unlock()
	}

	// Solo registrar handlers una vez para evitar mensajes/embeds duplicados
	if !botHandlersRegistered {
		dg.AddHandler(messageHandler(database))
		dg.AddHandler(interactionHandler(database))
		botHandlersRegistered = true
	}

	if err := dg.Open(); err != nil { return nil, fmt.Errorf("error conectando al bot: %w", err) }

	BotSession = dg
	go rates.refresh()
	fmt.Printf("🤖 Bot de Discord iniciado — prefijo: %s\n", getPrefix())
	return dg, nil
}

// ── SendOrderNotification ──
func SendOrderNotification(discordID, status, itemName string, priceKC int, lang string) {
	if BotSession == nil || discordID == "" { return }
	es := lang == "es"
	var title, desc string
	var color int
	switch status {
	case "sent":
		color = 0x22c55e
		if es {
			title = fmt.Sprintf("✅ ¡Item enviado! **%s**", itemName)
			desc  = fmt.Sprintf("Tu compra de **%s** fue enviada exitosamente a tu cuenta de Fortnite. ¡Disfrútalo! 🎮", itemName)
		} else {
			title = fmt.Sprintf("✅ Item sent! **%s**", itemName)
			desc  = fmt.Sprintf("Your purchase of **%s** was successfully sent to your Fortnite account. Enjoy! 🎮", itemName)
		}
	case "refunded":
		color = 0xf59e0b
		if es {
			title = fmt.Sprintf("↩️ Reembolso: **%s**", itemName)
			desc  = fmt.Sprintf("Tu pedido de **%s** fue reembolsado. Se acreditaron **%s %s KC** a tu cuenta.", itemName, kcCoin, fmtNum(priceKC))
		} else {
			title = fmt.Sprintf("↩️ Refund: **%s**", itemName)
			desc  = fmt.Sprintf("Your order for **%s** was refunded. **%s %s KC** have been credited to your account.", itemName, kcCoin, fmtNum(priceKC))
		}
	case "failed":
		color = 0xef4444
		if es {
			title = fmt.Sprintf("❌ Pedido fallido: **%s**", itemName)
			desc  = fmt.Sprintf("Tu pedido de **%s** no pudo procesarse. Tus KC fueron reembolsados automáticamente.", itemName)
		} else {
			title = fmt.Sprintf("❌ Order failed: **%s**", itemName)
			desc  = fmt.Sprintf("Your order for **%s** could not be processed. Your KC were automatically refunded.", itemName)
		}
	default:
		return
	}
	ch, err := BotSession.UserChannelCreate(discordID)
	if err != nil { return }
	BotSession.ChannelMessageSendEmbed(ch.ID, &discordgo.MessageEmbed{
		Title: title, Description: desc, Color: color,
		Footer: &discordgo.MessageEmbedFooter{Text: storeURL + "/dashboard"},
	})
}

// ── Handler de mensajes ──
func messageHandler(database *sql.DB) func(*discordgo.Session, *discordgo.MessageCreate) {
	return func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if m.Author.ID == s.State.User.ID { return }
		content := strings.TrimSpace(m.Content)
		pfx := getPrefix()
		if !strings.HasPrefix(content, pfx) { return }

		parts := strings.Fields(strings.TrimPrefix(content, pfx))
		if len(parts) == 0 { return }
		cmd  := strings.ToLower(parts[0])
		args := parts[1:]
		lang := getUserLang(database, m.Author.ID)
		es   := lang == "es"

		customer, err := db.GetCustomerByDiscordID(database, m.Author.ID)
		linked := err == nil

		switch cmd {

		case "ayuda", "help", "comandos":
			sendHelp(s, m.ChannelID, m.Author.ID, lang)

		case "lang", "idioma", "language":
			if len(args) == 0 { s.ChannelMessageSend(m.ChannelID, "🌐 Uso: **`"+pfx+"lang es`** o **`"+pfx+"lang en`**"); return }
			newLang := strings.ToLower(args[0])
			if newLang != "es" && newLang != "en" { s.ChannelMessageSend(m.ChannelID, "❌ Idioma no válido. Usa `"+pfx+"lang es` o `"+pfx+"lang en`"); return }
			setUserLang(database, m.Author.ID, newLang)
			if newLang == "es" { s.ChannelMessageSend(m.ChannelID, "✅ **Idioma cambiado a Español** 🇵🇪") } else { s.ChannelMessageSend(m.ChannelID, "✅ **Language changed to English** 🇺🇸") }

		case "setprefix":
			if !isAdmin(m.Author.ID, database) {
				if es { s.ChannelMessageSend(m.ChannelID, "❌ Solo el administrador puede cambiar el prefijo.") } else { s.ChannelMessageSend(m.ChannelID, "❌ Only the administrator can change the prefix.") }
				return
			}
			if len(args) == 0 { s.ChannelMessageSend(m.ChannelID, "❌ Uso: **`"+pfx+"setprefix [nuevo_prefijo]`**\nEjemplo: `"+pfx+"setprefix !`"); return }
			newPfx := args[0]
			if len(newPfx) > 3 { s.ChannelMessageSend(m.ChannelID, "❌ El prefijo no puede tener más de 3 caracteres."); return }
			setPrefix(database, newPfx)
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("✅ **Prefijo cambiado a `%s`**\nAhora usa `%sayuda` para ver los comandos.", newPfx, newPfx))

		case "kc", "addkc", "quitarkc", "gestionkc":
			if !isAdmin(m.Author.ID, database) {
				if es { s.ChannelMessageSend(m.ChannelID, "❌ Solo el administrador puede gestionar KC.") } else { s.ChannelMessageSend(m.ChannelID, "❌ Only the administrator can manage KC.") }
				return
			}
			if len(args) < 2 {
				s.ChannelMessageSend(m.ChannelID, "❌ Uso: **`"+pfx+"kc @usuario +500`** para añadir o **`"+pfx+"kc @usuario -200`** para quitar\nEjemplo: `"+pfx+"kc @Kiddarkness +1000`")
				return
			}
			manageKC(s, m.ChannelID, database, args, lang)

		case "horario", "schedule", "horas":
			sendSchedule(s, m.ChannelID, m.Author.ID, database, lang)

		case "saldo", "balance":
			if !linked { sendNotLinked(s, m.ChannelID, m.Author.Username, lang); return }
			sendBalance(s, m.ChannelID, m.Author.ID, customer, lang)

		case "perfil", "profile", "cuenta":
			if !linked { sendNotLinked(s, m.ChannelID, m.Author.Username, lang); return }
			sendProfile(s, m.ChannelID, m.Author.ID, customer, lang)

		case "pedidos", "orders", "historial":
			if !linked { sendNotLinked(s, m.ChannelID, m.Author.Username, lang); return }
			sendOrders(s, m.ChannelID, m.Author.ID, database, customer, lang)

		case "tienda", "shop", "store":
			sendShop(s, m.ChannelID, m.Author.ID, lang)

		case "recargar", "recharge", "paquetes":
			sendPackages(s, m.ChannelID, m.Author.ID, lang)

		case "comprar", "buy":
			if !linked { sendNotLinked(s, m.ChannelID, m.Author.Username, lang); return }
			query := ""
			if len(args) > 0 { query = strings.Join(args, " ") }
			sendShopBrowser(s, m.ChannelID, m.Author.ID, database, customer, query, lang)

		case "si", "yes", "confirmar", "confirm":
			if !linked { sendNotLinked(s, m.ChannelID, m.Author.Username, lang); return }
			confirmPurchase(s, m.ChannelID, m.Author.ID, database, customer, lang)

		case "no", "cancelar", "cancel":
			pendingMu.Lock(); delete(pending, m.Author.ID); pendingMu.Unlock()
			if es { s.ChannelMessageSend(m.ChannelID, "❌ **Compra cancelada.**") } else { s.ChannelMessageSend(m.ChannelID, "❌ **Purchase cancelled.**") }

		case "agregar", "bots", "amigos", "add":
			sendAddBots(s, m.ChannelID, m.Author.ID, lang)

		case "vincular", "link":
			sendLink(s, m.ChannelID, m.Author.ID, m.Author.Username, lang)

		default:
			if es {
				s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("❓ Comando **`%s%s`** no reconocido. Usa **`%sayuda`** para ver los comandos.", pfx, cmd, pfx))
			} else {
				s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("❓ Command **`%s%s`** not recognized. Use **`%shelp`** for all commands.", pfx, cmd, pfx))
			}
		}
	}
}

// ── isAdmin ──
func isAdmin(discordID string, database *sql.DB) bool {
	admins, err := db.GetBotAdmins(database)
	if err != nil { return false }
	for _, a := range admins { if a == discordID { return true } }
	return false
}

// ── manageKC ──
func manageKC(s *discordgo.Session, channelID string, database *sql.DB, args []string, lang string) {
	es := lang == "es"
	targetDiscordID := args[0]
	targetDiscordID = strings.TrimPrefix(targetDiscordID, "<@!")
	targetDiscordID = strings.TrimPrefix(targetDiscordID, "<@")
	targetDiscordID = strings.TrimSuffix(targetDiscordID, ">")

	amountStr := args[1]
	negative := strings.HasPrefix(amountStr, "-")
	amountStr = strings.TrimPrefix(strings.TrimPrefix(amountStr, "+"), "-")
	var amount int
	fmt.Sscanf(amountStr, "%d", &amount)
	if amount <= 0 { s.ChannelMessageSend(channelID, "❌ La cantidad debe ser un número positivo. Ejemplo: `+500` o `-200`"); return }
	if negative { amount = -amount }

	customer, err := db.GetCustomerByDiscordID(database, targetDiscordID)
	if err != nil {
		if es { s.ChannelMessageSend(channelID, "❌ No se encontró ningún usuario vinculado con ese Discord.") } else { s.ChannelMessageSend(channelID, "❌ No user found linked to that Discord account.") }
		return
	}

	var newBalance int
	note := "Ajuste manual via Discord bot por admin"
	if amount > 0 {
		if err := db.RechargeKC(database, customer.ID, amount, nil, &note, "discord-admin"); err != nil { s.ChannelMessageSend(channelID, fmt.Sprintf("❌ Error: %s", err.Error())); return }
		newBalance = customer.KCBalance + amount
	} else {
		deduct := -amount
		if customer.KCBalance < deduct {
			if es { s.ChannelMessageSend(channelID, fmt.Sprintf("❌ El usuario solo tiene **%s KC** y quieres quitar **%s KC**.", fmtNum(customer.KCBalance), fmtNum(deduct))) } else { s.ChannelMessageSend(channelID, fmt.Sprintf("❌ User only has **%s KC** and you want to remove **%s KC**.", fmtNum(customer.KCBalance), fmtNum(deduct))) }
			return
		}
		if err := db.DeductKCAdmin(database, customer.ID, deduct, note); err != nil { s.ChannelMessageSend(channelID, fmt.Sprintf("❌ Error: %s", err.Error())); return }
		newBalance = customer.KCBalance - deduct
	}

	db.AddAuditLog(database, &customer.ID, "KC_ADMIN_ADJUST", fmt.Sprintf("ajuste %+d KC via Discord bot", amount), "discord-admin")
	color := 0x22c55e; if amount < 0 { color = 0xef4444 }
	action := "➕ añadidos"; if amount < 0 { action = "➖ quitados" }
	if !es { action = "➕ added"; if amount < 0 { action = "➖ removed" } }
	fUser, fAmt, fBal := "👤 Usuario", "💰 Cantidad", "📊 Nuevo saldo"
	if !es { fUser = "👤 User"; fAmt = "💰 Amount"; fBal = "📊 New balance" }
	embed := &discordgo.MessageEmbed{
		Title: fmt.Sprintf("✅ KC %s", action), Color: color,
		Fields: []*discordgo.MessageEmbedField{
			{Name: fUser, Value: fmt.Sprintf("**%s**", customer.EpicUsername), Inline: true},
			{Name: fAmt,  Value: fmt.Sprintf("**%+d KC**", amount), Inline: true},
			{Name: fBal,  Value: fmt.Sprintf("**%s %s KC**", kcCoin, fmtNum(newBalance)), Inline: true},
		},
	}
	s.ChannelMessageSendEmbed(channelID, embed)
}

// ── ?horario ──
func sendSchedule(s *discordgo.Session, channelID, userID string, database *sql.DB, lang string) {
	es := lang == "es"
	schedule, err := db.GetBotSchedule(database)
	if err != nil {
		if es { s.ChannelMessageSend(channelID, "❌ Error obteniendo el horario.") } else { s.ChannelMessageSend(channelID, "❌ Error fetching schedule.") }
		return
	}

	startLima := schedule.StartHour
	endLima   := schedule.EndHour

	statusIcon := "🟢"; statusText := "Activos"
	if !es { statusText = "Active" }

	inSchedule, _ := db.IsWithinSchedule(database)
	if !inSchedule {
		statusIcon = "🔴"; statusText = "Fuera de horario"
		if !es { statusText = "Outside working hours" }
	}
	if !schedule.Enabled {
		statusIcon = "⚫"; statusText = "Deshabilitados"
		if !es { statusText = "Disabled" }
	}

	var title, desc, f1n, f1v, f2n, f2v, f3n, f3v, foot string
	if es {
		title = "🕐 Horario de Bots — KidStorePeru"
		desc  = fmt.Sprintf("Estado actual: %s **%s**", statusIcon, statusText)
		f1n   = "🇵🇪 Horario de atención"
		f1v   = fmt.Sprintf("**%02d:00 — %02d:00** hora Lima", startLima, endLima)
		f2n   = "📋 Zona horaria"
		f2v   = schedule.Timezone
		f3n   = "📅 Días de operación"
		f3v   = "Lunes a Domingo"
		foot  = "Horario configurable desde el panel de administración"
	} else {
		title = "🕐 Bot Schedule — KidStorePeru"
		desc  = fmt.Sprintf("Current status: %s **%s**", statusIcon, statusText)
		f1n   = "🇵🇪 Working hours"
		f1v   = fmt.Sprintf("**%02d:00 — %02d:00** Lima time", startLima, endLima)
		f2n   = "📋 Timezone"
		f2v   = schedule.Timezone
		f3n   = "📅 Operating days"
		f3v   = "Monday to Sunday"
		foot  = "Schedule configurable from the administration panel"
	}

	embed := &discordgo.MessageEmbed{
		Title: title, Description: desc,
		Color: func() int { if inSchedule && schedule.Enabled { return 0x22c55e }; return 0xef4444 }(),
		Fields: []*discordgo.MessageEmbedField{
			{Name: f1n, Value: f1v, Inline: true},
			{Name: f2n, Value: f2v, Inline: true},
			{Name: f3n, Value: f3v},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: foot},
	}
	s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{embed},
		Components: []discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

// ── Handler de interacciones ──
func interactionHandler(database *sql.DB) func(*discordgo.Session, *discordgo.InteractionCreate) {
	return func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if i.Type != discordgo.InteractionMessageComponent { return }
		data := i.MessageComponentData()

		userID := ""
		if i.Member != nil && i.Member.User != nil { userID = i.Member.User.ID } else if i.User != nil { userID = i.User.ID }
		if userID == "" { return }

		lang    := getUserLang(database, userID)
		customer, err := db.GetCustomerByDiscordID(database, userID)
		linked  := err == nil

		if !strings.Contains(data.CustomID, userID) {
			respondEphemeral(s, i, func() string {
				if lang == "es" { return "❌ Este menú no es tuyo. Ejecuta tu propio comando." }
				return "❌ This menu is not yours. Run your own command."
			}())
			return
		}

		if data.CustomID == "nav_menu_"+userID {
			if len(data.Values) == 0 { return }
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate})
			switch data.Values[0] {
			case "saldo":    if !linked { return }; updateToBalance(s, i, userID, customer, lang)
			case "recargar": updateToPackages(s, i, userID, lang)
			case "perfil":   if !linked { return }; updateToProfile(s, i, userID, customer, lang)
			case "pedidos":  if !linked { return }; updateToOrders(s, i, userID, database, customer, lang)
			case "tienda":   updateToShop(s, i, userID, lang)
			case "comprar":  if !linked { return }; updateToShopBrowser(s, i, userID, database, customer, "", lang)
			case "agregar":  updateToAddBots(s, i, userID, lang)
			case "vincular": updateToLink(s, i, userID, lang)
			case "horario":  updateToSchedule(s, i, userID, database, lang)
			}
			return
		}

		if strings.HasPrefix(data.CustomID, "shop_page_") {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate})
			parts := strings.Split(data.CustomID, "_")
			if len(parts) < 4 { return }
			direction := parts[2]
			shopSessionMu.Lock(); sess, ok := shopSessions[userID]; shopSessionMu.Unlock()
			if !ok || !linked { return }
			if direction == "next" { sess.Page++ } else if direction == "prev" && sess.Page > 0 { sess.Page-- }
			updateShopBrowserEmbed(s, i, userID, database, customer, sess, lang)
			return
		}

		if data.CustomID == "shop_select_"+userID {
			if len(data.Values) == 0 { return }
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate})
			offerID := data.Values[0]
			shopSessionMu.Lock(); sess, ok := shopSessions[userID]; shopSessionMu.Unlock()
			if !ok || !linked { return }
			var found *shopItem
			for idx := range sess.Items { if sess.Items[idx].OfferID == offerID { found = &sess.Items[idx]; break } }
			if found == nil { return }
			updateToBuyConfirm(s, i, userID, customer, found, lang)
			return
		}

		if data.CustomID == "buy_confirm_"+userID {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate})
			confirmPurchaseInteraction(s, i, userID, database, customer, lang)
			return
		}

		if data.CustomID == "buy_cancel_"+userID {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate})
			pendingMu.Lock(); delete(pending, userID); pendingMu.Unlock()
			msg := "❌ **Compra cancelada.**"
			if lang != "es" { msg = "❌ **Purchase cancelled.**" }
			empty := []discordgo.MessageComponent{}
			s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
				Content: &msg, Embeds: &[]*discordgo.MessageEmbed{}, Components: &empty,
			})
			return
		}
	}
}

// ── Menú de navegación ──
func navMenu(userID, lang string) discordgo.MessageComponent {
	es := lang == "es"
	pfx := getPrefix()
	ph := fmt.Sprintf("📋 Menú de comandos (%s)", pfx)
	if !es { ph = fmt.Sprintf("📋 Command menu (%s)", pfx) }
	return discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{
				CustomID:    "nav_menu_" + userID,
				Placeholder: ph,
				Options: []discordgo.SelectMenuOption{
					{Label: func() string { if es { return "💰 Saldo" }; return "💰 Balance" }(),         Value: "saldo",    Emoji: &discordgo.ComponentEmoji{Name: "kc_coin", ID: "1485952905302507670", Animated: true}, Description: func() string { if es { return "Ver tu balance KC" }; return "Check KC balance" }()},
					{Label: func() string { if es { return "💳 Recargar" }; return "💳 Recharge" }(),     Value: "recargar", Description: func() string { if es { return "Ver paquetes KC" }; return "View KC packages" }()},
					{Label: func() string { if es { return "👤 Perfil" }; return "👤 Profile" }(),        Value: "perfil",   Description: func() string { if es { return "Ver tu perfil" }; return "View your profile" }()},
					{Label: func() string { if es { return "📦 Pedidos" }; return "📦 Orders" }(),        Value: "pedidos",  Description: func() string { if es { return "Últimos pedidos" }; return "Recent orders" }()},
					{Label: func() string { if es { return "🛒 Tienda" }; return "🛒 Store" }(),          Value: "tienda",   Description: func() string { if es { return "Link a la tienda" }; return "Store link" }()},
					{Label: func() string { if es { return "🎮 Comprar" }; return "🎮 Buy" }(),           Value: "comprar",  Description: func() string { if es { return "Buscar y comprar items" }; return "Search and buy items" }()},
					{Label: func() string { if es { return "🕐 Horario" }; return "🕐 Schedule" }(),      Value: "horario",  Description: func() string { if es { return "Horario de los bots" }; return "Bot working hours" }()},
					{Label: func() string { if es { return "🤖 Agregar bots" }; return "🤖 Add bots" }(), Value: "agregar",  Description: func() string { if es { return "Cuentas a agregar en Epic" }; return "Accounts to add in Epic" }()},
					{Label: func() string { if es { return "🔗 Vincular" }; return "🔗 Link" }(),         Value: "vincular", Description: func() string { if es { return "Vincular cuenta Discord" }; return "Link Discord account" }()},
				},
			},
		},
	}
}

// ── ?ayuda ──
func sendHelp(s *discordgo.Session, channelID, userID, lang string) {
	es := lang == "es"
	pfx := getPrefix()
	var desc, linksTitle, linksVal string
	if es {
		desc = fmt.Sprintf(
			"**» Menú de ayuda**\n¡Aquí encontrarás todos los comandos disponibles!\nUsa `%shelp <comando>` para ver detalles.\n\n**» Categorías**\n`%ssaldo` :: %s  Ver tu balance de KidCoins\n`%srecargar` :: 💳  Ver paquetes KC con precios\n`%sperfil` :: 👤  Ver tu perfil completo\n`%svincular` :: 🔗  Cómo vincular tu cuenta\n`%slang es/en` :: 🌐  Cambiar idioma\n`%stienda` :: 🛒  Link a la tienda de Fortnite\n`%scomprar [item]` :: 🎮  Buscar y comprar un ítem\n`%spedidos` :: 📦  Ver tus últimos pedidos\n`%sagregar` :: 🤖  Cuentas bot a agregar en Epic\n`%shorario` :: 🕐  Ver horario de los bots",
			pfx, pfx, kcCoin, pfx, pfx, pfx, pfx, pfx, pfx, pfx, pfx, pfx,
		)
		linksTitle = "**» Enlaces útiles**"
		linksVal   = fmt.Sprintf("🌐 [Website](%s) | 📖 [FAQ](%s/faq) | 📜 [Términos y Condiciones](%s/terms)\n🔒 [Política de Privacidad](%s/privacy) | 💸 [Política de Reembolsos](%s/refunds)", storeURL, storeURL, storeURL, storeURL, storeURL)
	} else {
		desc = fmt.Sprintf(
			"**» Help Menu**\nHere you'll find all available commands!\nUse `%shelp <command>` for details.\n\n**» Categories**\n`%sbalance` :: %s  Check your KidCoins balance\n`%srecharge` :: 💳  View KC packages with prices\n`%sprofile` :: 👤  View your full profile\n`%slink` :: 🔗  How to link your account\n`%slang es/en` :: 🌐  Change language\n`%sshop` :: 🛒  Link to the Fortnite store\n`%sbuy [item]` :: 🎮  Search and buy an item\n`%sorders` :: 📦  View your recent orders\n`%sadd` :: 🤖  Bot accounts to add in Epic\n`%sschedule` :: 🕐  View bot working hours",
			pfx, pfx, kcCoin, pfx, pfx, pfx, pfx, pfx, pfx, pfx, pfx, pfx,
		)
		linksTitle = "**» Useful links**"
		linksVal   = fmt.Sprintf("🌐 [Website](%s) | 📖 [FAQ](%s/faq) | 📜 [Terms & Conditions](%s/terms)\n🔒 [Privacy Policy](%s/privacy) | 💸 [Refund Policy](%s/refunds)", storeURL, storeURL, storeURL, storeURL, storeURL)
	}
	embed := &discordgo.MessageEmbed{
		Title:       "🎮 KidStorePeru Bot — " + func() string { if es { return "Centro de Ayuda" }; return "Help Center" }(),
		Description: desc,
		Color:       0x7c3aed,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "\u200b", Value: "\u200b"},
			{Name: linksTitle, Value: linksVal},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: " © Kiddarkness "},
	}
	s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{embed},
		Components: []discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

// ── ?saldo ──
func sendBalance(s *discordgo.Session, channelID, userID string, c types.Customer, lang string) {
	s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{balanceEmbed(c, lang)},
		Components: []discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

func balanceEmbed(c types.Customer, lang string) *discordgo.MessageEmbed {
	es := lang == "es"
	title := fmt.Sprintf("💰 Balance de **%s**", c.EpicUsername)
	kl, ll := "KidCoins", "Nivel"
	if !es { title = fmt.Sprintf("💰 Balance of **%s**", c.EpicUsername); ll = "Level" }
	return &discordgo.MessageEmbed{
		Title: title, Color: 0xf59e0b,
		Fields: []*discordgo.MessageEmbedField{
			{Name: kl, Value: fmt.Sprintf("**%s %s KC**", kcCoin, fmtNum(c.KCBalance)), Inline: true},
			{Name: ll, Value: levelStr(c.KCBalance, lang), Inline: true},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: storeURL + "/recharge"},
	}
}

func updateToBalance(s *discordgo.Session, i *discordgo.InteractionCreate, userID string, c types.Customer, lang string) {
	e := balanceEmbed(c, lang)
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds:     &[]*discordgo.MessageEmbed{e},
		Components: &[]discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

// ── ?perfil ──
func sendProfile(s *discordgo.Session, channelID, userID string, c types.Customer, lang string) {
	s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{profileEmbed(c, lang)},
		Components: []discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

func profileEmbed(c types.Customer, lang string) *discordgo.MessageEmbed {
	es := lang == "es"
	dt := "—"; if c.DiscordUsername != nil && *c.DiscordUsername != "" { dt = *c.DiscordUsername }
	em := "—"; if c.Email != nil { em = *c.Email }
	title := fmt.Sprintf("👤 Perfil de **%s**", c.EpicUsername)
	f1, f2, f3, f4, f5, f6 := "Usuario Epic", "Balance KC", "Nivel", "Discord", "Email", "Miembro desde"
	if !es { title = fmt.Sprintf("👤 Profile of **%s**", c.EpicUsername); f1 = "Epic Username"; f2 = "KC Balance"; f3 = "Level"; f6 = "Member since" }
	return &discordgo.MessageEmbed{
		Title: title, Color: 0x7c3aed,
		Fields: []*discordgo.MessageEmbedField{
			{Name: f1, Value: fmt.Sprintf("**%s**", c.EpicUsername), Inline: true},
			{Name: f2, Value: fmt.Sprintf("**%s %s KC**", kcCoin, fmtNum(c.KCBalance)), Inline: true},
			{Name: f3, Value: levelStr(c.KCBalance, lang), Inline: true},
			{Name: f4, Value: dt, Inline: true},
			{Name: f5, Value: em, Inline: true},
			{Name: f6, Value: fmt.Sprintf("*%s*", c.CreatedAt.Format("02/01/2006")), Inline: true},
		},
	}
}

func updateToProfile(s *discordgo.Session, i *discordgo.InteractionCreate, userID string, c types.Customer, lang string) {
	e := profileEmbed(c, lang)
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds:     &[]*discordgo.MessageEmbed{e},
		Components: &[]discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

// ── ?pedidos ──
func sendOrders(s *discordgo.Session, channelID, userID string, database *sql.DB, c types.Customer, lang string) {
	s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{ordersEmbed(database, c, lang)},
		Components: []discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

func ordersEmbed(database *sql.DB, c types.Customer, lang string) *discordgo.MessageEmbed {
	es := lang == "es"
	orders, err := db.GetOrdersByCustomer(database, c.ID)
	if err != nil || len(orders) == 0 {
		msg := "*No tienes pedidos registrados aún.*"; if !es { msg = "*No orders registered yet.*" }
		return &discordgo.MessageEmbed{Description: "📭 " + msg, Color: 0x3b82f6}
	}
	if len(orders) > 5 { orders = orders[:5] }
	var fields []*discordgo.MessageEmbedField
	for _, o := range orders {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  fmt.Sprintf("%s **%s**", statusEmoji(o.Status), o.ItemName),
			Value: fmt.Sprintf("%s %s KC · *%s* · %s", kcCoin, fmtNum(o.PriceKC), statusStr(o.Status, lang), o.CreatedAt.Format("02/01/06")),
		})
	}
	title := fmt.Sprintf("📦 Últimos pedidos de **%s**", c.EpicUsername)
	if !es { title = fmt.Sprintf("📦 Recent orders of **%s**", c.EpicUsername) }
	return &discordgo.MessageEmbed{
		Title: title, Color: 0x3b82f6, Fields: fields,
		Footer: &discordgo.MessageEmbedFooter{Text: storeURL + "/dashboard"},
	}
}

func updateToOrders(s *discordgo.Session, i *discordgo.InteractionCreate, userID string, database *sql.DB, c types.Customer, lang string) {
	e := ordersEmbed(database, c, lang)
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds:     &[]*discordgo.MessageEmbed{e},
		Components: &[]discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

// ── ?tienda ──
func sendShop(s *discordgo.Session, channelID, userID, lang string) {
	s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{shopEmbed(lang)},
		Components: []discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

func shopEmbed(lang string) *discordgo.MessageEmbed {
	es := lang == "es"
	title := "🛒 Tienda de Fortnite — KidStorePeru"
	desc  := "Más de **200 items** disponibles hoy. ¡Compra con KidCoins!"
	f1v   := fmt.Sprintf("[🔗 Abrir tienda](%s/store)", storeURL)
	f2v   := fmt.Sprintf("[💳 Recargar KC](%s/recharge)", storeURL)
	foot  := "Items se actualizan diariamente a las 00:00 UTC (19:00 hora Lima)"
	if !es {
		title = "🛒 Fortnite Store — KidStorePeru"
		desc  = "More than **200 items** available today. Buy with KidCoins!"
		f1v   = fmt.Sprintf("[🔗 Open store](%s/store)", storeURL)
		f2v   = fmt.Sprintf("[💳 Recharge KC](%s/recharge)", storeURL)
		foot  = "Items update daily at 00:00 UTC (19:00 Lima time)"
	}
	return &discordgo.MessageEmbed{
		Title: title, Description: desc, Color: 0x22c55e,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "🌐 Web", Value: f1v, Inline: true},
			{Name: "⚡ KC",  Value: f2v, Inline: true},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: foot},
	}
}

func updateToShop(s *discordgo.Session, i *discordgo.InteractionCreate, userID, lang string) {
	e := shopEmbed(lang)
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds:     &[]*discordgo.MessageEmbed{e},
		Components: &[]discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

func updateToSchedule(s *discordgo.Session, i *discordgo.InteractionCreate, userID string, database *sql.DB, lang string) {
	es := lang == "es"
	schedule, err := db.GetBotSchedule(database)
	if err != nil { msg := "❌ Error"; s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg}); return }
	startLima := schedule.StartHour
	endLima   := schedule.EndHour
	inSchedule, _ := db.IsWithinSchedule(database)
	statusIcon := "🟢"; statusText := "Activos"; if !es { statusText = "Active" }
	if !inSchedule { statusIcon = "🔴"; statusText = "Fuera de horario"; if !es { statusText = "Outside working hours" } }
	if !schedule.Enabled { statusIcon = "⚫"; statusText = "Deshabilitados"; if !es { statusText = "Disabled" } }
	title := "🕐 Horario de Bots — KidStorePeru"; if !es { title = "🕐 Bot Schedule — KidStorePeru" }
	f1n, f2n, f3n, f3v := "🌍 Horario UTC", "🇵🇪 Horario Lima (UTC-5)", "📅 Días", "Lunes a Domingo"
	if !es { f1n = "🌍 UTC Schedule"; f2n = "🇵🇪 Lima Time (UTC-5)"; f3n = "📅 Days"; f3v = "Monday to Sunday" }
	foot := "Los items se actualizan a las 00:00 UTC (19:00 Lima)"; if !es { foot = "Items update at 00:00 UTC (19:00 Lima time)" }
	embed := &discordgo.MessageEmbed{
		Title: title, Description: fmt.Sprintf("%s **%s**", statusIcon, statusText),
		Color: func() int { if inSchedule && schedule.Enabled { return 0x22c55e }; return 0xef4444 }(),
		Fields: []*discordgo.MessageEmbedField{
			{Name: f1n, Value: fmt.Sprintf("**%02d:00 — %02d:00** UTC", schedule.StartHour, schedule.EndHour), Inline: true},
			{Name: f2n, Value: fmt.Sprintf("**%02d:00 — %02d:00** Lima", startLima, endLima), Inline: true},
			{Name: f3n, Value: f3v},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: foot},
	}
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds:     &[]*discordgo.MessageEmbed{embed},
		Components: &[]discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

// ── ?recargar ──
func sendPackages(s *discordgo.Session, channelID, userID, lang string) {
	s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{packagesEmbed(lang)},
		Components: []discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

func packagesEmbed(lang string) *discordgo.MessageEmbed {
	es := lang == "es"
	rates.refresh(); usd, eur := rates.get()
	var fields []*discordgo.MessageEmbedField
	for _, pkg := range kcPackages {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:  fmt.Sprintf("%s **%s — %s KC**", kcCoin, pkg.Name, fmtNum(pkg.KC)),
			Value: fmt.Sprintf("**S/ %.2f** · $%.2f · €%.2f", pkg.PricePEN, roundCents(pkg.PricePEN*usd), roundCents(pkg.PricePEN*eur)),
		})
	}
	title, foot := "💳 Paquetes de KidCoins", "Precios en USD y EUR calculados con tasa del día"
	if !es { title = "💳 KidCoins Packages"; foot = "USD and EUR prices calculated with today's exchange rate" }
	return &discordgo.MessageEmbed{Title: title, Color: 0xf59e0b, Fields: fields, Footer: &discordgo.MessageEmbedFooter{Text: foot}}
}

func updateToPackages(s *discordgo.Session, i *discordgo.InteractionCreate, userID, lang string) {
	e := packagesEmbed(lang)
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds:     &[]*discordgo.MessageEmbed{e},
		Components: &[]discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

// ── ?comprar ──
const pageSize = 20

func sendShopBrowser(s *discordgo.Session, channelID, userID string, database *sql.DB, c types.Customer, query, lang string) {
	items := fetchShopItems(lang)
	if query != "" { items = filterItems(items, query) }
	sess := &shopSession{Items: items, Page: 0, Query: query, Lang: lang, ExpiresAt: time.Now().Add(5 * time.Minute)}
	shopSessionMu.Lock(); shopSessions[userID] = sess; shopSessionMu.Unlock()
	embed, components := buildShopBrowserEmbed(userID, c, sess, lang)
	s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}, Components: components})
}

func updateToShopBrowser(s *discordgo.Session, i *discordgo.InteractionCreate, userID string, database *sql.DB, c types.Customer, query, lang string) {
	items := fetchShopItems(lang)
	if query != "" { items = filterItems(items, query) }
	sess := &shopSession{Items: items, Page: 0, Query: query, Lang: lang, ExpiresAt: time.Now().Add(5 * time.Minute)}
	shopSessionMu.Lock(); shopSessions[userID] = sess; shopSessionMu.Unlock()
	embed, components := buildShopBrowserEmbed(userID, c, sess, lang)
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Embeds: &[]*discordgo.MessageEmbed{embed}, Components: &components})
}

func updateShopBrowserEmbed(s *discordgo.Session, i *discordgo.InteractionCreate, userID string, database *sql.DB, c types.Customer, sess *shopSession, lang string) {
	embed, components := buildShopBrowserEmbed(userID, c, sess, lang)
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Embeds: &[]*discordgo.MessageEmbed{embed}, Components: &components})
}

func buildShopBrowserEmbed(userID string, c types.Customer, sess *shopSession, lang string) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	es := lang == "es"
	start := sess.Page * pageSize; end := start + pageSize
	if end > len(sess.Items) { end = len(sess.Items) }
	pageItems := sess.Items
	if start < len(sess.Items) { pageItems = sess.Items[start:end] } else { pageItems = []shopItem{} }
	totalPages := int(math.Ceil(float64(len(sess.Items)) / float64(pageSize)))
	if totalPages == 0 { totalPages = 1 }

	var title, desc string
	if es {
		title = fmt.Sprintf("🎮 Tienda de Fortnite — Página %d/%d", sess.Page+1, totalPages)
		if sess.Query != "" { desc = fmt.Sprintf("🔍 *Resultados para* **\"%s\"** — **%d items** encontrados\n*Selecciona un item del menú para comprarlo*", sess.Query, len(sess.Items)) } else { desc = fmt.Sprintf("**%d items** disponibles hoy\n*Selecciona un item del menú para comprarlo*", len(sess.Items)) }
	} else {
		title = fmt.Sprintf("🎮 Fortnite Store — Page %d/%d", sess.Page+1, totalPages)
		if sess.Query != "" { desc = fmt.Sprintf("🔍 *Results for* **\"%s\"** — **%d items** found\n*Select an item from the menu to buy it*", sess.Query, len(sess.Items)) } else { desc = fmt.Sprintf("**%d items** available today\n*Select an item from the menu to buy it*", len(sess.Items)) }
	}

	embed := &discordgo.MessageEmbed{
		Title: title, Description: desc, Color: 0x7c3aed,
		Footer: &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("Balance: %s KC", fmtNum(c.KCBalance))},
	}

	var selectOptions []discordgo.SelectMenuOption
	for _, it := range pageItems {
		suffix := ""; if c.KCBalance < it.KC { if es { suffix = " ❌ KC insuf." } else { suffix = " ❌ Not enough KC" } }
		selectOptions = append(selectOptions, discordgo.SelectMenuOption{
			Label: truncate(it.Name, 80), Value: it.OfferID,
			Description: fmt.Sprintf("%s KC · %d V-Bucks%s", fmtNum(it.KC), it.VBucks, suffix),
			Emoji: &discordgo.ComponentEmoji{Name: "kc_coin", ID: "1485952905302507670", Animated: true},
		})
	}

	ph := "🛒 Selecciona un item para comprar"; if !es { ph = "🛒 Select an item to buy" }
	if len(selectOptions) == 0 {
		if es { ph = "❌ No hay items disponibles" } else { ph = "❌ No items available" }
		selectOptions = []discordgo.SelectMenuOption{{Label: "-", Value: "none"}}
	}

	prevLabel := "◀ Anterior"; nextLabel := "Siguiente ▶"
	if !es { prevLabel = "◀ Previous"; nextLabel = "Next ▶" }

	components := []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{CustomID: "shop_select_" + userID, Placeholder: ph, Options: selectOptions},
		}},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: prevLabel, Style: discordgo.SecondaryButton, CustomID: "shop_page_prev_" + userID, Disabled: sess.Page == 0},
			discordgo.Button{Label: fmt.Sprintf("%d/%d", sess.Page+1, totalPages), Style: discordgo.SecondaryButton, CustomID: "shop_page_info_" + userID, Disabled: true},
			discordgo.Button{Label: nextLabel, Style: discordgo.SecondaryButton, CustomID: "shop_page_next_" + userID, Disabled: sess.Page >= totalPages-1},
		}},
		navMenu(userID, lang),
	}
	return embed, components
}

func updateToBuyConfirm(s *discordgo.Session, i *discordgo.InteractionCreate, userID string, c types.Customer, item *shopItem, lang string) {
	es := lang == "es"
	rates.refresh(); usd, eur := rates.get()
	penPerKC := 12.80 / 800.0
	pricePEN := roundCents(float64(item.KC) * penPerKC)

	pendingMu.Lock()
	pending[userID] = &pendingPurchase{CustomerID: c.ID.String(), OfferID: item.OfferID, ItemName: item.Name, PriceKC: item.KC, PriceVBucks: item.VBucks, Lang: lang, ExpiresAt: time.Now().Add(2 * time.Minute)}
	pendingMu.Unlock()

	title, desc, balField, foot := "", "", "", ""
	if es {
		title    = fmt.Sprintf("🛒 Confirmar compra: **%s**", item.Name)
		desc     = "*¿Deseas comprar este item?*"
		balField = fmt.Sprintf("Saldo restante: **%s %s KC**", kcCoin, fmtNum(c.KCBalance-item.KC))
		foot     = "⚠️ Asegúrate de tener al menos un bot agregado en Epic Games"
	} else {
		title    = fmt.Sprintf("🛒 Confirm purchase: **%s**", item.Name)
		desc     = "*Do you want to buy this item?*"
		balField = fmt.Sprintf("Remaining balance: **%s %s KC**", kcCoin, fmtNum(c.KCBalance-item.KC))
		foot     = "⚠️ Make sure you have at least one bot added in Epic Games"
	}

	embed := &discordgo.MessageEmbed{
		Title: title, Description: desc, Color: 0x7c3aed,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "💰 KC",            Value: fmt.Sprintf("**%s %s KC**", kcCoin, fmtNum(item.KC)), Inline: true},
			{Name: "🎮 V-Bucks",      Value: fmt.Sprintf("**%d**", item.VBucks), Inline: true},
			{Name: "💵 Precio aprox.", Value: fmt.Sprintf("S/ %.2f · $%.2f · €%.2f", pricePEN, roundCents(pricePEN*usd), roundCents(pricePEN*eur))},
			{Name: "📊 Balance",      Value: balField},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: foot},
	}
	cl, cal := "✅ Confirmar", "❌ Cancelar"; if !es { cl = "✅ Confirm"; cal = "❌ Cancel" }
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds: &[]*discordgo.MessageEmbed{embed},
		Components: &[]discordgo.MessageComponent{
			discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.Button{Label: cl,  Style: discordgo.SuccessButton, CustomID: "buy_confirm_" + userID},
				discordgo.Button{Label: cal, Style: discordgo.DangerButton,  CustomID: "buy_cancel_"  + userID},
			}},
		},
	})
}

func confirmPurchase(s *discordgo.Session, channelID, userID string, database *sql.DB, c types.Customer, lang string) {
	es := lang == "es"
	pendingMu.Lock(); p, ok := pending[userID]; if ok { delete(pending, userID) }; pendingMu.Unlock()
	if !ok || time.Now().After(p.ExpiresAt) {
		msg := "❓ *No hay ninguna compra pendiente o expiró. Usa* **`"+getPrefix()+"comprar [item]`** *primero.*"
		if !es { msg = "❓ *No pending purchase or expired. Use* **`"+getPrefix()+"buy [item]`** *first.*" }
		s.ChannelMessageSend(channelID, msg); return
	}
	executePurchase(s, channelID, userID, database, c, p, lang)
}

func confirmPurchaseInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, userID string, database *sql.DB, c types.Customer, lang string) {
	pendingMu.Lock(); p, ok := pending[userID]; if ok { delete(pending, userID) }; pendingMu.Unlock()
	if !ok || time.Now().After(p.ExpiresAt) {
		es := lang == "es"; msg := "❓ Compra expirada. Usa `"+getPrefix()+"comprar` de nuevo."; if !es { msg = "❓ Purchase expired. Use `"+getPrefix()+"buy` again." }
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg}); return
	}
	executePurchaseInteraction(s, i, userID, database, c, p, lang)
}

func executePurchase(s *discordgo.Session, channelID, userID string, database *sql.DB, c types.Customer, p *pendingPurchase, lang string) {
	es := lang == "es"
	customer, err := db.GetCustomerByID(database, c.ID)
	if err != nil || customer.KCBalance < p.PriceKC {
		msg := "❌ **Saldo insuficiente**."; if !es { msg = "❌ **Insufficient balance**." }
		s.ChannelMessageSend(channelID, msg); return
	}
	inSchedule, _ := db.IsWithinSchedule(database)
	if !inSchedule {
		schedule, _ := db.GetBotSchedule(database)
		msg := fmt.Sprintf("⏰ Los bots están fuera de horario (**%02d:00 - %02d:00** Lima). Intenta más tarde.", (schedule.StartHour-5+24)%24, (schedule.EndHour-5+24)%24)
		if !es { msg = fmt.Sprintf("⏰ Bots outside working hours (**%02d:00 - %02d:00** Lima). Try later.", (schedule.StartHour-5+24)%24, (schedule.EndHour-5+24)%24) }
		s.ChannelMessageSend(channelID, msg); return
	}
	req := types.CreateOrderRequest{ItemOfferID: p.OfferID, ItemName: p.ItemName, PriceKC: p.PriceKC, PriceVBucks: p.PriceVBucks}
	order, err := db.DeductKCAndCreateOrder(database, c.ID, customer.EpicUsername, req)
	if err != nil { s.ChannelMessageSend(channelID, fmt.Sprintf("❌ *Error: %s*", err.Error())); return }
	db.AddAuditLog(database, &c.ID, "ORDER_CREATED_BOT", fmt.Sprintf("pedido %s via Discord bot: %s por %d KC", order.ID, p.ItemName, p.PriceKC), "discord-bot")
	title := fmt.Sprintf("✅ ¡Compra realizada! **%s**", p.ItemName)
	desc  := fmt.Sprintf("El item será enviado a **%s** en cuanto el bot lo procese.", customer.EpicUsername)
	f1, f2, f3 := "🆔 Pedido", "💰 KC gastados", "📊 Nuevo saldo"
	if !es {
		title = fmt.Sprintf("✅ Purchase complete! **%s**", p.ItemName)
		desc  = fmt.Sprintf("The item will be sent to **%s** once the bot processes it.", customer.EpicUsername)
		f1 = "🆔 Order"; f2 = "💰 KC spent"; f3 = "📊 New balance"
	}
	embed := &discordgo.MessageEmbed{
		Title: title, Description: desc, Color: 0x22c55e,
		Fields: []*discordgo.MessageEmbedField{
			{Name: f1, Value: fmt.Sprintf("*%s...*", order.ID.String()[:8]), Inline: true},
			{Name: f2, Value: fmt.Sprintf("**%s %s KC**", kcCoin, fmtNum(p.PriceKC)), Inline: true},
			{Name: f3, Value: fmt.Sprintf("**%s %s KC**", kcCoin, fmtNum(customer.KCBalance-p.PriceKC)), Inline: true},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: storeURL + "/dashboard"},
	}
	s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{embed},
		Components: []discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

func executePurchaseInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, userID string, database *sql.DB, c types.Customer, p *pendingPurchase, lang string) {
	es := lang == "es"
	customer, err := db.GetCustomerByID(database, c.ID)
	if err != nil || customer.KCBalance < p.PriceKC {
		msg := "❌ **Saldo insuficiente**."; if !es { msg = "❌ **Insufficient balance**." }
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg}); return
	}
	inSchedule, _ := db.IsWithinSchedule(database)
	if !inSchedule {
		schedule, _ := db.GetBotSchedule(database)
		msg := fmt.Sprintf("⏰ Bots fuera de horario (**%02d:00 - %02d:00** Lima).", (schedule.StartHour-5+24)%24, (schedule.EndHour-5+24)%24)
		if !es { msg = fmt.Sprintf("⏰ Bots outside working hours (**%02d:00 - %02d:00** Lima).", (schedule.StartHour-5+24)%24, (schedule.EndHour-5+24)%24) }
		s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg}); return
	}
	req := types.CreateOrderRequest{ItemOfferID: p.OfferID, ItemName: p.ItemName, PriceKC: p.PriceKC, PriceVBucks: p.PriceVBucks}
	order, err := db.DeductKCAndCreateOrder(database, c.ID, customer.EpicUsername, req)
	if err != nil { msg := fmt.Sprintf("❌ *Error: %s*", err.Error()); s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &msg}); return }
	db.AddAuditLog(database, &c.ID, "ORDER_CREATED_BOT", fmt.Sprintf("pedido %s via Discord bot: %s por %d KC", order.ID, p.ItemName, p.PriceKC), "discord-bot")
	title := fmt.Sprintf("✅ ¡Compra realizada! **%s**", p.ItemName)
	desc  := fmt.Sprintf("Enviado a **%s** en breve.", customer.EpicUsername)
	f1, f2, f3 := "🆔 Pedido", "💰 KC gastados", "📊 Nuevo saldo"
	if !es {
		title = fmt.Sprintf("✅ Purchase complete! **%s**", p.ItemName)
		desc  = fmt.Sprintf("Will be sent to **%s** shortly.", customer.EpicUsername)
		f1 = "🆔 Order"; f2 = "💰 KC spent"; f3 = "📊 New balance"
	}
	embed := &discordgo.MessageEmbed{
		Title: title, Description: desc, Color: 0x22c55e,
		Fields: []*discordgo.MessageEmbedField{
			{Name: f1, Value: fmt.Sprintf("*%s...*", order.ID.String()[:8]), Inline: true},
			{Name: f2, Value: fmt.Sprintf("**%s %s KC**", kcCoin, fmtNum(p.PriceKC)), Inline: true},
			{Name: f3, Value: fmt.Sprintf("**%s %s KC**", kcCoin, fmtNum(customer.KCBalance-p.PriceKC)), Inline: true},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: storeURL + "/dashboard"},
	}
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds:     &[]*discordgo.MessageEmbed{embed},
		Components: &[]discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

// ── ?agregar ──
func sendAddBots(s *discordgo.Session, channelID, userID, lang string) {
	s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{addBotsEmbed(lang)},
		Components: []discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

func addBotsEmbed(lang string) *discordgo.MessageEmbed {
	es := lang == "es"
	bots := []string{"KidStore0001", "KidStore0002", "KidStore0003", "KidStore0004", "KidStore0005"}
	var sb strings.Builder
	for _, b := range bots { sb.WriteString(fmt.Sprintf("• `%s`\n", b)) }
	title  := "🤖 Cuentas Bot de KidStorePeru"
	desc   := "**Agrega al menos una** de estas cuentas como amigo en Epic Games."
	f1n    := "📋 Cuentas disponibles"
	f2n    := "⚠️ Importante"
	f2v    := "Fortnite requiere **48 horas** de amistad antes de poder enviarte un regalo."
	if !es {
		title = "🤖 KidStorePeru Bot Accounts"
		desc  = "**Add at least one** of these accounts as a friend in Epic Games."
		f1n   = "📋 Available accounts"
		f2n   = "⚠️ Important"
		f2v   = "Fortnite requires **48 hours** of friendship before sending a gift."
	}
	return &discordgo.MessageEmbed{
		Title: title, Description: desc, Color: 0x3b82f6,
		Fields: []*discordgo.MessageEmbedField{
			{Name: f1n, Value: sb.String()},
			{Name: f2n, Value: f2v},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: storeURL + "/bots"},
	}
}

func updateToAddBots(s *discordgo.Session, i *discordgo.InteractionCreate, userID, lang string) {
	e := addBotsEmbed(lang)
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds:     &[]*discordgo.MessageEmbed{e},
		Components: &[]discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

// ── ?vincular ──
func sendLink(s *discordgo.Session, channelID, userID, username, lang string) {
	s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{linkEmbed(username, lang)},
		Components: []discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

func linkEmbed(username, lang string) *discordgo.MessageEmbed {
	es := lang == "es"
	greeting := ""
	if username != "" {
		if es { greeting = fmt.Sprintf("Hola **%s**, sigue estos pasos:\n\n", username) } else { greeting = fmt.Sprintf("Hi **%s**, follow these steps:\n\n", username) }
	}
	linkText := "kidstoreperu.com"
	title := "🔗 Vincular cuenta"
	fn    := "Pasos"
	fv    := fmt.Sprintf("1. Ve a [%s](%s)\n2. **Inicia sesión** con tu cuenta\n3. Ve a **Mi Cuenta**\n4. Haz clic en **Conectar con Discord**\n5. Autoriza la aplicación", linkText, storeURL)
	if !es {
		title = "🔗 Link account"
		fn    = "Steps"
		fv    = fmt.Sprintf("1. Go to [%s](%s)\n2. **Log in** with your account\n3. Go to **My Account**\n4. Click **Connect with Discord**\n5. Authorize the app", linkText, storeURL)
	}
	desc := greeting
	if es { desc += "Vincula tu cuenta de Discord con KidStorePeru para comprar items de Fortnite directamente desde Discord." } else { desc += "Link your Discord account with KidStorePeru to buy Fortnite items directly from Discord." }
	return &discordgo.MessageEmbed{
		Title: title, Description: desc, Color: 0x5865f2,
		Fields: []*discordgo.MessageEmbedField{{Name: fn, Value: fv}},
	}
}

func updateToLink(s *discordgo.Session, i *discordgo.InteractionCreate, userID, lang string) {
	username := ""
	if i.Member != nil && i.Member.User != nil { username = i.Member.User.Username } else if i.User != nil { username = i.User.Username }
	e := linkEmbed(username, lang)
	s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Embeds:     &[]*discordgo.MessageEmbed{e},
		Components: &[]discordgo.MessageComponent{navMenu(userID, lang)},
	})
}

// ── No vinculado ──
func sendNotLinked(s *discordgo.Session, channelID, username, lang string) {
	es := lang == "es"
	linkText := "kidstoreperu.com"
	title := "⚠️ Cuenta no vinculada"
	desc  := fmt.Sprintf("Hola **%s**, tu Discord no está vinculado a ninguna cuenta de KidStorePeru.", username)
	fn    := "¿Cómo vincular?"
	fv    := fmt.Sprintf("1. Inicia sesión en [%s](%s)\n2. Ve a **Mi Cuenta**\n3. Haz clic en **Conectar con Discord**", linkText, storeURL)
	if !es {
		title = "⚠️ Account not linked"
		desc  = fmt.Sprintf("Hi **%s**, your Discord is not linked to any KidStorePeru account.", username)
		fn    = "How to link?"
		fv    = fmt.Sprintf("1. Log in at [%s](%s)\n2. Go to **My Account**\n3. Click **Connect with Discord**", linkText, storeURL)
	}
	s.ChannelMessageSendEmbed(channelID, &discordgo.MessageEmbed{
		Title: title, Description: desc, Color: 0xf59e0b,
		Fields: []*discordgo.MessageEmbedField{{Name: fn, Value: fv}},
	})
}

// ── Helpers de tienda ──
func fetchShopItems(lang string) []shopItem {
	shopLang := "es-419"; if lang == "en" { shopLang = "en" }
	resp, err := http.Get(fmt.Sprintf("https://backend-discord-bot-kidstore-production.up.railway.app/store/shop?lang=%s", shopLang))
	if err != nil { return nil }
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var data struct {
		Data struct {
			Entries []struct {
				OfferId    string `json:"offerId"`
				FinalPrice int    `json:"finalPrice"`
				Bundle     *struct{ Name string `json:"name"` } `json:"bundle"`
				BrItems    []struct{ Name string `json:"name"` } `json:"brItems"`
				Tracks     []struct{ Title string `json:"title"` } `json:"tracks"`
				Layout     *struct{ Name string `json:"name"` } `json:"layout"`
			} `json:"entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &data); err != nil { return nil }
	var items []shopItem
	for _, e := range data.Data.Entries {
		var name, section string
		if e.Bundle != nil { name = e.Bundle.Name } else if len(e.BrItems) > 0 { name = e.BrItems[0].Name } else if len(e.Tracks) > 0 { name = e.Tracks[0].Title }
		if e.Layout != nil { section = e.Layout.Name }
		if name == "" { continue }
		kc := int(math.Ceil(float64(e.FinalPrice) * 0.5))
		items = append(items, shopItem{OfferID: e.OfferId, Name: name, VBucks: e.FinalPrice, KC: kc, Section: section})
	}
	return items
}

func filterItems(items []shopItem, query string) []shopItem {
	q := strings.ToLower(query)
	var result []shopItem
	for _, it := range items {
		if strings.Contains(strings.ToLower(it.Name), q) || strings.Contains(strings.ToLower(it.Section), q) { result = append(result, it) }
	}
	return result
}

// ── Helpers generales ──
func levelStr(kc int, lang string) string {
	switch { case kc >= 10000: return "👑 Legend"; case kc >= 4000: return "🔥 Pro"; case kc >= 1000: return "🎮 Gamer"; default: return "⚡ Starter" }
}

func fmtNum(n int) string {
	s := fmt.Sprintf("%d", n); if len(s) <= 3 { return s }
	var r strings.Builder
	for i, c := range s { if i > 0 && (len(s)-i)%3 == 0 { r.WriteRune('.') }; r.WriteRune(c) }
	return r.String()
}

func roundCents(n float64) float64 { return math.Round(n*100) / 100 }

func statusEmoji(status string) string {
	switch status { case "sent": return "✅"; case "pending": return "⏳"; case "processing": return "⚙️"; case "failed": return "❌"; case "refunded": return "↩️"; default: return "❓" }
}

func statusStr(status, lang string) string {
	if lang == "es" { switch status { case "sent": return "Enviado"; case "pending": return "Pendiente"; case "processing": return "Procesando"; case "failed": return "Fallido"; case "refunded": return "Reembolsado"; default: return status } }
	switch status { case "sent": return "Sent"; case "pending": return "Pending"; case "processing": return "Processing"; case "failed": return "Failed"; case "refunded": return "Refunded"; default: return status }
}

func truncate(s string, max int) string { if len(s) <= max { return s }; return s[:max-3] + "..." }

func respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, msg string) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: msg, Flags: discordgo.MessageFlagsEphemeral},
	})
}

var _ = uuid.Nil
var _ = kcKid
