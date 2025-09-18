package transcription

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

const (
	assemblyAIStreamURL = "wss://streaming.assemblyai.com/v3/ws"
)

// AssemblyAI Streaming Message Types
type SessionBegin struct {
	Type      string  `json:"type"`
	SessionID string  `json:"session_id"`
	ExpiresAt float64 `json:"expires_at"`
}

type TurnMessage struct {
	Type                string  `json:"type"`
	TurnOrder           int     `json:"turn_order"`
	TurnIsFormatted     bool    `json:"turn_is_formatted"`
	EndOfTurn           bool    `json:"end_of_turn"`
	Transcript          string  `json:"transcript"`
	EndOfTurnConfidence float64 `json:"end_of_turn_confidence"`
}

type StreamingConfig struct {
	SampleRate  int  `json:"sample_rate"`
	FormatTurns bool `json:"format_turns"`
}

type AudioMessage struct {
	AudioData string `json:"audio_data"`
}

type Client struct {
	wsConn              *websocket.Conn
	wsMutex             sync.Mutex
	transcriptCallback  func(string, bool) // transcript, isComplete
	connectionCallback  func(bool)         // connected
	terminationCallback func()             // called when session terminates
}

func NewClient(transcriptCallback func(string, bool), connectionCallback func(bool)) *Client {
	return &Client{
		transcriptCallback: transcriptCallback,
		connectionCallback: connectionCallback,
	}
}

func (c *Client) SetTerminationCallback(callback func()) {
	c.terminationCallback = callback
}

func (c *Client) Connect(apiKey string) error {
	// Create WebSocket URL with query parameters (matching JS example)
	u, err := url.Parse(assemblyAIStreamURL)
	if err != nil {
		return fmt.Errorf("error parsing WebSocket URL: %v", err)
	}

	// Add required query parameters (matching Python example exactly)
	query := u.Query()
	query.Set("sample_rate", "16000") // Use underscore format like Python
	query.Set("format_turns", "true") // Use underscore format like Python
	u.RawQuery = query.Encode()

	// Create headers with authorization (just API key, no "Bearer")
	headers := make(map[string][]string)
	headers["Authorization"] = []string{apiKey}

	// Establish WebSocket connection
	c.wsMutex.Lock()
	c.wsConn, _, err = websocket.DefaultDialer.Dial(u.String(), headers)
	c.wsMutex.Unlock()

	if err != nil {
		return fmt.Errorf("error connecting to AssemblyAI: %v", err)
	}

	// Start listening for responses in a goroutine
	go c.handleResponses()

	// Notify connection callback
	if c.connectionCallback != nil {
		c.connectionCallback(true)
	}

	return nil
}

func (c *Client) SendAudio(audioData []byte) error {
	c.wsMutex.Lock()
	defer c.wsMutex.Unlock()

	if c.wsConn == nil {
		return fmt.Errorf("WebSocket connection not established")
	}

	// Send raw audio bytes directly (not JSON, not base64)
	err := c.wsConn.WriteMessage(websocket.BinaryMessage, audioData)

	// If we get a close error, the connection is no longer usable
	if err != nil && (websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) ||
		strings.Contains(err.Error(), "websocket: close sent") ||
		strings.Contains(err.Error(), "use of closed network connection")) {
		// Clean up the connection since it's no longer usable
		c.wsConn = nil
	}

	return err
}

func (c *Client) Terminate() error {
	c.wsMutex.Lock()
	defer c.wsMutex.Unlock()

	if c.wsConn != nil {
		// Send termination message to AssemblyAI (like Python example)
		terminateMessage := map[string]string{"type": "Terminate"}
		if jsonData, err := json.Marshal(terminateMessage); err == nil {
			c.wsConn.WriteMessage(websocket.TextMessage, jsonData)
		}
	}
	return nil
}

func (c *Client) Close() {
	c.wsMutex.Lock()
	defer c.wsMutex.Unlock()

	if c.wsConn != nil {
		// Send close frame to AssemblyAI before closing
		c.wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		c.wsConn.Close()
		c.wsConn = nil
	}

	// Notify connection callback
	if c.connectionCallback != nil {
		c.connectionCallback(false)
	}
}

func (c *Client) IsConnected() bool {
	c.wsMutex.Lock()
	defer c.wsMutex.Unlock()

	if c.wsConn == nil {
		return false
	}

	// Test if connection is still alive with a simple ping
	// If this fails, the connection was closed by the server
	err := c.wsConn.WriteMessage(websocket.PingMessage, []byte{})
	if err != nil {
		// Connection is dead, clean it up
		c.wsConn.Close()
		c.wsConn = nil
		return false
	}
	return true
}

func (c *Client) handleResponses() {
	for {
		c.wsMutex.Lock()
		conn := c.wsConn
		c.wsMutex.Unlock()

		if conn == nil {
			break
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return
			}
			// Suppress network connection closed errors during shutdown
			if strings.Contains(err.Error(), "use of closed network connection") ||
				strings.Contains(err.Error(), "connection reset by peer") {
				return
			}
			return
		}

		// Parse the message
		var baseMsg map[string]any
		if err := json.Unmarshal(message, &baseMsg); err != nil {
			log.Printf("Error parsing message: %v", err)
			continue
		}

		// Handle different message types (matching JS example)
		if msgType, ok := baseMsg["type"].(string); ok {
			switch msgType {
			case "Begin":
				if sessionId, ok := baseMsg["id"].(string); ok {
					// Session started
					_ = sessionId
				}

			case "Turn":
				if transcript, ok := baseMsg["transcript"].(string); ok && transcript != "" {
					// Check if this is a formatted turn (final) or partial
					isComplete := false
					if formatted, ok := baseMsg["turn_is_formatted"].(bool); ok && formatted {
						isComplete = true
					}

					// Send transcript to callback
					if c.transcriptCallback != nil {
						c.transcriptCallback(transcript, isComplete)
					}
				}

			case "Termination":
				if c.terminationCallback != nil {
					c.terminationCallback()
				}
			}
		}
	}
}
