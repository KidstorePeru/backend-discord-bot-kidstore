package autobuyer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

var (
	baseURL string
	apiKey  string
	client      = &http.Client{Timeout: 30 * time.Second}
	fastClient  = &http.Client{Timeout: 3 * time.Second}   // for status polls
)

func Init(url, key string) {
	baseURL = url
	apiKey = key
}

func IsConfigured() bool {
	return baseURL != ""
}

func doRequestWithClient(method, path string, body interface{}, httpClient *http.Client) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewBuffer(b)
	}

	req, err := http.NewRequest(method, baseURL+"/api/v1"+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("autobuyer unreachable: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("autobuyer error %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

func doRequest(method, path string, body interface{}) ([]byte, error) {
	return doRequestWithClient(method, path, body, client)
}

// ==================== API FUNCTIONS ====================

func Ping() bool {
	_, err := doRequest("GET", "/ping", nil)
	return err == nil
}

// SetDiscordLang notifies the autobuyer to update a Discord user's language cache.
func SetDiscordLang(discordID, lang string) {
	go func() {
		_, _ = doRequestWithClient("POST", "/bots/discord-lang", map[string]string{
			"discord_id": discordID,
			"lang":       lang,
		}, fastClient)
	}()
}

type TaskResult struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	AuthURL     string `json:"auth_url,omitempty"`
	InputNeeded string `json:"input_needed,omitempty"`
	Error       string `json:"error,omitempty"`
	Result      map[string]interface{} `json:"result,omitempty"`
	Messages []struct {
		Text      string  `json:"text"`
		Timestamp float64 `json:"timestamp"`
	} `json:"messages,omitempty"`
}

type CreateSaleRequest struct {
	ProductName       string `json:"product_name"`
	PaymentType       string `json:"payment_type"` // "razer" or "card"
	Buyer             string `json:"buyer,omitempty"`
	OrderID           string `json:"order_id,omitempty"`
	MessageWebhookURL string `json:"message_webhook_url,omitempty"`
	StatusWebhookURL  string `json:"status_webhook_url,omitempty"`
}

func CreateSale(req CreateSaleRequest) (*TaskResult, error) {
	data, err := doRequest("POST", "/tasks/sale", req)
	if err != nil {
		return nil, err
	}
	var result TaskResult
	json.Unmarshal(data, &result)
	return &result, nil
}

func CreateRegion(buyer string, statusWebhookURL, messageWebhookURL string) (*TaskResult, error) {
	body := map[string]string{}
	if buyer != "" {
		body["buyer"] = buyer
	}
	if statusWebhookURL != "" {
		body["status_webhook_url"] = statusWebhookURL
	}
	if messageWebhookURL != "" {
		body["message_webhook_url"] = messageWebhookURL
	}
	data, err := doRequest("POST", "/tasks/region", body)
	if err != nil {
		return nil, err
	}
	var result TaskResult
	json.Unmarshal(data, &result)
	return &result, nil
}

func CreateRegionCheck(buyer string, statusWebhookURL, messageWebhookURL string) (*TaskResult, error) {
	body := map[string]string{}
	if buyer != "" {
		body["buyer"] = buyer
	}
	if statusWebhookURL != "" {
		body["status_webhook_url"] = statusWebhookURL
	}
	if messageWebhookURL != "" {
		body["message_webhook_url"] = messageWebhookURL
	}
	data, err := doRequest("POST", "/tasks/region-check", body)
	if err != nil {
		return nil, err
	}
	var result TaskResult
	json.Unmarshal(data, &result)
	return &result, nil
}

func GetTask(taskID string) (*TaskResult, error) {
	data, err := doRequestWithClient("GET", "/tasks/"+taskID, nil, fastClient)
	if err != nil {
		return nil, err
	}
	var result TaskResult
	json.Unmarshal(data, &result)
	return &result, nil
}

func SubmitInput(taskID, value string) error {
	_, err := doRequest("POST", "/tasks/"+taskID+"/input", map[string]string{"value": value})
	return err
}

// RegisterCode registers an activation code in the autobuyer's chatbot system.
// Called after a payment is approved so the chatbot can validate !activar commands.
func RegisterCode(code, productName, paymentType, buyer, orderID string) error {
	body := map[string]string{
		"code":         code,
		"product_name": productName,
		"payment_type": paymentType,
	}
	if buyer != "" {
		body["buyer"] = buyer
	}
	if orderID != "" {
		body["order_id"] = orderID
	}
	_, err := doRequest("POST", "/codes", body)
	return err
}
