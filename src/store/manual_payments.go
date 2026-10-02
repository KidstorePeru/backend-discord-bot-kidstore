package store

// Pagos manuales con comprobante subido desde la web (Yape, Plin, BCP,
// Interbank, BBVA, Bizum): el cliente paga por su cuenta, sube la captura y
// el admin la revisa y acredita (panel o Discord).

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // decodificador PNG
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"KidStoreStore/src/db"
	"KidStoreStore/src/discordbot"
	"KidStoreStore/src/safe"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // decodificador WebP
)

// ProofStore — dónde se guardan los comprobantes (Cloudflare R2).
type ProofStore interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

var (
	proofStoreMu  sync.RWMutex
	proofStore    ProofStore
	proofKey      []byte // clave AES-256 para cifrar los comprobantes
	proofSignKey  []byte // clave para firmar los enlaces de "ver comprobante"
	publicAPIBase = "https://api.kidstoreperu.net"
)

// SetManualPaymentsConfig activa la subida de comprobantes. Sin almacenamiento
// o sin ENCRYPTION_KEY la función queda desactivada (la web ofrece WhatsApp).
func SetManualPaymentsConfig(store ProofStore, encryptionKey, secretKey, apiBase string) {
	proofStoreMu.Lock()
	defer proofStoreMu.Unlock()
	proofStore = store
	proofKey, proofSignKey = nil, nil
	if encryptionKey != "" {
		k := sha256.Sum256([]byte("kidstore-proofs:" + encryptionKey))
		proofKey = k[:]
	}
	if secretKey != "" {
		k := sha256.Sum256([]byte("kidstore-proof-links:" + secretKey))
		proofSignKey = k[:]
	}
	if apiBase != "" {
		publicAPIBase = strings.TrimSuffix(apiBase, "/")
	}
}

func manualUploadsEnabled() bool {
	proofStoreMu.RLock()
	defer proofStoreMu.RUnlock()
	return proofStore != nil && proofKey != nil && proofSignKey != nil
}

// ==================== PRECIO (lo calcula SIEMPRE el servidor) ====================

type manualMethod struct {
	Currency   string
	Commission float64 // sobre el precio en la divisa (Bizum 1.5%)
}

// Deben coincidir con METHODS en Recharge.tsx.
var manualMethods = map[string]manualMethod{
	"yape":      {Currency: "PEN"},
	"plin":      {Currency: "PEN"},
	"bcp":       {Currency: "PEN"},
	"interbank": {Currency: "PEN"},
	"bbva":      {Currency: "PEN"},
	"bizum":     {Currency: "EUR", Commission: 0.015},
}

type manualQuote struct {
	PackageID   string
	PackageName string
	KC          int
	AmountPEN   float64 // precio base en soles
	Amount      float64 // lo que debe pagar, en Currency
	Currency    string
}

var errManualInvalid = errors.New("paquete o método inválido")

// quoteManual calcula el monto con las mismas reglas que la página de
// recarga (mismo precio que el pago automático; Bizum en euros + 1.5%).
func quoteManual(packageID string, customKC int, method string, eurRate float64) (manualQuote, error) {
	m, ok := manualMethods[method]
	if !ok {
		return manualQuote{}, errManualInvalid
	}
	q := manualQuote{PackageID: packageID, Currency: m.Currency}
	if packageID == "custom" {
		if customKC < minCustomKC || customKC > maxCustomKC {
			return manualQuote{}, errManualInvalid
		}
		q.KC = customKC
		q.PackageName = fmt.Sprintf("%d KC (personalizado)", customKC)
		q.AmountPEN = math.Round(float64(customKC)*kcRatePEN*100) / 100
	} else {
		p, ok := productPrices[packageID]
		if !ok || p.KCAmount <= 0 {
			return manualQuote{}, errManualInvalid
		}
		q.KC, q.PackageName, q.AmountPEN = p.KCAmount, p.Name, p.PricePEN
	}
	if q.AmountPEN <= 0 {
		return manualQuote{}, errManualInvalid
	}
	amount := q.AmountPEN
	if m.Currency == "EUR" {
		if eurRate <= 0 {
			return manualQuote{}, errManualInvalid
		}
		amount = q.AmountPEN * eurRate
	}
	if m.Commission > 0 {
		amount = math.Ceil(amount*(1+m.Commission)*100-1e-9) / 100
	} else {
		amount = math.Round(amount*100) / 100
	}
	q.Amount = amount
	return q, nil
}

