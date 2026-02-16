package messenger

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
)

// MessageBroker abstracts message routing (Redis or local-only)
type MessageBroker interface {
	Publish(channel string, msg []byte) error
	SetPresence(key string, serverID string, ttl time.Duration) error
	RemovePresence(key string) error
	CountAgents(pattern string) (int, error)
	Ping() error
}

// RedisBroker implements MessageBroker using Redis
type RedisBroker struct {
	client *redis.Client
}

const presenceZSetKey = "agent_presence_zset"
const heartbeatZSetKey = "agent_heartbeat_zset"
const activeWindowSec = 60

func (b *RedisBroker) Publish(channel string, msg []byte) error {
	return b.client.Publish(context.Background(), channel, msg).Err()
}

func (b *RedisBroker) SetPresence(key, serverID string, ttl time.Duration) error {
	ctx := context.Background()
	agentID := strings.TrimPrefix(key, "agent_server:")
	now := float64(time.Now().Unix())

	pipe := b.client.TxPipeline()
	pipe.Set(ctx, key, serverID, ttl)
	pipe.ZAdd(ctx, presenceZSetKey, redis.Z{Score: now, Member: agentID})
	_, err := pipe.Exec(ctx)
	return err
}

func (b *RedisBroker) RemovePresence(key string) error {
	ctx := context.Background()
	agentID := strings.TrimPrefix(key, "agent_server:")
	pipe := b.client.TxPipeline()
	pipe.Del(ctx, key)
	pipe.ZRem(ctx, presenceZSetKey, agentID)
	_, err := pipe.Exec(ctx)
	return err
}

func (b *RedisBroker) CountAgents(pattern string) (int, error) {
	ctx := context.Background()
	now := time.Now().Unix()
	cutoff := now - activeWindowSec

	// Cleanup stale activity markers first
	if err := b.client.ZRemRangeByScore(ctx, presenceZSetKey, "-inf", strconv.FormatInt(cutoff, 10)).Err(); err != nil {
		return 0, err
	}

	count, err := b.client.ZCount(ctx, presenceZSetKey, strconv.FormatInt(cutoff, 10), "+inf").Result()
	if err != nil {
		return 0, err
	}
	return int(count), nil
}

func (b *RedisBroker) Ping() error {
	return b.client.Ping(context.Background()).Err()
}

// LocalBroker is a no-op broker for single-instance mode (no Redis)
type LocalBroker struct{}

func (b *LocalBroker) Publish(channel string, msg []byte) error                  { return nil }
func (b *LocalBroker) SetPresence(key, serverID string, ttl time.Duration) error { return nil }
func (b *LocalBroker) RemovePresence(key string) error                           { return nil }
func (b *LocalBroker) CountAgents(pattern string) (int, error)                   { return 0, nil }
func (b *LocalBroker) Ping() error                                               { return nil }

// AgentClient is the interface for real-time messaging clients (SSE or WebSocket)
type AgentClient interface {
	Send(msg Message) error
	GetPublicKey() string
	Close()
}

// SSEClient implements AgentClient for Server-Sent Events
type SSEClient struct {
	PublicKey string
	Messages  chan Message
}

func (c *SSEClient) Send(msg Message) error {
	select {
	case c.Messages <- msg:
		return nil
	default:
		log.Printf("Warning: message dropped for SSE client %s (channel full)", c.PublicKey[:min(16, len(c.PublicKey))])
		return nil
	}
}

func (c *SSEClient) GetPublicKey() string { return c.PublicKey }

func (c *SSEClient) Close() { close(c.Messages) }

// WSClient implements AgentClient for WebSocket
type WSClient struct {
	PublicKey string
	Conn      *websocket.Conn
	closed    bool
	mu        sync.Mutex
}

func (c *WSClient) Send(msg Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	return c.Conn.WriteJSON(msg)
}

func (c *WSClient) GetPublicKey() string { return c.PublicKey }

func (c *WSClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		c.Conn.Close()
	}
}

// Handler handles messenger requests
type Handler struct {
	broker         MessageBroker
	clients        map[string]map[AgentClient]bool
	heartbeats     map[string]int64
	mu             sync.RWMutex
	serverID       string
	remoteDelivery bool // true when a subscriber handles local broadcast (e.g. Redis pub/sub)
	messagesTotal  uint64
}

