package fortnite

import (
	"KidStoreStore/src/db"
	"KidStoreStore/src/types"
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ==================== CONSTANTS ====================

var epicClient string
var epicSecret string
var encryptionKey string

func Init(client, secret, encKey string) {
	epicClient = client
	epicSecret = secret
	encryptionKey = encKey
}

func authHeader() string {
	return "basic " + base64.StdEncoding.EncodeToString([]byte(epicClient+":"+epicSecret))
}

// ==================== CONNECT ACCOUNT (Step 1) ====================
// Inicia el flujo OAuth de Epic Games — devuelve device_code y URL de login

func HandlerConnectBotAccount(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		client := &http.Client{Timeout: 10 * time.Second}

		// 1. Obtener client credentials token
		reqToken, _ := http.NewRequest("POST",
			"https://account-public-service-prod.ol.epicgames.com/account/api/oauth/token",
			strings.NewReader("grant_type=client_credentials"))
		reqToken.Header.Set("Authorization", authHeader())
		reqToken.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		respToken, err := client.Do(reqToken)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "No se pudo obtener el token de cliente", "details": err.Error()})
			return
		}
		defer respToken.Body.Close()

		var tokenResult types.EpicAccessTokenResult
		if err := json.NewDecoder(respToken.Body).Decode(&tokenResult); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Respuesta inválida del token de cliente"})
			return
		}

		// 2. Solicitar device authorization
		reqDevice, _ := http.NewRequest("POST",
			"https://account-public-service-prod.ol.epicgames.com/account/api/oauth/deviceAuthorization", nil)
		reqDevice.Header.Set("Authorization", "bearer "+tokenResult.AccessToken)
		reqDevice.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		respDevice, err := client.Do(reqDevice)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "No se pudo iniciar la autorización del dispositivo"})
			return
		}
		defer respDevice.Body.Close()

		// Leer body crudo para poder loguearlo y detectar errores de Epic
		deviceBodyBytes, err := io.ReadAll(respDevice.Body)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Error leyendo respuesta de Epic"})
			return
		}
		slog.Info("Epic deviceAuthorization", "status", respDevice.StatusCode, "body", string(deviceBodyBytes))

		// Si Epic devolvió error (no 200), retornarlo directamente
		if respDevice.StatusCode != 200 {
			c.JSON(http.StatusInternalServerError, gin.H{
				"success": false,
				"error":   "Epic Games rechazó la solicitud",
				"details": string(deviceBodyBytes),
				"status":  respDevice.StatusCode,
			})
			return
		}

		var deviceResult types.EpicDeviceAuthResponse
		if err := json.Unmarshal(deviceBodyBytes, &deviceResult); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Respuesta inválida de autorización del dispositivo"})
			return
		}

		// Si user_code viene vacío aunque el status fue 200, algo raro pasó
		if deviceResult.UserCode == "" {
			c.JSON(http.StatusInternalServerError, gin.H{
				"success": false,
				"error":   "Epic no devolvió el user_code",
				"details": string(deviceBodyBytes),
			})
			return
		}

		// 3. Armar URL de activación
		// Si verification_uri_complete viene vacío, lo construimos manualmente
		activateURL := deviceResult.VerificationUriComplete
		if activateURL == "" {
			activateURL = fmt.Sprintf("https://www.epicgames.com/activate?userCode=%s", deviceResult.UserCode)
		}

		// Forzar logout de Epic primero para limpiar sesión anterior,
		// luego redirigir a la página de activación
		finalRedirect := url.QueryEscape(activateURL)
		loginURL := fmt.Sprintf("https://www.epicgames.com/id/logout?lang=en-US&redirectUrl=%s", finalRedirect)

		c.JSON(http.StatusOK, gin.H{
			"success":     true,
			"login_url":   loginURL,
			"epic_url":    activateURL,
			"user_code":   deviceResult.UserCode,
			"device_code": deviceResult.DeviceCode,
			"expires_in":  deviceResult.ExpiresIn,
		})
	}
}

// ==================== FINISH CONNECT (Step 2) ====================
// Completa el flujo OAuth y guarda la cuenta bot en la DB

