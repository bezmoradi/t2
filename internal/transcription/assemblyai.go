package transcription

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

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
	transcriptCallback  func(string, bool, bool, float64) // transcript, isComplete, endOfTurn, confidence
	connectionCallback  func(bool)                        // connected
	terminationCallback func()                            // called when session terminates
	chunkCount          int                               // for audio logging
	lastChunkSize       int                               // for audio logging
	connectionHealth    int                               // tracks connection quality (0-100)
	lastConnectionTime  time.Time                         // when connection was established
	sessionCount        int                               // number of sessions since connection
	failedSessions      int                               // consecutive failed sessions
}

func NewClient(transcriptCallback func(string, bool, bool, float64), connectionCallback func(bool)) *Client {
	return &Client{
		transcriptCallback: transcriptCallback,
		connectionCallback: connectionCallback,
		connectionHealth:   100, // Start with perfect health
	}
}

func (c *Client) SetTerminationCallback(callback func()) {
	c.terminationCallback = callback
}

func (c *Client) Connect(apiKey string) error {
	log.Printf("[WS] Starting connection to AssemblyAI at %s", time.Now().Format("15:04:05.000"))

	// Create WebSocket URL with query parameters (matching JS example)
	u, err := url.Parse(assemblyAIStreamURL)
	if err != nil {
		log.Printf("[WS] ERROR: Failed to parse WebSocket URL: %v", err)
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

	log.Printf("[WS] Attempting connection to: %s", u.String())

	// Establish WebSocket connection
	c.wsMutex.Lock()
	c.wsConn, _, err = websocket.DefaultDialer.Dial(u.String(), headers)
	c.wsMutex.Unlock()

	if err != nil {
		log.Printf("[WS] ERROR: Connection failed: %v", err)
		return fmt.Errorf("error connecting to AssemblyAI: %v", err)
	}

	log.Printf("[WS] SUCCESS: Connected successfully at %s", time.Now().Format("15:04:05.000"))

	// Update connection health tracking
	c.lastConnectionTime = time.Now()
	c.connectionHealth = 100
	c.sessionCount = 0
	c.failedSessions = 0

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
		log.Printf("[AUDIO] ERROR: WebSocket connection not established")
		return fmt.Errorf("WebSocket connection not established")
	}

	// Log audio chunk details (reduce frequency to avoid spam)
	// Only log every 50th chunk or when chunk size changes
	c.chunkCount++

	if c.chunkCount%50 == 1 || len(audioData) != c.lastChunkSize {
		log.Printf("[AUDIO] Sending chunk #%d, size: %d bytes", c.chunkCount, len(audioData))
		c.lastChunkSize = len(audioData)
	}

	// Send raw audio bytes directly (not JSON, not base64)
	err := c.wsConn.WriteMessage(websocket.BinaryMessage, audioData)

	if err != nil {
		log.Printf("[AUDIO] ERROR: Failed to send audio chunk: %v", err)
	}

	// If we get a close error, the connection is no longer usable
	if err != nil && (websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) ||
		strings.Contains(err.Error(), "websocket: close sent") ||
		strings.Contains(err.Error(), "use of closed network connection")) {
		// Clean up the connection since it's no longer usable
		log.Printf("[AUDIO] Connection closed, cleaning up WebSocket")
		c.wsConn = nil
	}

	return err
}

func (c *Client) Terminate() error {
	c.wsMutex.Lock()
	defer c.wsMutex.Unlock()

	log.Printf("[WS] Sending termination signal at %s", time.Now().Format("15:04:05.000"))

	if c.wsConn != nil {
		// Send termination message to AssemblyAI (like Python example)
		terminateMessage := map[string]string{"type": "Terminate"}
		if jsonData, err := json.Marshal(terminateMessage); err == nil {
			err = c.wsConn.WriteMessage(websocket.TextMessage, jsonData)
			if err != nil {
				log.Printf("[WS] ERROR: Failed to send termination message: %v", err)
			} else {
				log.Printf("[WS] Termination message sent successfully")
			}
		} else {
			log.Printf("[WS] ERROR: Failed to marshal termination message: %v", err)
		}
	} else {
		log.Printf("[WS] WARNING: Attempted to terminate null connection")
	}
	return nil
}

func (c *Client) Close() {
	c.wsMutex.Lock()
	defer c.wsMutex.Unlock()

	log.Printf("[WS] Closing connection at %s", time.Now().Format("15:04:05.000"))

	if c.wsConn != nil {
		// Send close frame to AssemblyAI before closing
		err := c.wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		if err != nil {
			log.Printf("[WS] ERROR: Failed to send close message: %v", err)
		}
		c.wsConn.Close()
		c.wsConn = nil
		log.Printf("[WS] Connection closed and cleaned up")
	} else {
		log.Printf("[WS] WARNING: Attempted to close null connection")
	}

	// Reset chunk counters for next session
	c.chunkCount = 0
	c.lastChunkSize = 0

	// Notify connection callback
	if c.connectionCallback != nil {
		c.connectionCallback(false)
	}
}