const presenceTTL = 60 * time.Second

// New creates a new messenger handler with an optional broker
func New(broker MessageBroker) *Handler {
	if broker == nil {
		broker = &LocalBroker{}
	}
	h := &Handler{
		broker:     broker,
		clients:    make(map[string]map[AgentClient]bool),
		heartbeats: make(map[string]int64),
	}
	return h
}

// NewWithRedis creates a handler with Redis broker and starts subscription
func NewWithRedis(addr, password string) *Handler {
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       0,
	})

	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		log.Printf("Warning: Redis connection failed: %v, falling back to local", err)
		return New(nil)
	}
	log.Printf("Redis connected: %s", addr)

	broker := &RedisBroker{client: client}
	h := New(broker)
	h.remoteDelivery = true

	// Subscribe to all agent messages
	pubsub := client.PSubscribe(ctx, "agent:*")
	go func() {
		ch := pubsub.Channel()
		for msg := range ch {
			var m Message
			if err := json.Unmarshal([]byte(msg.Payload), &m); err != nil {
				log.Printf("Failed to unmarshal: %v", err)
				continue
			}
			h.broadcastToAgent(m.ReceiverID, m)
		}
	}()

	return h
}

func (h *Handler) getServerID() string {
	if h.serverID == "" {
		hostname, _ := os.Hostname()
		h.serverID = hostname
	}
	return h.serverID
}

func (h *Handler) touchHeartbeat(agentID string) error {
	now := time.Now().Unix()
	if rb, ok := h.broker.(*RedisBroker); ok {
		ctx := context.Background()
		return rb.client.ZAdd(ctx, heartbeatZSetKey, redis.Z{Score: float64(now), Member: agentID}).Err()
	}

	h.mu.Lock()
	h.heartbeats[agentID] = now
	h.mu.Unlock()
	return nil
}

func (h *Handler) countActiveHeartbeats() (int, error) {
	now := time.Now().Unix()
	cutoff := now - activeWindowSec
	if rb, ok := h.broker.(*RedisBroker); ok {
		ctx := context.Background()
		if err := rb.client.ZRemRangeByScore(ctx, heartbeatZSetKey, "-inf", strconv.FormatInt(cutoff, 10)).Err(); err != nil {
			return 0, err
		}
		count, err := rb.client.ZCount(ctx, heartbeatZSetKey, strconv.FormatInt(cutoff, 10), "+inf").Result()
		if err != nil {
			return 0, err
		}
		return int(count), nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for k, ts := range h.heartbeats {
		if ts < cutoff {
			delete(h.heartbeats, k)
		}
	}
	return len(h.heartbeats), nil
}

// registerClient registers a client for receiving messages
func (h *Handler) registerClient(publicKey string, client AgentClient) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.clients[publicKey] == nil {
		h.clients[publicKey] = make(map[AgentClient]bool)
	}
	h.clients[publicKey][client] = true

	if err := h.broker.SetPresence("agent_server:"+publicKey, h.getServerID(), presenceTTL); err != nil {
		log.Printf("SetPresence failed for %s: %v", publicKey[:min(16, len(publicKey))], err)
	}
	log.Printf("Registered client: %s (total: %d)", publicKey[:min(16, len(publicKey))], len(h.clients[publicKey]))
}

// unregisterClient removes a client
func (h *Handler) unregisterClient(publicKey string, client AgentClient) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.clients[publicKey] != nil {
		delete(h.clients[publicKey], client)
		client.Close()

		if len(h.clients[publicKey]) == 0 {
			delete(h.clients, publicKey)
			h.broker.RemovePresence("agent_server:" + publicKey)
		}
	}
	log.Printf("Unregistered client: %s (remaining: %d)", publicKey[:min(16, len(publicKey))], len(h.clients[publicKey]))
}

// broadcastToAgent sends a message to all local clients for a given public key
func (h *Handler) broadcastToAgent(publicKey string, msg Message) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if clientSet, ok := h.clients[publicKey]; ok {
		for client := range clientSet {
			if err := client.Send(msg); err != nil {
				log.Printf("Failed to send to client: %v", err)
			}
		}
	}
}