// ==================== COMPROBANTE: validación, limpieza y cifrado ====================

const (
	maxProofUpload  = 6 << 20 // lo que se acepta recibir
	maxProofPixels  = 50_000_000
	maxProofSide    = 12_000
	proofTargetSide = 2000 // se reduce a este lado máximo
)

var (
	errProofEmpty       = errors.New("el comprobante está vacío")
	errProofTooLarge    = errors.New("el comprobante supera 5 MB")
	errProofUnsupported = errors.New("formato no permitido: sube una imagen (JPG, PNG, WebP) o un PDF")
	errProofDamaged     = errors.New("no se pudo leer la imagen; prueba con otra captura")
)

// processProof valida el archivo por su contenido real (no por su nombre) y
// devuelve la versión que se guarda: las imágenes se vuelven a codificar en
// JPEG (eso borra los datos ocultos, como la ubicación del celular) y se
// reducen si son muy grandes; los PDF se guardan tal cual.
func processProof(data []byte) ([]byte, string, error) {
	if len(data) == 0 {
		return nil, "", errProofEmpty
	}
	if len(data) > maxProofUpload {
		return nil, "", errProofTooLarge
	}
	switch http.DetectContentType(data) {
	case "application/pdf":
		if !bytes.HasPrefix(data, []byte("%PDF-")) {
			return nil, "", errProofUnsupported
		}
		return data, "application/pdf", nil
	case "image/jpeg", "image/png", "image/webp":
	default:
		return nil, "", errProofUnsupported
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, "", errProofDamaged
	}
	if cfg.Width > maxProofSide || cfg.Height > maxProofSide || cfg.Width*cfg.Height > maxProofPixels {
		return nil, "", errProofDamaged
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", errProofDamaged
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if longest := max(w, h); longest > proofTargetSide {
		w = w * proofTargetSide / longest
		h = h * proofTargetSide / longest
	}
	// Fondo blanco (las capturas PNG con transparencia no quedan negras).
	dst := image.NewRGBA(image.Rect(0, 0, max(w, 1), max(h, 1)))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 88}); err != nil {
		return nil, "", errProofDamaged
	}
	return out.Bytes(), "image/jpeg", nil
}

func encryptProof(plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(proofKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, []byte("kidstore-proof-v1")), nil
}

func decryptProof(data []byte) ([]byte, error) {
	block, err := aes.NewCipher(proofKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(data) < gcm.NonceSize() {
		return nil, errors.New("comprobante dañado")
	}
	return gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], []byte("kidstore-proof-v1"))
}

func proofObjectKey(id uuid.UUID) string { return "comprobantes/" + id.String() }

// LoadProof devuelve el comprobante descifrado (para el panel admin).
func LoadProof(ctx context.Context, m db.ManualPaymentRequest) ([]byte, string, error) {
	if m.ProofKey == nil {
		return nil, "", errors.New("el comprobante ya se borró (se guardan 30 días)")
	}
	proofStoreMu.RLock()
	store := proofStore
	proofStoreMu.RUnlock()
	if store == nil || proofKey == nil {
		return nil, "", errors.New("almacenamiento de comprobantes no configurado")
	}
	enc, err := store.Get(ctx, *m.ProofKey)
	if err != nil {
		return nil, "", err
	}
	plain, err := decryptProof(enc)
	if err != nil {
		return nil, "", err
	}
	return plain, m.ProofContentType, nil
}

// ==================== ENLACE FIRMADO (botón "Ver comprobante" de Discord) ====================

const proofLinkTTL = 24 * time.Hour

func proofSignature(id uuid.UUID, exp int64) string {
	mac := hmac.New(sha256.New, proofSignKey)
	fmt.Fprintf(mac, "%s|%d", id, exp)
	return hex.EncodeToString(mac.Sum(nil))
}

// ProofViewURL — enlace para ver el comprobante sin iniciar sesión, válido 24 h
// (solo se manda por mensaje privado al admin).
func ProofViewURL(id uuid.UUID, now time.Time) string {
	exp := now.Add(proofLinkTTL).Unix()
	return fmt.Sprintf("%s/store/manual-payments/%s/proof?exp=%d&sig=%s", publicAPIBase, id, exp, proofSignature(id, exp))
}

func validProofLink(id uuid.UUID, expStr, sig string, now time.Time) bool {
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || now.Unix() > exp || proofSignKey == nil {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(proofSignature(id, exp)))
}