func (c *Client) IsConnected() bool {
	c.wsMutex.Lock()
	defer c.wsMutex.Unlock()

	if c.wsConn == nil {
		log.Printf("[WS] Connection check: null connection")
		return false
	}

	// Test if connection is still alive with a simple ping
	// If this fails, the connection was closed by the server
	err := c.wsConn.WriteMessage(websocket.PingMessage, []byte{})
	if err != nil {
		// Connection is dead, clean it up
		log.Printf("[WS] Connection check failed, cleaning up: %v", err)
		c.wsConn.Close()
		c.wsConn = nil
		c.connectionHealth = 0
		return false
	}
	log.Printf("[WS] Connection check: healthy")
	return true
}

func (c *Client) handleResponses() {
	log.Printf("[WS] Starting response handler goroutine")
	for {
		c.wsMutex.Lock()
		conn := c.wsConn
		c.wsMutex.Unlock()

		if conn == nil {
			log.Printf("[WS] Response handler exiting: null connection")
			break
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[WS] Response handler exiting: connection closed normally")
				return
			}
			// Suppress network connection closed errors during shutdown
			if strings.Contains(err.Error(), "use of closed network connection") ||
				strings.Contains(err.Error(), "connection reset by peer") {
				log.Printf("[WS] Response handler exiting: network error during shutdown")
				return
			}
			log.Printf("[WS] ERROR: Response handler error: %v", err)
			return
		}

		log.Printf("[RECV] Message received, size: %d bytes", len(message))

		// Parse the message
		var baseMsg map[string]any
		if err := json.Unmarshal(message, &baseMsg); err != nil {
			log.Printf("[RECV] ERROR: Failed to parse message: %v", err)
			continue
		}

		// Handle different message types (matching JS example)
		if msgType, ok := baseMsg["type"].(string); ok {
			switch msgType {
			case "Begin":
				if sessionId, ok := baseMsg["id"].(string); ok {
					log.Printf("[RECV] Session began: %s", sessionId)
				} else {
					log.Printf("[RECV] Session began (no ID)")
				}

			case "Turn":
				if transcript, ok := baseMsg["transcript"].(string); ok && transcript != "" {
					// Check if this is a formatted turn (final) or partial
					isComplete := false
					if formatted, ok := baseMsg["turn_is_formatted"].(bool); ok && formatted {
						isComplete = true
					}

					// Extract completion indicators from AssemblyAI
					endOfTurn := false
					if eot, ok := baseMsg["end_of_turn"].(bool); ok {
						endOfTurn = eot
					}

					confidence := 0.0
					if conf, ok := baseMsg["end_of_turn_confidence"].(float64); ok {
						confidence = conf
					}

					// Log transcript details with completion signals
					transcriptType := "partial"
					if isComplete {
						transcriptType = "final"
					}
					log.Printf("[RECV] Transcript (%s): %d chars: \"%s\" | end_of_turn: %v, confidence: %.2f",
						transcriptType, len(transcript), transcript, endOfTurn, confidence)

					// Send transcript to callback with completion indicators
					if c.transcriptCallback != nil {
						c.transcriptCallback(transcript, isComplete, endOfTurn, confidence)
					}
				}

			case "Termination":
				log.Printf("[RECV] Session termination received")
				if c.terminationCallback != nil {
					c.terminationCallback()
				}

			default:
				log.Printf("[RECV] Unknown message type: %s", msgType)
			}
		} else {
			log.Printf("[RECV] Message without type field")
		}
	}
	log.Printf("[WS] Response handler goroutine ended")
}

// ConnectionNeedsRefresh returns true if connection should be refreshed due to degradation
func (c *Client) ConnectionNeedsRefresh() bool {
	c.wsMutex.Lock()
	defer c.wsMutex.Unlock()

	// Force refresh if health is very low
	if c.connectionHealth < 20 {
		return true
	}

	// Force refresh if too many consecutive failed sessions
	if c.failedSessions >= 3 {
		return true
	}

	// Force refresh if connection is very old and showing signs of degradation
	connectionAge := time.Since(c.lastConnectionTime)
	if connectionAge > 10*time.Minute && c.connectionHealth < 60 {
		return true
	}

	return false
}

// ReportSessionSuccess improves connection health
func (c *Client) ReportSessionSuccess() {
	c.wsMutex.Lock()
	defer c.wsMutex.Unlock()

	c.sessionCount++
	c.failedSessions = 0 // Reset failed count on success

	// Improve health but cap at 100
	if c.connectionHealth < 100 {
		c.connectionHealth += 10
		if c.connectionHealth > 100 {
			c.connectionHealth = 100
		}
	}
}

// ReportSessionFailure degrades connection health
func (c *Client) ReportSessionFailure() {
	c.wsMutex.Lock()
	defer c.wsMutex.Unlock()

	c.failedSessions++

	// Degrade health
	c.connectionHealth -= 15
	if c.connectionHealth < 0 {
		c.connectionHealth = 0
	}
}