// SendMessage sends a message to another agent
func (h *Handler) SendMessage(c *gin.Context) {
	senderID := c.GetHeader("X-Agent-ID")
	if senderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Agent-ID header is required"})
		return
	}

	var req SendRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request: " + err.Error()})
		return
	}

	if req.SenderID != senderID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "sender_id must match X-Agent-ID header"})
		return
	}
	if req.ProtocolVersion != ProtocolVersion {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported protocol_version"})
		return
	}

	switch req.Type {
	case MsgTypeHello:
		if req.SenderPublicKey == "" || req.EphemeralPubKey == "" || req.Signature == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "sender_public_key, ephemeral_public_key and signature are required for hello"})
			return
		}
		if req.SenderMessengerURL == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "sender_messenger_url is required for hello"})
			return
		}
	case MsgTypeHelloAck:
		if req.SenderPublicKey == "" || req.EphemeralPubKey == "" || req.PeerEphemeralPubKey == "" || req.Signature == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "sender_public_key, ephemeral_public_key, peer_ephemeral_public_key and signature are required for hello_ack"})
			return
		}
	case MsgTypeData:
		if req.SessionID == "" || req.Ciphertext == "" || req.Counter == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "session_id, counter and ciphertext are required for data"})
			return
		}
	case MsgTypeReset:
		if req.SessionID == "" || req.SenderPublicKey == "" || req.ResetSignature == "" || req.Reason == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "session_id, sender_public_key, reset_signature and reason are required for reset"})
			return
		}
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid type"})
		return
	}

	msg := &Message{
		ID:                  uuid.New().String(),
		ProtocolVersion:     req.ProtocolVersion,
		Type:                req.Type,
		SenderID:            req.SenderID,
		ReceiverID:          req.ReceiverID,
		SenderPublicKey:     req.SenderPublicKey,
		EphemeralPubKey:     req.EphemeralPubKey,
		PeerEphemeralPubKey: req.PeerEphemeralPubKey,
		Signature:           req.Signature,
		SenderMessengerURL:  req.SenderMessengerURL,
		SessionID:           req.SessionID,
		Counter:             req.Counter,
		Ciphertext:          req.Ciphertext,
		ResetSignature:      req.ResetSignature,
		Reason:              req.Reason,
		CreatedAt:           time.Now(),
	}

	// Publish via broker (Redis or local)
	msgJSON, err := json.Marshal(msg)
	if err != nil {
		log.Printf("Failed to marshal message: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	if err := h.broker.Publish("agent:"+req.ReceiverID, msgJSON); err != nil {
		log.Printf("Failed to publish message: %v", err)
	}

	// Only broadcast locally when there's no remote subscriber to handle it
	// (Redis pub/sub subscriber already calls broadcastToAgent)
	if !h.remoteDelivery {
		h.broadcastToAgent(req.ReceiverID, *msg)
	}

	atomic.AddUint64(&h.messagesTotal, 1)
	c.JSON(http.StatusOK, SendResponse{Status: "ok"})
}

// StreamMessages SSE endpoint for real-time messaging
func (h *Handler) StreamMessages(c *gin.Context) {
	publicKey := c.GetHeader("X-Agent-ID")
	if publicKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Agent-ID header is required"})
		return
	}

	client := &SSEClient{
		PublicKey: publicKey,
		Messages:  make(chan Message, 10),
	}
	h.registerClient(publicKey, client)
	defer h.unregisterClient(publicKey, client)

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	if _, err := c.Writer.Write([]byte("data: ")); err != nil {
		return
	}
	if err := json.NewEncoder(c.Writer).Encode(map[string]string{"type": "connected", "message": "SSE stream established"}); err != nil {
		return
	}
	if _, err := c.Writer.Write([]byte("\n\n")); err != nil {
		return
	}
	c.Writer.Flush()

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case msg := <-client.Messages:
			if _, err := c.Writer.Write([]byte("data: ")); err != nil {
				return
			}
			if err := json.NewEncoder(c.Writer).Encode(msg); err != nil {
				return
			}
			if _, err := c.Writer.Write([]byte("\n\n")); err != nil {
				return
			}
			c.Writer.Flush()
		case <-ticker.C:
			if _, err := c.Writer.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			c.Writer.Flush()
			if err := h.broker.SetPresence("agent_server:"+publicKey, h.getServerID(), presenceTTL); err != nil {
				log.Printf("SetPresence keepalive failed for %s: %v", publicKey[:min(16, len(publicKey))], err)
			}
		case <-c.Request.Context().Done():
			return
		}
	}
}