func serveProof(c *gin.Context, data []byte, contentType string) {
	c.Header("Cache-Control", "no-store, private")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'")
	c.Header("Content-Disposition", "inline")
	c.Data(http.StatusOK, contentType, data)
}

// HandlerViewProofSigned sirve el comprobante con un enlace firmado vigente.
func HandlerViewProofSigned(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil || !validProofLink(id, c.Query("exp"), c.Query("sig"), time.Now()) {
			c.String(http.StatusForbidden, "Enlace inválido o vencido. Abre el comprobante desde el panel admin.")
			return
		}
		m, err := db.GetManualPaymentRequest(database, id)
		if err != nil {
			c.String(http.StatusNotFound, "Solicitud no encontrada.")
			return
		}
		data, ct, err := LoadProof(c.Request.Context(), m)
		if err != nil {
			c.String(http.StatusGone, err.Error())
			return
		}
		serveProof(c, data, ct)
	}
}

// ==================== CLIENTE: subir comprobante y ver sus solicitudes ====================

var operationPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,40}$`)

// Aviso al admin (variable para pruebas).
var alertManualPayment = discordbot.AlertManualPayment

func HandlerCreateManualPayment(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerID, ok := requestCustomerID(c)
		if !ok {
			return
		}
		if !manualUploadsEnabled() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "code": "MANUAL_UPLOAD_UNAVAILABLE",
				"error": "la subida de comprobantes no está disponible ahora; envíalo por WhatsApp"})
			return
		}
		if c.PostForm("confirm") != "true" {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "confirma que ya realizaste el pago"})
			return
		}
		customKC, _ := strconv.Atoi(c.PostForm("custom_kc"))
		eur := currentConversionRates()["EUR"]
		quote, err := quoteManual(strings.TrimSpace(c.PostForm("package_id")), customKC, strings.TrimSpace(c.PostForm("method")), eur)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "paquete o método de pago inválido"})
			return
		}
		operation := strings.ReplaceAll(strings.TrimSpace(c.PostForm("operation_number")), " ", "")
		if operation != "" && !operationPattern.MatchString(operation) {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "el número de operación solo puede tener letras, números y guiones"})
			return
		}

		// Límite de solicitudes en revisión, antes de subir nada.
		if list, err := db.ListManualPaymentsByCustomer(database, customerID, db.MaxPendingManualPayments+5); err == nil {
			pending := 0
			for _, m := range list {
				if m.Status == "pending" {
					pending++
				}
			}
			if pending >= db.MaxPendingManualPayments {
				c.JSON(http.StatusConflict, gin.H{"success": false, "code": "TOO_MANY_PENDING",
					"error": fmt.Sprintf("ya tienes %d comprobantes en revisión; espera a que los revisemos", pending)})
				return
			}
		}

		file, err := c.FormFile("proof")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "sube la imagen o el PDF de tu comprobante"})
			return
		}
		f, err := file.Open()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "no se pudo leer el archivo"})
			return
		}
		raw, err := io.ReadAll(io.LimitReader(f, maxProofUpload+1))
		f.Close()
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "no se pudo leer el archivo"})
			return
		}
		proof, contentType, err := processProof(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "code": "PROOF_INVALID", "error": err.Error()})
			return
		}
		enc, err := encryptProof(proof)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudo guardar el comprobante"})
			return
		}

		id := uuid.New()
		key := proofObjectKey(id)
		proofStoreMu.RLock()
		store := proofStore
		proofStoreMu.RUnlock()
		putCtx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		err = store.Put(putCtx, key, enc)
		cancel()
		if err != nil {
			slog.Error("Comprobantes: no se pudo subir al almacenamiento", "error", err)
			c.JSON(http.StatusBadGateway, gin.H{"success": false, "error": "no se pudo guardar el comprobante, intenta de nuevo o envíalo por WhatsApp"})
			return
		}

		created, err := db.CreateManualPaymentRequest(database, db.ManualPaymentRequest{
			ID: id, CustomerID: customerID, PackageID: quote.PackageID, PackageName: quote.PackageName,
			KCAmount: quote.KC, Amount: quote.Amount, Currency: quote.Currency, AmountPEN: quote.AmountPEN,
			Method: c.PostForm("method"), OperationNumber: operation, ProofKey: &key, ProofContentType: contentType,
			Lang: requestLang(c),
		})
		if err != nil {
			delCtx, cancelDel := context.WithTimeout(context.Background(), 15*time.Second)
			store.Delete(delCtx, key) // no dejar archivos huérfanos
			cancelDel()
			if errors.Is(err, db.ErrTooManyPendingManual) {
				c.JSON(http.StatusConflict, gin.H{"success": false, "code": "TOO_MANY_PENDING",
					"error": "ya tienes comprobantes en revisión; espera a que los revisemos"})
				return
			}
			slog.Error("Comprobantes: no se pudo registrar la solicitud", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudo registrar tu comprobante"})
			return
		}

		db.AddAuditLog(database, &customerID, "MANUAL_PAYMENT_SUBMITTED",
			fmt.Sprintf("comprobante %s: %s %.2f %s → %d KC", id, created.Method, created.Amount, created.Currency, created.KCAmount), c.ClientIP())
		dupes, _ := db.OperationNumberUses(database, operation, id)
		customer, _ := db.GetCustomerByID(database, customerID)
		email := ""
		if customer.Email != nil {
			email = *customer.Email
		}
		go alertManualPayment(discordbot.ManualPaymentAlert{
			ID: id, Customer: customer.EpicUsername, Email: email, Package: created.PackageName, KC: created.KCAmount,
			Amount: formatMoney(created.Amount, created.Currency), Method: methodLabel(created.Method),
			Operation: operation, DuplicateOps: dupes, ViewURL: ProofViewURL(id, time.Now()),
		})
		c.JSON(http.StatusOK, gin.H{"success": true, "request": created})
	}
}

func HandlerListMyManualPayments(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		customerID, ok := requestCustomerID(c)
		if !ok {
			return
		}
		list, err := db.ListManualPaymentsByCustomer(database, customerID, 10)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "no se pudieron cargar tus comprobantes"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "requests": list, "enabled": manualUploadsEnabled(),
			"max_pending": db.MaxPendingManualPayments})
	}
}

// ==================== APROBAR / RECHAZAR (panel admin y Discord) ====================

var methodLabels = map[string]string{"yape": "Yape", "plin": "Plin", "bcp": "BCP", "interbank": "Interbank", "bbva": "BBVA", "bizum": "Bizum"}

func methodLabel(m string) string {
	if l, ok := methodLabels[m]; ok {
		return l
	}
	return m
}

func formatMoney(amount float64, currency string) string {
	if currency == "EUR" {
		return fmt.Sprintf("€%.2f", amount)
	}
	return fmt.Sprintf("S/ %.2f", amount)
}

// ApproveManualPayment acredita los KC (una sola vez) y avisa al cliente por la
// campana, por correo y en el canal de recargas de Discord.
func ApproveManualPayment(database *sql.DB, id uuid.UUID, reviewer string, afterReject bool) (db.ManualPaymentRequest, error) {
	m, rechargeID, err := db.ApproveManualPayment(database, id, reviewer, afterReject)
	if err != nil {
		return m, err
	}
	detail := fmt.Sprintf("comprobante %s aprobado por %s: +%d KC", id, reviewer, m.KCAmount)
	if m.RejectReason != nil && *m.RejectReason != "" {
		detail += " (tras haberse rechazado: " + *m.RejectReason + ")"
	}
	db.AddAuditLog(database, &m.CustomerID, "MANUAL_PAYMENT_APPROVED", detail, "admin")
	go discordbot.ManualReviewDone(id, approvedSummary(reviewer, m.KCAmount)) // copias del aviso en Discord
	db.AddNotification(database, m.CustomerID, db.NotifKCCredited, map[string]any{"amount_kc": m.KCAmount, "method": m.Method})
	if customer, err := db.GetCustomerByID(database, m.CustomerID); err == nil {
		discordbot.NotifyRecharge(customer, m.KCAmount, customer.KCBalance, "Comprobante ("+methodLabel(m.Method)+")")
		if customer.Email != nil && *customer.Email != "" {
			voucherURL := fmt.Sprintf("https://www.kidstoreperu.net/dashboard/comprobantes/recarga/%s", rechargeID)
			go SendPaymentApprovedEmail(smtpConfig, *customer.Email, m.PackageName, m.Amount, m.Currency, m.KCAmount,
				"Pago manual ("+methodLabel(m.Method)+")", voucherURL, m.Lang)
		}
	}
	return m, nil
}

// RejectManualPayment rechaza la solicitud y le explica el motivo al cliente.
func RejectManualPayment(database *sql.DB, id uuid.UUID, reason, reviewer string) (db.ManualPaymentRequest, error) {
	reason = cleanText(reason, 300)
	if reason == "" {
		reason = "No pudimos verificar tu pago."
	}
	m, err := db.RejectManualPayment(database, id, reason, reviewer)
	if err != nil {
		return m, err
	}
	db.AddAuditLog(database, &m.CustomerID, "MANUAL_PAYMENT_REJECTED",
		fmt.Sprintf("comprobante %s rechazado por %s: %s", id, reviewer, reason), "admin")
	go discordbot.ManualReviewDone(id, rejectedSummary(reviewer, reason))
	db.AddNotification(database, m.CustomerID, db.NotifManualRejected, map[string]any{
		"amount_kc": m.KCAmount, "method": m.Method, "reason": reason,
		"amount": formatMoney(m.Amount, m.Currency),
	})
	if customer, err := db.GetCustomerByID(database, m.CustomerID); err == nil && customer.Email != nil && *customer.Email != "" {
		go SendManualPaymentRejectedEmail(smtpConfig, *customer.Email, m, reason)
	}
	return m, nil
}

// Texto del resultado en el aviso de Discord (igual en todas sus copias).
func approvedSummary(reviewer string, kc int) string {
	return fmt.Sprintf("✅ Aprobado por %s — +%d KC acreditados", reviewer, kc)
}

func rejectedSummary(reviewer, reason string) string {
	return fmt.Sprintf("❌ Rechazado por %s — %s", reviewer, reason)
}

// ManualPaymentDiscordActions — lo que hacen los botones del aviso de Discord.
func ManualPaymentDiscordActions(database *sql.DB) (approve, reject func(uuid.UUID, string, string) (string, error)) {
	approve = func(id uuid.UUID, _ string, reviewer string) (string, error) {
		m, err := ApproveManualPayment(database, id, reviewer, false)
		if err != nil {
			return "", err
		}
		return approvedSummary(reviewer, m.KCAmount), nil
	}
	reject = func(id uuid.UUID, reason, reviewer string) (string, error) {
		m, err := RejectManualPayment(database, id, reason, reviewer)
		if err != nil {
			return "", err
		}
		saved := "" // el motivo tal como quedó guardado (limpio, o el de por defecto)
		if m.RejectReason != nil {
			saved = *m.RejectReason
		}
		return rejectedSummary(reviewer, saved), nil
	}
	return approve, reject
}

// ManualSupportCode es el código corto con el que el cliente y soporte
// identifican la solicitud (es el mismo que muestra el panel admin).
func ManualSupportCode(id uuid.UUID) string {
	return strings.ToUpper(id.String()[:8])
}

// manualSupportWhatsApp abre WhatsApp con el mensaje ya escrito para que el
// cliente reclame un rechazo indicando el código de la solicitud.
func manualSupportWhatsApp(id uuid.UUID, es bool) string {
	text := fmt.Sprintf("Hola, mi comprobante de pago %s fue rechazado y creo que es un error.", ManualSupportCode(id))
	if !es {
		text = fmt.Sprintf("Hi, my payment proof %s was rejected and I think it's a mistake.", ManualSupportCode(id))
	}
	return "https://wa.me/51983454837?text=" + strings.ReplaceAll(url.QueryEscape(text), "+", "%20")
}

// ==================== BORRADO AUTOMÁTICO (1 mes) ====================

func purgeExpiredProofs(database *sql.DB, now time.Time) int {
	expired, err := db.ExpiredProofs(database, now)
	if err != nil || len(expired) == 0 {
		return 0
	}
	proofStoreMu.RLock()
	store := proofStore
	proofStoreMu.RUnlock()
	if store == nil {
		return 0
	}
	deleted := 0
	for id, key := range expired {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := store.Delete(ctx, key)
		cancel()
		if err != nil {
			slog.Warn("Comprobantes: no se pudo borrar un comprobante vencido", "id", id, "error", err)
			continue
		}
		if err := db.MarkProofDeleted(database, id); err == nil {
			deleted++
		}
	}
	if deleted > 0 {
		slog.Info("Comprobantes: borrados por antigüedad (más de 30 días)", "cantidad", deleted)
	}
	return deleted
}

// StartProofRetention borra cada hora los comprobantes con más de 30 días.
func StartProofRetention(database *sql.DB) {
	go func() {
		time.Sleep(5 * time.Minute)
		for {
			safe.Run("proofs.retention", func() { purgeExpiredProofs(database, time.Now()) })
			time.Sleep(time.Hour)
		}
	}()
}
