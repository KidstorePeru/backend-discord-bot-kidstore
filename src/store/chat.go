package store

import (
	"KidStoreStore/src/autobuyer"
	"net/http"

	"github.com/gin-gonic/gin"
)

// HandlerChatStart proxies POST /store/chat/start → Autobuyer /api/v1/chat/start
func HandlerChatStart() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !autobuyer.IsConfigured() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "chatbot not available"})
			return
		}
		lang := c.Query("lang")
		data, err := autobuyer.ChatStart(lang)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "chatbot unreachable"})
			return
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", data)
	}
}

// HandlerChatMessage proxies POST /store/chat/message → Autobuyer /api/v1/chat/message
func HandlerChatMessage() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !autobuyer.IsConfigured() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "chatbot not available"})
			return
		}
		var body struct {
			SessionID string `json:"session_id"`
			Text      string `json:"text"`
		}
		if err := c.ShouldBindJSON(&body); err != nil || body.SessionID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		if err := autobuyer.ChatMessage(body.SessionID, body.Text); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "chatbot unreachable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// HandlerChatPoll proxies GET /store/chat/poll/:sid → Autobuyer /api/v1/chat/poll/:sid
// Transparently forwards the autobuyer's status code (404 = session expired).
func HandlerChatPoll() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !autobuyer.IsConfigured() {
			c.JSON(http.StatusOK, gin.H{"messages": []interface{}{}})
			return
		}
		sid := c.Param("sid")
		statusCode, data, err := autobuyer.ChatPoll(sid)
		if err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "chatbot unreachable"})
			return
		}
		c.Data(statusCode, "application/json; charset=utf-8", data)
	}
}