func HandlerFinishConnectBotAccount(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			DeviceCode string `json:"device_code" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		client := &http.Client{Timeout: 15 * time.Second}

		// 1. Canjear device_code por access token
		reqToken, _ := http.NewRequest("POST",
			"https://account-public-service-prod.ol.epicgames.com/account/api/oauth/token",
			strings.NewReader("grant_type=device_code&device_code="+req.DeviceCode))
		reqToken.Header.Set("Authorization", authHeader())
		reqToken.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		respToken, err := client.Do(reqToken)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Error al autorizar con Epic Games"})
			return
		}
		defer respToken.Body.Close()

		if respToken.StatusCode != 200 {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "Device code inválido o expirado. Vuelve a intentarlo."})
			return
		}

		var loginResult types.EpicLoginResult
		if err := json.NewDecoder(respToken.Body).Decode(&loginResult); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Respuesta inválida de Epic Games"})
			return
		}

		accountID, err := uuid.Parse(loginResult.AccountId)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "ID de cuenta Epic inválido"})
			return
		}

		// 2. Guardar cuenta bot en DB
		account := types.GameAccount{
			ID:                  accountID,
			DisplayName:         loginResult.DisplayName,
			RemainingGifts:      5,
			VBucks:              0,
			AccessToken:         loginResult.AccessToken,
			AccessTokenExpDate:  time.Now().Add(time.Duration(loginResult.ExpiresIn) * time.Second),
			RefreshToken:        loginResult.RefreshToken,
			RefreshTokenExpDate: time.Now().Add(time.Duration(loginResult.RefreshExpiresIn) * time.Second),
			IsActive:            true,
			CreatedAt:           time.Now(),
			UpdatedAt:           time.Now(),
		}
		if err := db.UpsertGameAccount(database, account, encryptionKey); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "No se pudo guardar la cuenta bot", "details": err.Error()})
			return
		}

		// 3. Obtener device secrets para re-autenticación permanente (opcional pero importante)
		reqSecrets, _ := http.NewRequest("POST",
			fmt.Sprintf("https://account-public-service-prod.ol.epicgames.com/account/api/public/account/%s/deviceAuth", loginResult.AccountId),
			nil)
		reqSecrets.Header.Set("Authorization", "Bearer "+loginResult.AccessToken)

		respSecrets, err := client.Do(reqSecrets)
		if err == nil && respSecrets.StatusCode == 200 {
			defer respSecrets.Body.Close()
			var secrets types.EpicDeviceSecretsResult
			if err := json.NewDecoder(respSecrets.Body).Decode(&secrets); err == nil {
				db.UpsertGameAccountSecrets(database, types.GameAccountSecrets{
					ID:        uuid.New(),
					AccountID: accountID,
					DeviceID:  secrets.DeviceId,
					Secret:    secrets.Secret,
					CreatedAt: time.Now(),
				}, encryptionKey)
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"success":      true,
			"message":      "Cuenta bot vinculada correctamente",
			"display_name": loginResult.DisplayName,
			"account_id":   loginResult.AccountId,
		})
	}
}

// ==================== DISCONNECT BOT ACCOUNT ====================

func HandlerDisconnectBotAccount(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			AccountID string `json:"account_id" binding:"required"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}

		id, err := uuid.Parse(req.AccountID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "ID inválido"})
			return
		}

		if err := db.DeleteGameAccount(database, id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "No se pudo desconectar la cuenta"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true, "message": "Cuenta bot desconectada"})
	}
}

// ==================== GET ALL BOT ACCOUNTS ====================

func HandlerGetBotAccounts(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		accounts, err := db.GetAllGameAccounts(database, encryptionKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Error obteniendo cuentas bot"})
			return
		}
		if accounts == nil {
			accounts = []types.GameAccount{}
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "accounts": accounts})
	}
}

// ==================== UPDATE REMAINING GIFTS ====================

func HandlerUpdateRemainingGifts(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			AccountID      string `json:"account_id" binding:"required"`
			RemainingGifts int    `json:"remaining_gifts"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}
		id, _ := uuid.Parse(req.AccountID)
		if err := db.UpdateRemainingGifts(database, id, req.RemainingGifts); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Error actualizando gifts restantes"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

// HandlerUpdateBotVbucks actualiza los V-Bucks de una cuenta bot manualmente
func HandlerUpdateBotVbucks(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			AccountID string `json:"account_id" binding:"required"`
			Vbucks    int    `json:"vbucks"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": err.Error()})
			return
		}
		id, err := uuid.Parse(req.AccountID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "error": "ID inválido"})
			return
		}
		if err := db.UpdateBotVbucks(database, id, req.Vbucks); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"success": false, "error": "Error actualizando pavos"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "message": "Pavos actualizados"})
	}
}

// ==================== TOKEN REFRESH ====================

func refreshAccessToken(database *sql.DB, account types.GameAccount) (types.GameAccount, error) {
	client := &http.Client{Timeout: 10 * time.Second}

	reqToken, _ := http.NewRequest("POST",
		"https://account-public-service-prod.ol.epicgames.com/account/api/oauth/token",
		strings.NewReader("grant_type=refresh_token&refresh_token="+url.QueryEscape(account.RefreshToken)))
	reqToken.Header.Set("Authorization", authHeader())
	reqToken.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(reqToken)
	if err != nil {
		return account, fmt.Errorf("error refreshing token: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		// Intentar re-auth con device secrets
		return refreshWithDeviceSecrets(database, account)
	}

	var result types.EpicLoginResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return account, fmt.Errorf("error decoding refresh response: %w", err)
	}

	account.AccessToken = result.AccessToken
	account.AccessTokenExpDate = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	account.RefreshToken = result.RefreshToken
	account.RefreshTokenExpDate = time.Now().Add(time.Duration(result.RefreshExpiresIn) * time.Second)
	account.UpdatedAt = time.Now()

	db.UpsertGameAccount(database, account, encryptionKey)
	return account, nil
}