// StreamMessagesWS WebSocket endpoint for real-time messaging
func (h *Handler) StreamMessagesWS(c *gin.Context) {
	publicKey := c.Query("public_key")
	if publicKey == "" {
		publicKey = c.GetHeader("X-Agent-ID")
	}
	if publicKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "public_key query or X-Agent-ID header is required"})
		return
	}

	wsUpgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	conn, err := wsUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	client := &WSClient{PublicKey: publicKey, Conn: conn}
	h.registerClient(publicKey, client)
	defer h.unregisterClient(publicKey, client)

	conn.WriteJSON(map[string]string{"type": "connected", "message": "WebSocket connected"})

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

// HealthCheck returns health status with Redis connectivity
func (h *Handler) HealthCheck(c *gin.Context) {
	status := "ok"
	httpCode := http.StatusOK

	if err := h.broker.Ping(); err != nil {
		status = "degraded"
		httpCode = http.StatusServiceUnavailable
		log.Printf("Health check: broker ping failed: %v", err)
	}

	c.JSON(httpCode, gin.H{"status": status})
}

// GetServerInfo returns server info
func (h *Handler) GetServerInfo(c *gin.Context) {
	info := gin.H{
		"server_id": h.getServerID(),
	}

	count, err := h.broker.CountAgents("agent_server:*")
	if err != nil {
		log.Printf("CountAgents failed: %v", err)
	} else {
		info["redis_agents"] = count
	}

	h.mu.RLock()
	info["local_agent_count"] = len(h.clients)
	h.mu.RUnlock()

	c.JSON(http.StatusOK, info)
}

// PresenceHeartbeat records agent liveness for active-agent stats.
func (h *Handler) PresenceHeartbeat(c *gin.Context) {
	agentID := c.GetHeader("X-Agent-ID")
	if agentID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Agent-ID header is required"})
		return
	}

	if err := h.touchHeartbeat(agentID); err != nil {
		log.Printf("touchHeartbeat failed for %s: %v", agentID[:min(16, len(agentID))], err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "ttl_seconds": activeWindowSec})
}

// GetPublicStats returns safe public stats for website/status widgets
func (h *Handler) GetPublicStats(c *gin.Context) {
	// CORS for public website widgets (including local dev at 127.0.0.1)
	c.Header("Access-Control-Allow-Origin", "*")
	c.Header("Access-Control-Allow-Methods", "GET, OPTIONS")
	c.Header("Access-Control-Allow-Headers", "Content-Type, X-Agent-ID")
	if c.Request.Method == http.MethodOptions {
		c.Status(http.StatusNoContent)
		return
	}

	connected := 0
	if count, err := h.countActiveHeartbeats(); err == nil {
		connected = count
	} else {
		log.Printf("countActiveHeartbeats failed in public stats: %v", err)
	}

	redisCount := 0
	if count, err := h.broker.CountAgents("agent_server:*"); err == nil {
		redisCount = count
	}

	h.mu.RLock()
	localCount := len(h.clients)
	h.mu.RUnlock()

	resp := gin.H{
		"connected_agents": connected,
		"messages_total":   atomic.LoadUint64(&h.messagesTotal),
		"updated_at":       time.Now().UTC().Format(time.RFC3339),
	}

	// Optional debug fields (disabled by default)
	if os.Getenv("PUBLIC_STATS_DEBUG") == "1" {
		resp["redis_agents"] = redisCount
		resp["local_agents"] = localCount
	}

	c.Header("Cache-Control", "public, max-age=10")
	c.JSON(http.StatusOK, resp)
}
