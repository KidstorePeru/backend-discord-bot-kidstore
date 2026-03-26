package main

import (
	"KidStoreStore/src/admin"
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

	fortnite.Init(cfg.EpicClient, cfg.EpicSecret)

	psqlInfo := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName)
	database, err := sql.Open("postgres", psqlInfo)
	if err != nil { log.Fatalf("Error abriendo DB: %v", err) }
	defer database.Close()

	if err := database.Ping(); err != nil { log.Fatalf("Error conectando a DB: %v", err) }
	fmt.Println("✅ Conectado a PostgreSQL")

	if err := db.CreateTables(database); err != nil { log.Fatalf("Error creando tablas: %v", err) }
	fmt.Println("✅ Tablas verificadas")

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
	fmt.Printf("🌐 CORS orígenes permitidos: %v\n", allowedOrigins)

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
	}

	router.GET("/store/shop",        store.HandlerGetShop)
	router.GET("/store/bots-status", store.HandlerBotsStatus(database))

	// ── Rutas de cliente (JWT requerido) ──
	customer := router.Group("/store")
	customer.Use(middleware.CustomerAuthMiddleware(cfg.SecretKey))
	{
		customer.GET("/me",                store.HandlerMe(database))
		customer.GET("/orders",            store.HandlerGetMyOrders(database))
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
	adminGroup.Use(middleware.AdminAuthMiddleware(cfg.AdminAPIKey))
	{
		adminGroup.GET("/customers",        admin.HandlerGetAllCustomers(database))
		adminGroup.GET("/customers/:id",    admin.HandlerGetCustomer(database))
		adminGroup.PUT("/customers/:id",    admin.HandlerUpdateCustomer(database))
		adminGroup.DELETE("/customers/:id", admin.HandlerDeleteCustomer(database))
		adminGroup.POST("/recharge",        admin.HandlerRechargeKC(database))
		adminGroup.GET("/orders",           admin.HandlerGetAllOrders(database))
		adminGroup.GET("/stats",            admin.HandlerGetStats(database))
		adminGroup.GET("/bot-schedule",     admin.HandlerGetBotSchedule(database))
		adminGroup.PUT("/bot-schedule",     admin.HandlerUpdateBotSchedule(database))
		adminGroup.GET("/bots",             fortnite.HandlerGetBotAccounts(database))
		adminGroup.POST("/bots/connect",    fortnite.HandlerConnectBotAccount(database))
		adminGroup.POST("/bots/finish",     fortnite.HandlerFinishConnectBotAccount(database))
		adminGroup.POST("/bots/disconnect", fortnite.HandlerDisconnectBotAccount(database))
		adminGroup.POST("/bots/gifts",      fortnite.HandlerUpdateRemainingGifts(database))
		adminGroup.POST("/bots/vbucks",     fortnite.HandlerUpdateBotVbucks(database))
		adminGroup.POST("/bots/verify",     fortnite.HandlerVerifyBotTokens(database))
	}

	// ── Discord Bot ──
	var discordSession interface{ Close() error }
	if cfg.DiscordBotToken != "" {
		session, err := discord.StartBot(database, cfg.DiscordBotToken)
		if err != nil {
			fmt.Printf("⚠️ Error iniciando bot de Discord: %v\n", err)
		} else {
			discordSession = session
			store.SetDiscordNotifier(discord.SendOrderNotification)
			fmt.Println("✅ Bot de Discord iniciado")
		}
	}

	// ── Workers ──
	workerCtx, workerCancel := context.WithCancel(context.Background())
	store.StartOrderWorker(workerCtx, database)
	fortnite.StartFriendRequestAcceptor(database, 300)
	fortnite.StartTokenHealthCheck(database, cfg.BotCheckInterval)
	fmt.Println("✅ Workers iniciados: pedidos, amigos, health check de tokens")

	port := cfg.Port
	if port == "" { port = "8081" }
	srv := &http.Server{Addr: ":" + port, Handler: router}

	go func() {
		fmt.Printf("🚀 KidStore Store API corriendo en puerto %s\n", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Error iniciando servidor: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	fmt.Println("\n⏳ Apagando servidor...")
	workerCancel()
	if discordSession != nil {
		discordSession.Close()
		fmt.Println("✅ Bot de Discord desconectado")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	fmt.Println("✅ Servidor detenido limpiamente.")
}