func refreshWithDeviceSecrets(database *sql.DB, account types.GameAccount) (types.GameAccount, error) {
	secrets, err := db.GetGameAccountSecrets(database, account.ID, encryptionKey)
	if err != nil {
		return account, fmt.Errorf("no device secrets found for account %s: %w", account.ID, err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	body := fmt.Sprintf("grant_type=device_auth&device_id=%s&secret=%s&account_id=%s",
		secrets.DeviceID, secrets.Secret, account.ID.String())

	reqToken, _ := http.NewRequest("POST",
		"https://account-public-service-prod.ol.epicgames.com/account/api/oauth/token",
		strings.NewReader(body))
	reqToken.Header.Set("Authorization", authHeader())
	reqToken.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(reqToken)
	if err != nil {
		return account, fmt.Errorf("device auth failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		// Cuenta inservible — desactivar
		db.DeactivateGameAccount(database, account.ID)
		return account, fmt.Errorf("device auth failed with status %d — account deactivated", resp.StatusCode)
	}

	var result types.EpicLoginResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return account, fmt.Errorf("error decoding device auth response: %w", err)
	}

	account.AccessToken = result.AccessToken
	account.AccessTokenExpDate = time.Now().Add(time.Duration(result.ExpiresIn) * time.Second)
	account.RefreshToken = result.RefreshToken
	account.RefreshTokenExpDate = time.Now().Add(time.Duration(result.RefreshExpiresIn) * time.Second)
	account.UpdatedAt = time.Now()

	db.UpsertGameAccount(database, account, encryptionKey)
	return account, nil
}

// executeWithRefresh ejecuta una request HTTP con la cuenta bot,
// auto-refrescando el token si es necesario.
func executeWithRefresh(database *sql.DB, account types.GameAccount, req *http.Request) (*http.Response, types.GameAccount, error) {
	// Refrescar token si está próximo a expirar (menos de 5 min)
	if time.Until(account.AccessTokenExpDate) < 5*time.Minute {
		var err error
		account, err = refreshAccessToken(database, account)
		if err != nil {
			return nil, account, fmt.Errorf("token refresh failed: %w", err)
		}
	}

	req.Header.Set("Authorization", "Bearer "+account.AccessToken)
	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return nil, account, err
	}

	// Body buffering para poder re-leerlo
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	// Si 401/403 → intentar refresh y reintentar
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		account, err = refreshAccessToken(database, account)
		if err != nil {
			return nil, account, fmt.Errorf("token refresh after 401 failed: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+account.AccessToken)
		resp2, err := client.Do(req)
		if err != nil {
			return nil, account, err
		}
		body2, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		resp2.Body = io.NopCloser(bytes.NewBuffer(body2))
		return resp2, account, nil
	}

	return resp, account, nil
}

// ==================== SEARCH EPIC ACCOUNT ====================
// Busca el accountId de un usuario Epic por su displayName y verifica amistad

func GetReceiverAccountID(database *sql.DB, account types.GameAccount, displayName string) (string, error) {
	req, _ := http.NewRequest("GET",
		fmt.Sprintf("https://account-public-service-prod.ol.epicgames.com/account/api/public/account/displayName/%s", displayName),
		nil)

	resp, _, err := executeWithRefresh(database, account, req)
	if err != nil {
		return "", fmt.Errorf("error buscando usuario Epic: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("usuario Epic '%s' no encontrado", displayName)
	}

	var result types.EpicPublicAccount
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("error decodificando respuesta de búsqueda: %w", err)
	}

	return result.AccountId, nil
}

// CheckFriendship verifica si la cuenta bot ya es amiga del cliente y hace cuánto
func CheckFriendship(database *sql.DB, account types.GameAccount, receiverAccountID string) (bool, time.Time, error) {
	botIDClean := strings.ReplaceAll(account.ID.String(), "-", "")
	req, _ := http.NewRequest("GET",
		fmt.Sprintf("https://friends-public-service-prod.ol.epicgames.com/friends/api/v1/%s/friends/%s",
			botIDClean, strings.ReplaceAll(receiverAccountID, "-", "")),
		nil)

	resp, _, err := executeWithRefresh(database, account, req)
	if err != nil {
		return false, time.Time{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return false, time.Time{}, nil
	}
	if resp.StatusCode != 200 {
		return false, time.Time{}, fmt.Errorf("error verificando amistad: status %d", resp.StatusCode)
	}

	var friend types.EpicFriendEntry
	if err := json.NewDecoder(resp.Body).Decode(&friend); err != nil {
		return false, time.Time{}, err
	}

	createdAt, err := time.Parse(time.RFC3339, friend.Created)
	if err != nil {
		return true, time.Now().Add(-49 * time.Hour), nil // asumir 48h+ si no se puede parsear
	}

	return true, createdAt, nil
}

// ==================== SEND GIFT ====================

func SendGift(database *sql.DB, account types.GameAccount, receiverAccountID, offerID string, priceVBucks int, itemName, message string) error {
	botIDClean := strings.ReplaceAll(account.ID.String(), "-", "")
	receiverClean := strings.ReplaceAll(receiverAccountID, "-", "")

	payload := map[string]interface{}{
		"offerId":            offerID,
		"currency":           "MtxCurrency",
		"currencySubType":    "",
		"expectedTotalPrice": priceVBucks,
		"gameContext":        "Frontend.CatabaScreen",
		"receiverAccountIds": []string{receiverClean},
		"giftWrapTemplateId": "",
		"personalMessage":    message,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("error marshaling gift payload: %w", err)
	}

	req, _ := http.NewRequest("POST",
		fmt.Sprintf("https://fngw-mcp-gc-livefn.ol.epicgames.com/fortnite/api/game/v2/profile/%s/client/GiftCatalogEntry?profileId=common_core", botIDClean),
		bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")

	resp, updatedAccount, err := executeWithRefresh(database, account, req)
	if err != nil {
		return fmt.Errorf("error enviando gift request: %w", err)
	}
	defer resp.Body.Close()
	_ = updatedAccount

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode > 204 {
		var errResp map[string]interface{}
		json.Unmarshal(respBody, &errResp)

		if errCode, ok := errResp["errorCode"].(string); ok {
			switch errCode {
			case "errors.com.epicgames.modules.gamesubcatalog.purchase_not_allowed":
				db.UpdateRemainingGifts(database, account.ID, 0)
				return fmt.Errorf("la cuenta bot no tiene slots de regalo disponibles")
			case "errors.com.epicgames.friends.friendship_not_found":
				return fmt.Errorf("el cliente no tiene agregado al bot como amigo")
			case "errors.com.epicgames.modules.gamesubcatalog.receiver_will_own_more_than_one":
				return fmt.Errorf("el cliente ya tiene este item")
			default:
				return fmt.Errorf("error de Epic Games: %s", errCode)
			}
		}
		return fmt.Errorf("error enviando gift, status: %d", resp.StatusCode)
	}

	return nil
}

// ==================== GOROUTINE: Auto-accept friend requests ====================

func StartFriendRequestAcceptor(database *sql.DB, intervalSeconds int) {
	go func() {
		ticker := time.NewTicker(time.Duration(intervalSeconds) * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			acceptPendingFriendRequests(database)
		}
	}()
	slog.Info("Bots: auto-aceptar solicitudes de amistad", "intervalSeconds", intervalSeconds)
}

func acceptPendingFriendRequests(database *sql.DB) {
	accounts, err := db.GetActiveGameAccounts(database, encryptionKey)
	if err != nil || len(accounts) == 0 {
		return
	}

	for _, account := range accounts {
		botIDClean := strings.ReplaceAll(account.ID.String(), "-", "")
		req, _ := http.NewRequest("GET",
			fmt.Sprintf("https://friends-public-service-prod.ol.epicgames.com/friends/api/v1/%s/incoming", botIDClean),
			nil)

		resp, _, err := executeWithRefresh(database, account, req)
		if err != nil {
			continue
		}

		var incoming []types.EpicFriendEntry
		json.NewDecoder(resp.Body).Decode(&incoming)
		resp.Body.Close()

		for _, friend := range incoming {
			acceptReq, _ := http.NewRequest("POST",
				fmt.Sprintf("https://friends-public-service-prod.ol.epicgames.com/friends/api/v1/%s/friends/%s",
					botIDClean, friend.AccountId),
				nil)
			acceptResp, _, err := executeWithRefresh(database, account, acceptReq)
			if err == nil {
				acceptResp.Body.Close()
				slog.Info("Bots: solicitud aceptada", "friendAccountId", friend.AccountId, "bot", account.DisplayName)
			}
		}
	}
}

// HandlerVerifyBotTokens verifica manualmente los tokens de todos los bots
// y marca como inactivos los que ya no son válidos.
// Útil para forzar la verificación desde el panel admin sin esperar el health check.
func HandlerVerifyBotTokens(database *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		go checkAllTokens(database)
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": "Verificación iniciada. Refresca la lista en unos segundos.",
		})
	}
}
