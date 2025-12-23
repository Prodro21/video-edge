package websocket

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for local network
	},
}

// Event represents a WebSocket event
type Event struct {
	Type      string      `json:"type"`
	Data      interface{} `json:"data"`
	Timestamp time.Time   `json:"timestamp"`
}

// Client represents a connected WebSocket client
type Client struct {
	hub      *Hub
	conn     *websocket.Conn
	send     chan []byte
	deviceID string
}

// Hub manages all WebSocket connections
type Hub struct {
	mu         sync.RWMutex
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	upstream   *websocket.Conn
	upstreamMu sync.Mutex
}

// NewHub creates a new WebSocket hub
func NewHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte, 256),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

// Run starts the hub's event loop
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
			log.Printf("Client connected: %s (total: %d)", client.deviceID, len(h.clients))

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			h.mu.Unlock()
			log.Printf("Client disconnected: %s (total: %d)", client.deviceID, len(h.clients))

		case message := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					close(client.send)
					delete(h.clients, client)
				}
			}
			h.mu.RUnlock()
		}
	}
}

// Broadcast sends an event to all connected clients
func (h *Hub) Broadcast(event Event) {
	event.Timestamp = time.Now()
	data, err := json.Marshal(event)
	if err != nil {
		log.Printf("Failed to marshal event: %v", err)
		return
	}
	h.broadcast <- data
}

// ClientCount returns the number of connected clients
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// HandleWebSocket handles WebSocket upgrade requests
func (h *Hub) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	deviceID := r.URL.Query().Get("device_id")
	if deviceID == "" {
		deviceID = r.RemoteAddr
	}

	client := &Client{
		hub:      h,
		conn:     conn,
		send:     make(chan []byte, 256),
		deviceID: deviceID,
	}

	h.register <- client

	// Send welcome message
	welcome := Event{
		Type: "connected",
		Data: map[string]interface{}{
			"device_id":    deviceID,
			"server":       "video-edge",
			"client_count": h.ClientCount(),
		},
	}
	data, _ := json.Marshal(welcome)
	client.send <- data

	go client.writePump()
	go client.readPump()
}

// writePump sends messages to the WebSocket connection
func (c *Client) writePump() {
	ticker := time.NewTicker(30 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)
			w.Close()

		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// readPump reads messages from the WebSocket connection
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()

	c.conn.SetReadLimit(512 * 1024) // 512KB max message
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket error: %v", err)
			}
			break
		}

		// Handle incoming messages (e.g., clip requests, tag updates)
		c.handleMessage(message)
	}
}

// handleMessage processes incoming WebSocket messages
func (c *Client) handleMessage(message []byte) {
	var event Event
	if err := json.Unmarshal(message, &event); err != nil {
		log.Printf("Invalid message from %s: %v", c.deviceID, err)
		return
	}

	log.Printf("Received %s from %s", event.Type, c.deviceID)

	// Handle different event types
	switch event.Type {
	case "ping":
		// Respond with pong
		c.send <- mustMarshal(Event{Type: "pong", Timestamp: time.Now()})
	case "subscribe":
		// Client wants to subscribe to specific events
		// (future: implement subscription filtering)
	default:
		log.Printf("Unknown event type: %s", event.Type)
	}
}

// ConnectUpstream establishes a WebSocket connection to the upstream server
func (h *Hub) ConnectUpstream(upstreamURL string) error {
	h.upstreamMu.Lock()
	defer h.upstreamMu.Unlock()

	// Close existing connection
	if h.upstream != nil {
		h.upstream.Close()
	}

	conn, _, err := websocket.DefaultDialer.Dial(upstreamURL, nil)
	if err != nil {
		return err
	}

	h.upstream = conn
	go h.forwardUpstreamEvents()
	log.Printf("Connected to upstream WebSocket: %s", upstreamURL)
	return nil
}

// forwardUpstreamEvents forwards events from upstream to local clients
func (h *Hub) forwardUpstreamEvents() {
	for {
		h.upstreamMu.Lock()
		conn := h.upstream
		h.upstreamMu.Unlock()

		if conn == nil {
			return
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("Upstream WebSocket error: %v", err)
			h.upstreamMu.Lock()
			h.upstream = nil
			h.upstreamMu.Unlock()
			return
		}

		// Forward to all local clients
		h.broadcast <- message
	}
}

// DisconnectUpstream closes the upstream WebSocket connection
func (h *Hub) DisconnectUpstream() {
	h.upstreamMu.Lock()
	defer h.upstreamMu.Unlock()

	if h.upstream != nil {
		h.upstream.Close()
		h.upstream = nil
	}
}

func mustMarshal(v interface{}) []byte {
	data, _ := json.Marshal(v)
	return data
}
