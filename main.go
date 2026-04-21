package main

import (
	"KidStoreStore/src/admin"
	"KidStoreStore/src/autobuyer"
	"KidStoreStore/src/db"
	"KidStoreStore/src/discord"
	"KidStoreStore/src/fortnite"
	"KidStoreStore/src/middleware"
	"KidStoreStore/src/store"
	"KidStoreStore/src/types"
	"context"
	"database/sql"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	_ "github.com/lib/pq"
)

func main() {
	if _, err := os.Stat(".env"); err == nil {
		if err := godotenv.Load(); err != nil {
			log.Fatalf("Error cargando .env: %v", err)
		}
	}

	var cfg types.EnvConfig
	if err := envconfig.Process("", &cfg); err != nil {
		log.Fatalf("Error procesando variables de entorno: %v", err)
	}

	fortnite.Init(cfg.EpicClient, cfg.EpicSecret, cfg.EncryptionKey)
	store.SetEncryptionKey(cfg.EncryptionKey)
	store.SetExchangeRateAPIKey(cfg.ExchangeRateAPIKey)
	store.SetSMTPConfig(cfg)
	store.SetPaymentInfoJSON(cfg.PaymentInfoJSON)

	// Determine backend URL for webhooks
	backendURL := fmt.Sprintf("http://localhost:%s", cfg.Port)
	if cfg.FrontendURL != "http://localhost:5173" {
		// Production: derive backend URL from frontend URL pattern
		backendURL = "https://backend-discord-bot-kidstore-production.up.railway.app"
	}
	store.SetPaymentConfig(store.PaymentConfig{
		MercadoPagoToken:    cfg.MercadoPagoAccessToken,
		PayPalClientID:      cfg.PayPalClientID,
		PayPalClientSecret:  cfg.PayPalClientSecret,
		PayPalMode:          cfg.PayPalMode,
		NOWPaymentsAPIKey:   cfg.NOWPaymentsAPIKey,
		FrontendURL:         cfg.FrontendURL,
		BackendURL:          backendURL,
	})

	autobuyer.Init(cfg.AutobuyerURL, cfg.AutobuyerAPIKey)
	store.SetActivationBackendURL(backendURL)

	psqlInfo := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName)
	database, err := sql.Open("postgres", psqlInfo)
	if err != nil { log.Fatalf("Error abriendo DB: %v", err) }
	defer database.Close()

	database.SetMaxOpenConns(25)
	database.SetMaxIdleConns(10)
	database.SetConnMaxLifetime(5 * time.Minute)
	database.SetConnMaxIdleTime(3 * time.Minute)

	if err := database.Ping(); err != nil { log.Fatalf("Error conectando a DB: %v", err) }
	slog.Info("Conectado a PostgreSQL")

	if err := db.CreateTables(database); err != nil { log.Fatalf("Error creando tablas: %v", err) }
	slog.Info("Tablas verificadas")

	if cfg.EncryptionKey != "" {
		if err := db.MigrateEncryptTokens(database, cfg.EncryptionKey); err != nil {
			log.Fatalf("Error migrando tokens encriptados: %v", err)
		}
		slog.Info("Tokens encriptados verificados")
	}

	authLimiter  := middleware.NewIPRateLimiter(5, time.Minute)
	orderLimiter := middleware.NewIPRateLimiter(10, time.Minute)
	adminLimiter := middleware.NewIPRateLimiter(30, time.Minute)

	gin.SetMode(gin.ReleaseMode)
	router := gin.Default()
	router.Use(gin.Logger())
	router.Use(gin.Recovery())

	// Construir lista de orígenes permitidos incluyendo siempre los dominios de producción
	allowedOrigins := []string{
		"https://www.kidstoreperu.net",
		"https://kidstoreperu.net",
		"https://frontend-discord-bot-kidstore-production.up.railway.app",
		"http://localhost:5173",
		"http://localhost:5174",
		"http://localhost:3000",
	}
	// Añadir FRONTEND_URL si es diferente a los anteriores
	if cfg.FrontendURL != "" {
		found := false
		for _, o := range allowedOrigins {
			if o == cfg.FrontendURL { found = true; break }
		}
		if !found { allowedOrigins = append(allowedOrigins, cfg.FrontendURL) }
	}
	slog.Info("CORS configurado", "origins", allowedOrigins)

	router.Use(cors.New(cors.Config{
		AllowOrigins:     allowedOrigins,
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "X-Admin-Key", "X-Approved-By", "X-Lang"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	router.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"service": "KidStore Store API", "status": "ok"})
	})

	// ── Discord OAuth ──
	discordCfg := discord.Config{
		ClientID:     cfg.DiscordClientID,
		ClientSecret: cfg.DiscordClientSecret,
		RedirectURL:  cfg.DiscordRedirectURL,
		FrontendURL:  cfg.FrontendURL,
		BotToken:     cfg.DiscordBotToken,
		SecretKey:    cfg.SecretKey,
	}
	router.GET("/discord/auth",     discord.HandlerGetAuthURL(discordCfg))
	router.GET("/discord/callback", discord.HandlerCallback(database, discordCfg))

	// ── Verificación de email (pública) ──
	router.GET("/store/verify-email", store.HandlerVerifyEmail(database, cfg.SecretKey))

	// ── Auth pública (con rate limit) ──
	authGroup := router.Group("/store")
	authGroup.Use(middleware.RateLimitMiddleware(authLimiter))
	{
		authGroup.POST("/register",            store.HandlerRegister(database, cfg.SecretKey, cfg))
		authGroup.POST("/login",               store.HandlerLogin(database, cfg.SecretKey))
		authGroup.POST("/forgot-password",     store.HandlerForgotPassword(database, cfg))
		authGroup.POST("/reset-password",      store.HandlerResetPassword(database))
		authGroup.POST("/resend-verification", store.HandlerResendVerification(database, cfg))
		authGroup.POST("/refresh-token",      store.HandlerRefreshToken(database, cfg.SecretKey))
	}

	// Payment webhooks (public, no auth — called by gateways)
	router.POST("/store/webhook/mercadopago", store.HandlerMercadoPagoWebhook(database))
	router.POST("/store/webhook/paypal",      store.HandlerPayPalWebhook(database))
	router.POST("/store/webhook/nowpayments", store.HandlerNOWPaymentsWebhook(database))
	router.POST("/store/paypal-capture",       store.HandlerPayPalCapture(database))
	router.POST("/store/webhook/autobuyer",   store.HandlerAutobuyerWebhook(database))
	router.GET("/store/shop",            store.HandlerGetShop)
	router.GET("/store/bots-status",     store.HandlerBotsStatus(database))
	router.GET("/store/exchange-rates",  store.HandlerGetExchangeRates)
	router.GET("/store/product-available/:id", admin.HandlerCheckProductAvailable(database))
	router.GET("/store/discord-lang/:discord_id", func(c *gin.Context) {
		lang, err := db.GetDiscordLang(database, c.Param("discord_id"))
		if err != nil || lang == "" { lang = "es" }
		c.JSON(200, gin.H{"lang": lang})
	})

	// ── Chat proxy (public — no auth, session-based) ──
	router.POST("/store/chat/start",    store.HandlerChatStart())
	router.POST("/store/chat/message",  store.HandlerChatMessage())
	router.GET("/store/chat/poll/:sid", store.HandlerChatPoll())

	// ── Rutas de cliente (JWT requerido) ──
	customer := router.Group("/store")
	customer.Use(middleware.CustomerAuthMiddleware(cfg.SecretKey))
	{
		customer.GET("/me",                store.HandlerMe(database))
		customer.GET("/payment-info",      store.HandlerGetPaymentInfo())
		customer.POST("/payment",              middleware.RateLimitMiddleware(orderLimiter), store.HandlerCreatePayment(database))
		customer.GET("/payment-status/:id",    store.HandlerPaymentStatus(database))
		customer.POST("/activate",             store.HandlerActivate(database))
		customer.GET("/activation-status/:code", store.HandlerActivationStatus(database))
		customer.POST("/activation-input/:code", store.HandlerActivationInput(database))
		customer.GET("/orders",            store.HandlerGetMyOrders(database))
		customer.GET("/recharges",         store.HandlerGetMyRecharges(database))
		customer.PUT("/profile",           store.HandlerUpdateProfile(database, cfg.SecretKey))
		customer.POST("/link-discord",     store.HandlerLinkDiscord(database))
		customer.DELETE("/unlink-discord", store.HandlerUnlinkDiscord(database))
		customer.POST("/order",
			middleware.RateLimitMiddleware(orderLimiter),
			store.HandlerCreateOrder(database),
		)
	}

	// ── Admin (API Key + rate limit) ──
	adminGroup := router.Group("/admin")
	adminGroup.Use(middleware.RateLimitMiddleware(adminLimiter))
	adminGroup.Use(middleware.AdminAuthMiddleware(cfg.AdminAPIKey, cfg.SecretKey))
	{
		adminGroup.GET("/customers",        admin.HandlerGetAllCustomers(database))
		adminGroup.GET("/customers/:id",    admin.HandlerGetCustomer(database))
		adminGroup.PUT("/customers/:id",    admin.HandlerUpdateCustomer(database))
		adminGroup.DELETE("/customers/:id", admin.HandlerDeleteCustomer(database))
		adminGroup.POST("/recharge",        admin.HandlerRechargeKC(database))
		adminGroup.GET("/orders",           admin.HandlerGetAllOrders(database))
		adminGroup.GET("/stats",            admin.HandlerGetStats(database))
		adminGroup.GET("/payments",         admin.HandlerGetAllPayments(database))
		adminGroup.GET("/product-availability",  admin.HandlerGetProductAvailability(database))
		adminGroup.PUT("/product-availability",  admin.HandlerUpdateProductAvailability(database))
		adminGroup.GET("/bot-schedule",     admin.HandlerGetBotSchedule(database))
		adminGroup.PUT("/bot-schedule",     admin.HandlerUpdateBotSchedule(database))
		adminGroup.GET("/bots",             fortnite.HandlerGetBotAccounts(database))
		adminGroup.POST("/bots/connect",    fortnite.HandlerConnectBotAccount(database))
		adminGroup.POST("/bots/finish",     fortnite.HandlerFinishConnectBotAccount(database))
		adminGroup.POST("/bots/disconnect", fortnite.HandlerDisconnectBotAccount(database))
		adminGroup.POST("/bots/gifts",      fortnite.HandlerUpdateRemainingGifts(database))
		adminGroup.POST("/bots/vbucks",     fortnite.HandlerUpdateBotVbucks(database))
		adminGroup.POST("/bots/verify",     fortnite.HandlerVerifyBotTokens(database))
		adminGroup.PUT("/payments/:id",     admin.HandlerUpdatePayment(database))
		adminGroup.DELETE("/payments/:id",  admin.HandlerDeletePayment(database))
		adminGroup.GET("/check",            admin.HandlerAdminCheck(database))
	}

	// ── Payment expiration goroutine (expire pending payments after 30 min) ──
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			if n, err := db.ExpirePendingPayments(database); err != nil {
				slog.Error("Error expirando pagos", "error", err)
			} else if n > 0 {
				slog.Info("Pagos pendientes expirados", "count", n)
			}
		}
	}()

	// ── Discord Bot ──
	var discordSession interface{ Close() error }
	if cfg.DiscordBotToken != "" {
		session, err := discord.StartBot(database, cfg.DiscordBotToken, cfg.DiscordGuildID)
		if err != nil {
			slog.Warn("Error iniciando bot de Discord", "error", err)
		} else {
			discordSession = session
			store.SetDiscordNotifier(discord.SendOrderNotification)
			store.SetProductPurchaseNotifier(discord.SendProductPurchaseNotification)
			slog.Info("Bot de Discord iniciado")
		}
	}

	// ── Workers ──
	workerCtx, workerCancel := context.WithCancel(context.Background())
	store.StartOrderWorker(workerCtx, database)
	fortnite.StartFriendRequestAcceptor(database, 300)
	fortnite.StartTokenHealthCheck(database, cfg.BotCheckInterval)
	slog.Info("Workers iniciados", "workers", "pedidos, amigos, health check")

	port := cfg.Port
	if port == "" { port = "8081" }
	srv := &http.Server{Addr: ":" + port, Handler: router}

	go func() {
		slog.Info("KidStore Store API iniciado", "port", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Error iniciando servidor: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	slog.Info("Apagando servidor...")
	workerCancel()
	if discordSession != nil {
		discordSession.Close()
		slog.Info("Bot de Discord desconectado")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	slog.Info("Servidor detenido limpiamente")
}
