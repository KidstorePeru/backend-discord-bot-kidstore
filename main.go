package main

import (
	"KidStoreStore/src/admin"
	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"
	"KidStoreStore/src/fortnite"
	"KidStoreStore/src/middleware"
	"KidStoreStore/src/oauth"
	"KidStoreStore/src/safe"
	"KidStoreStore/src/store"
	"KidStoreStore/src/types"
	"context"
	"database/sql"
	"encoding/hex"
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

	// ── Validación de secretos críticos — fallar rápido y ruidoso en vez de
	// arrancar "normal" con un hueco de seguridad silencioso ──
	// SECRET_KEY firma TODOS los JWT (clientes y admin). Si estuviera vacía,
	// el servidor arrancaría sin problema pero firmaría los tokens con una
	// clave vacía — cualquiera podría forjar un token con is_admin:true sin
	// necesitar contraseña ni exploit alguno, solo un editor de texto. Es el
	// hueco de seguridad más grave posible, así que se rechaza arrancar.
	if len(cfg.SecretKey) < 32 {
		log.Fatalf("SECRET_KEY debe estar configurada con al menos 32 caracteres — sin esto cualquiera podría forjar un token de administrador. Configúrala antes de iniciar el servidor.")
	}
	// ENCRYPTION_KEY protege los tokens de las cuentas bot en la base de
	// datos. Si estuviera vacía, crypto.Encrypt/Decrypt caen en un modo de
	// compatibilidad que guarda los tokens SIN cifrar en texto plano — mejor
	// fallar aquí que dejarlo pasar en silencio.
	if cfg.EncryptionKey == "" {
		log.Fatalf("ENCRYPTION_KEY debe estar configurada — sin esto los tokens de las cuentas bot se guardarían sin cifrar en la base de datos.")
	}
	if keyBytes, err := hex.DecodeString(cfg.EncryptionKey); err != nil || len(keyBytes) != 32 {
		log.Fatalf("ENCRYPTION_KEY inválida: debe ser exactamente 64 caracteres hexadecimales (32 bytes).")
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
		DLocalGoAPIKey:      cfg.DLocalGoAPIKey,
		DLocalGoSecretKey:   cfg.DLocalGoSecretKey,
		DLocalGoSandbox:     cfg.DLocalGoSandbox,
		FrontendURL:         cfg.FrontendURL,
		BackendURL:          backendURL,
	})

	// sslmode=require — la conexión a Postgres viaja por la red pública de
	// Railway (el host es un proxy público, caboose.proxy.rlwy.net), no por
	// una red interna. Con sslmode=disable, cada consulta SQL — contraseñas
	// hasheadas, correos, tokens, todo — viajaba sin cifrar por esa red
	// pública. "require" cifra el canal (protege contra cualquiera
	// escuchando el tráfico) aunque no valide el certificado contra una CA
	// conocida — Railway no publica uno; validar el certificado exigiría
	// distribuir su CA propia, que no vale la pena acá.
	psqlInfo := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=require",
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

	authLimiter    := middleware.NewIPRateLimiter(5, time.Minute)
	orderLimiter   := middleware.NewIPRateLimiter(10, time.Minute)
	adminLimiter   := middleware.NewIPRateLimiter(30, time.Minute)
	// Los webhooks de pago son rutas públicas sin autenticación por diseño
	// (las llaman las pasarelas) — cada solicitud dispara una llamada saliente
	// real a la API de PayPal/MercadoPago/etc. para verificar el pago. Sin un
	// límite, cualquiera podría bombardear estas rutas para gastar cuota de
	// esas APIs o sobrecargar el servidor. 40/min es generoso para tráfico
	// legítimo de pasarelas reales, pero frena un abuso automatizado.
	webhookLimiter := middleware.NewIPRateLimiter(40, time.Minute)
	// Libro de Reclamaciones: público (cualquier consumidor, sin cuenta) —
	// límite generoso para uso legítimo pero que frena un bombardeo automatizado.
	complaintLimiter := middleware.NewIPRateLimiter(5, time.Hour)

	gin.SetMode(gin.ReleaseMode)
	// gin.Default() ya trae Logger + Recovery — no hace falta (ni conviene)
	// volver a registrarlos, eso duplicaba cada línea de log en producción.
	router := gin.Default()

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

	// Cabeceras de seguridad estándar — barato de agregar, sin downside real.
	router.Use(func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		c.Next()
	})

	router.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"service": "KidStore Store API", "status": "ok"})
	})

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
		authGroup.POST("/logout",             store.HandlerLogout(database))
		authGroup.POST("/login/2fa",          store.HandlerLoginVerify2FA(database, cfg.SecretKey))
	}

	// ── OAuth: login/registro con Google y Discord (con rate limit) ──
	oauthCfg := oauth.Config{
		GoogleClientID:      cfg.GoogleClientID,
		GoogleClientSecret:  cfg.GoogleClientSecret,
		GoogleRedirectURL:   cfg.GoogleRedirectURL,
		DiscordClientID:     cfg.DiscordClientID,
		DiscordClientSecret: cfg.DiscordClientSecret,
		DiscordRedirectURL:  cfg.DiscordRedirectURL,
		FrontendURL:         cfg.FrontendURL,
		SecretKey:           cfg.SecretKey,
	}
	authRateLimited := router.Group("/auth")
	authRateLimited.Use(middleware.RateLimitMiddleware(authLimiter))
	{
		authRateLimited.GET("/google",                oauth.HandlerGoogleAuth(oauthCfg))
		authRateLimited.GET("/discord",                oauth.HandlerDiscordAuth(oauthCfg))
		authRateLimited.POST("/complete-registration", oauth.HandlerCompleteRegistration(database, oauthCfg))
		authRateLimited.POST("/exchange",              oauth.HandlerExchangeOAuthCode(database))
	}
	// Los callbacks los invoca el navegador redirigido por Google/Discord — sin rate limit por IP del cliente
	router.GET("/auth/google/callback", oauth.HandlerGoogleCallback(database, oauthCfg))
	router.GET("/auth/discord/callback", oauth.HandlerDiscordCallback(database, oauthCfg))
	router.GET("/auth/pending/:token", oauth.HandlerGetPendingRegistration(database))

	// Payment webhooks (public, no auth — called by gateways; con rate limit
	// por IP para que no se puedan bombardear)
	webhookGroup := router.Group("/store")
	webhookGroup.Use(middleware.RateLimitMiddleware(webhookLimiter))
	{
		webhookGroup.POST("/webhook/mercadopago", store.HandlerMercadoPagoWebhook(database))
		webhookGroup.POST("/webhook/paypal",      store.HandlerPayPalWebhook(database))
		webhookGroup.POST("/webhook/nowpayments", store.HandlerNOWPaymentsWebhook(database))
		webhookGroup.POST("/webhook/dlocalgo",    store.HandlerDLocalGoWebhook(database))
		webhookGroup.POST("/paypal-capture",      store.HandlerPayPalCapture(database))
	}
	router.GET("/store/shop",            store.HandlerGetShop)
	router.GET("/store/bots-status",     store.HandlerBotsStatus(database))
	router.GET("/store/exchange-rates",  store.HandlerGetExchangeRates)
	router.GET("/store/product-available/:id", admin.HandlerCheckProductAvailable(database))

	// Libro de Reclamaciones Virtual — público, no requiere cuenta (requisito
	// legal en Perú: cualquier consumidor debe poder presentar un reclamo).
	complaintGroup := router.Group("/store")
	complaintGroup.Use(middleware.RateLimitMiddleware(complaintLimiter))
	{
		complaintGroup.POST("/complaints",              store.HandlerCreateComplaint(database, cfg))
		complaintGroup.GET("/complaints/:reference",    store.HandlerGetComplaintStatus(database))
	}

	// ── Rutas de cliente (JWT requerido) ──
	customer := router.Group("/store")
	customer.Use(middleware.CustomerAuthMiddleware(cfg.SecretKey))
	{
		customer.GET("/me",                store.HandlerMe(database))
		customer.GET("/payment-info",      store.HandlerGetPaymentInfo())
		customer.POST("/payment",              middleware.RateLimitMiddleware(orderLimiter), store.HandlerCreatePayment(database))
		customer.GET("/payment-status/:id",    store.HandlerPaymentStatus(database))
		customer.POST("/payment/:id/cancel",   store.HandlerCancelPayment(database))
		customer.GET("/voucher/payment/:id",   store.HandlerPaymentVoucher(database))
		customer.GET("/voucher/order/:id",     store.HandlerOrderVoucher(database))
		customer.GET("/voucher/recharge/:id",  store.HandlerRechargeVoucher(database))
		customer.GET("/orders",            store.HandlerGetMyOrders(database))
		customer.GET("/orders/stats",      store.HandlerGetMyOrderStats(database))
		customer.GET("/recharges",         store.HandlerGetMyRecharges(database))
		customer.GET("/recharges/stats",   store.HandlerGetMyRechargeStats(database))
		customer.PUT("/profile",           store.HandlerUpdateProfile(database, cfg.SecretKey))
		customer.PUT("/avatar",            store.HandlerUpdateAvatar(database))
		customer.POST("/email/request-change", middleware.RateLimitMiddleware(authLimiter), store.HandlerRequestEmailChange(database, cfg))
		customer.POST("/email/confirm-change", middleware.RateLimitMiddleware(authLimiter), store.HandlerConfirmEmailChange(database, cfg.SecretKey))
		customer.POST("/link/:provider/start", oauth.HandlerStartLink(oauthCfg))
		customer.DELETE("/link/:provider",     oauth.HandlerUnlinkProvider(database))
		customer.POST("/2fa/setup",         middleware.RateLimitMiddleware(authLimiter), store.HandlerSetup2FA(database))
		customer.POST("/2fa/confirm",       middleware.RateLimitMiddleware(authLimiter), store.HandlerConfirm2FA(database))
		customer.POST("/2fa/disable",       middleware.RateLimitMiddleware(authLimiter), store.HandlerDisable2FA(database))
		customer.GET("/2fa/status",         store.HandlerGet2FAStatus(database))
		customer.DELETE("/account",         middleware.RateLimitMiddleware(authLimiter), store.HandlerDeleteOwnAccount(database))
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
		adminGroup.GET("/complaints",       admin.HandlerGetAllComplaints(database))
		adminGroup.PUT("/complaints/:id/respond", admin.HandlerRespondComplaint(database, cfg))
		adminGroup.PUT("/complaints/:id/close",   admin.HandlerCloseComplaint(database))
	}

	// ── Payment expiration goroutine (expire pending payments after 30 min) ──
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			safe.Run("ExpirePendingPayments", func() {
				if n, err := db.ExpirePendingPayments(database); err != nil {
					slog.Error("Error expirando pagos", "error", err)
				} else if n > 0 {
					slog.Info("Pagos pendientes expirados", "count", n)
				}
			})
		}
	}()

	// ── Conciliación de pagos pendientes con las pasarelas (red de seguridad
	// para cuando un webhook nunca llega o se pierde en el camino) ──
	go func() {
		for {
			time.Sleep(2 * time.Minute)
			safe.Run("ReconcilePendingPayments", func() {
				store.ReconcilePendingPayments(database)
			})
		}
	}()

	// ── Reintento de reembolsos de pedidos que quedaron pendientes de
	// confirmar (ver failOrderAndRefund / RetryFailedRefunds) ──
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			safe.Run("RetryFailedRefunds", func() {
				store.RetryFailedRefunds(database)
			})
		}
	}()

	// ── Reset diario de gifts de bots (remaining_gifts vuelve a 5 cada día) ──
	go func() {
		for {
			safe.Run("ResetDailyGifts", func() {
				if n, err := db.ResetDailyGifts(database); err != nil {
					slog.Error("Error reseteando gifts diarios", "error", err)
				} else if n > 0 {
					slog.Info("Gifts diarios reseteados", "cuentas", n)
					discordbot.ClearAllNoGiftSlotsAlerts()
				}
			})
			time.Sleep(10 * time.Minute)
		}
	}()

	// ── Workers ──
	workerCtx, workerCancel := context.WithCancel(context.Background())
	store.StartOrderWorker(workerCtx, database)
	fortnite.StartFriendRequestAcceptor(database, 300)
	fortnite.StartFriendship48hChecker(database, 900)
	fortnite.StartTokenHealthCheck(database, cfg.BotCheckInterval)
	slog.Info("Workers iniciados", "workers", "pedidos, amigos, health check")

	discordbot.SetEmailSender(store.SendPaymentApprovedEmail)
	discordbot.Start(cfg, database)

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
	discordbot.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	slog.Info("Servidor detenido limpiamente")
}
