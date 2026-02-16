package messenger

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// --- MockBroker for testing ---

type MockBroker struct {
	mu            sync.Mutex
	published     []PublishedMessage
	presenceSet   map[string]string
	presenceCount int
}

type PublishedMessage struct {
	Channel string
	Payload []byte
}

func NewMockBroker() *MockBroker {
	return &MockBroker{
		presenceSet: make(map[string]string),
	}
}

func (b *MockBroker) Publish(channel string, msg []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, PublishedMessage{Channel: channel, Payload: msg})
	return nil
}

func (b *MockBroker) SetPresence(key, serverID string, ttl time.Duration) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.presenceSet[key] = serverID
	return nil
}

func (b *MockBroker) RemovePresence(key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.presenceSet, key)
	return nil
}

func (b *MockBroker) CountAgents(pattern string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.presenceSet), nil
}

func (b *MockBroker) Ping() error { return nil }

func (b *MockBroker) GetPublished() []PublishedMessage {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := make([]PublishedMessage, len(b.published))
	copy(result, b.published)
	return result
}

func (b *MockBroker) GetPresence() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := make(map[string]string)
	for k, v := range b.presenceSet {
		result[k] = v
	}
	return result
}

// --- Helper ---

func setupRouter() (*gin.Engine, *Handler, *MockBroker) {
	r := gin.New()
	broker := NewMockBroker()
	h := New(broker)

	r.POST("/v1/messages", h.SendMessage)
	r.GET("/v1/messages/stream", h.StreamMessages)
	r.GET("/health", h.HealthCheck)
	r.GET("/info", h.GetServerInfo)
	return r, h, broker
}

// --- SSEClient Tests ---

func TestSSEClientSend(t *testing.T) {
	client := &SSEClient{PublicKey: "test-key-12345678", Messages: make(chan Message, 1)}
	msg := Message{Type: MsgTypeData, SenderID: "sender"}
	if err := client.Send(msg); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	received := <-client.Messages
	if received.Type != MsgTypeData {
		t.Errorf("expected message type, got %s", received.Type)
	}
}

func TestSSEClientSendDropsWhenFull(t *testing.T) {
	client := &SSEClient{PublicKey: "test-key-12345678", Messages: make(chan Message, 1)}
	client.Send(Message{Type: "first"})
	// Should not block; second message dropped
	client.Send(Message{Type: "second"})
	msg := <-client.Messages
	if msg.Type != "first" {
		t.Errorf("expected 'first', got %s", msg.Type)
	}
}

func TestSSEClientGetPublicKey(t *testing.T) {
	client := &SSEClient{PublicKey: "my-key"}
	if client.GetPublicKey() != "my-key" {
		t.Errorf("expected 'my-key', got %s", client.GetPublicKey())
	}
}

// --- LocalBroker Tests ---

func TestLocalBrokerNoOp(t *testing.T) {
	b := &LocalBroker{}
	if err := b.Publish("ch", []byte("data")); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if err := b.SetPresence("k", "s", time.Minute); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	if err := b.RemovePresence("k"); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	count, err := b.CountAgents("*")
	if err != nil || count != 0 {
		t.Errorf("expected 0/nil, got %d/%v", count, err)
	}
	if err := b.Ping(); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

// FailingBroker simulates a broker with a dead connection
type FailingBroker struct{ LocalBroker }

func (b *FailingBroker) Ping() error { return errors.New("connection refused") }

// --- Handler: Client Registry Tests ---

func TestRegisterAndUnregisterClient(t *testing.T) {
	broker := NewMockBroker()
	h := New(broker)

	client := &SSEClient{PublicKey: "agent-key-12345678", Messages: make(chan Message, 10)}
	h.registerClient("agent-key-12345678", client)

	h.mu.RLock()
	count := len(h.clients["agent-key-12345678"])
	h.mu.RUnlock()
	if count != 1 {
		t.Errorf("expected 1 client, got %d", count)
	}

	// Verify presence was set
	presence := broker.GetPresence()
	if _, ok := presence["agent_server:agent-key-12345678"]; !ok {
		t.Error("expected presence to be set in broker")
	}

	h.unregisterClient("agent-key-12345678", client)

	h.mu.RLock()
	count = len(h.clients["agent-key-12345678"])
	h.mu.RUnlock()
	if count != 0 {
		t.Errorf("expected 0 clients, got %d", count)
	}

	// Verify presence was removed
	presence = broker.GetPresence()
	if _, ok := presence["agent_server:agent-key-12345678"]; ok {
		t.Error("expected presence to be removed from broker")
	}
}

func TestBroadcastToAgent(t *testing.T) {
	h := New(NewMockBroker())
	c1 := &SSEClient{PublicKey: "agent", Messages: make(chan Message, 10)}
	c2 := &SSEClient{PublicKey: "agent", Messages: make(chan Message, 10)}
	h.registerClient("agent", c1)
	h.registerClient("agent", c2)

	h.broadcastToAgent("agent", Message{Type: MsgTypeData, SenderID: "sender"})

	for _, c := range []*SSEClient{c1, c2} {
		select {
		case m := <-c.Messages:
			if m.Type != MsgTypeData {
				t.Errorf("expected message, got %s", m.Type)
			}
		case <-time.After(100 * time.Millisecond):
			t.Error("did not receive message")
		}
	}
	h.unregisterClient("agent", c1)
	h.unregisterClient("agent", c2)
}

func TestBroadcastToNonexistentAgent(t *testing.T) {
	h := New(NewMockBroker())
	h.broadcastToAgent("nonexistent", Message{Type: MsgTypeData}) // should not panic
}

func TestConcurrentClientOperations(t *testing.T) {
	h := New(NewMockBroker())
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &SSEClient{PublicKey: "concurrent-key-12345678", Messages: make(chan Message, 10)}
			h.registerClient("concurrent-key-12345678", c)
			h.broadcastToAgent("concurrent-key-12345678", Message{Type: MsgTypeData})
			h.unregisterClient("concurrent-key-12345678", c)
		}()
	}
	wg.Wait()
}

// --- Handler: HTTP Tests ---

func TestHealthCheck(t *testing.T) {
	r, _, _ := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/health", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "ok" {
		t.Errorf("expected ok, got %s", resp["status"])
	}
}

func TestHealthCheckDegraded(t *testing.T) {
	r := gin.New()
	h := New(&FailingBroker{})
	r.GET("/health", h.HealthCheck)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/health", nil)
	r.ServeHTTP(w, req)

	if w.Code != 503 {
		t.Errorf("expected 503, got %d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "degraded" {
		t.Errorf("expected degraded, got %s", resp["status"])
	}
}

func TestGetServerInfo(t *testing.T) {
	r, h, broker := setupRouter()

	// Register some clients to verify count
	c := &SSEClient{PublicKey: "info-test", Messages: make(chan Message, 10)}
	h.registerClient("info-test", c)
	defer h.unregisterClient("info-test", c)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/info", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp["local_agent_count"].(float64) != 1 {
		t.Errorf("expected local_agent_count=1, got %v", resp["local_agent_count"])
	}

	// broker should return count of presence keys
	redisAgents := int(resp["redis_agents"].(float64))
	presence := broker.GetPresence()
	if redisAgents != len(presence) {
		t.Errorf("redis_agents=%d, broker presence=%d", redisAgents, len(presence))
	}
}

func TestSendMessageBodyTooLarge(t *testing.T) {
	broker := NewMockBroker()
	h := New(broker)
	r := gin.New()

	// Same middleware as main.go
	r.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64*1024)
		c.Next()
	})
	r.POST("/v1/messages", h.SendMessage)

	// 128KB payload — exceeds 64KB limit
	bigBody := strings.Repeat("x", 128*1024)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(bigBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-ID", "sender")
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("expected 400 for oversized body, got %d", w.Code)
	}
}

func TestSendMessageMissingHeader(t *testing.T) {
	r, _, _ := setupRouter()
	body := `{"protocol_version":"0.1.0","type":"data","sender_id":"s","receiver_id":"r","session_id":"sid","counter":0,"ciphertext":"ct"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestSendMessageInvalidJSON(t *testing.T) {
	r, _, _ := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString("not json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-ID", "sender")
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestSendMessageSenderMismatch(t *testing.T) {
	r, _, _ := setupRouter()
	body := `{"protocol_version":"0.1.0","type":"data","sender_id":"wrong","receiver_id":"r","session_id":"sid","counter":0,"ciphertext":"ct"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-ID", "actual")
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Errorf("expected 400, got %d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "sender_id must match X-Agent-ID header" {
		t.Errorf("unexpected error: %s", resp["error"])
	}
}

func TestSendMessageUnsupportedProtocolVersion(t *testing.T) {
	r, _, _ := setupRouter()
	body := `{"protocol_version":"0.2.0","type":"data","sender_id":"s","receiver_id":"r","session_id":"sid","counter":0,"ciphertext":"ct"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-ID", "s")
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Errorf("expected 400, got %d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "unsupported protocol_version" {
		t.Errorf("unexpected error: %s", resp["error"])
	}
}

func TestSendMessageMissingRequiredFields(t *testing.T) {
	r, _, _ := setupRouter()

	tests := []struct {
		name string
		body string
	}{
		{"missing type", `{"sender_id":"s","receiver_id":"r"}`},
		{"missing receiver_id", `{"protocol_version":"0.1.0","type":"data","sender_id":"s"}`},
		{"missing sender_id", `{"protocol_version":"0.1.0","type":"data","receiver_id":"r"}`},
	}

	for _, tt := range tests {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(tt.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Agent-ID", "s")
		r.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Errorf("%s: expected 400, got %d", tt.name, w.Code)
		}
	}
}

func TestSendMessageTypeValidation(t *testing.T) {
	r, _, _ := setupRouter()

	tests := []struct {
		name string
		body string
	}{
		{"invalid type", `{"protocol_version":"0.1.0","type":"unknown","sender_id":"s","receiver_id":"r"}`},
		{"hello without required fields", `{"protocol_version":"0.1.0","type":"hello","sender_id":"s","receiver_id":"r","signature":"sig"}`},
		{"hello_ack without required fields", `{"protocol_version":"0.1.0","type":"hello_ack","sender_id":"s","receiver_id":"r","sender_public_key":"pub","ephemeral_public_key":"eph","signature":"sig"}`},
		{"data without required fields", `{"protocol_version":"0.1.0","type":"data","sender_id":"s","receiver_id":"r"}`},
		{"reset without required fields", `{"protocol_version":"0.1.0","type":"reset","sender_id":"s","receiver_id":"r","reason":1}`},
	}

	for _, tt := range tests {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(tt.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Agent-ID", "s")
		r.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Errorf("%s: expected 400, got %d", tt.name, w.Code)
		}
	}
}

func TestSendMessageValidHello(t *testing.T) {
	r, _, broker := setupRouter()
	body := `{"protocol_version":"0.1.0","type":"hello","sender_id":"s","receiver_id":"r","sender_public_key":"pub","ephemeral_public_key":"eph","signature":"sig","sender_messenger_url":"https://s.example"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-ID", "s")
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify message was published to broker
	published := broker.GetPublished()
	if len(published) != 1 {
		t.Fatalf("expected 1 published message, got %d", len(published))
	}
	if published[0].Channel != "agent:r" {
		t.Errorf("expected channel agent:r, got %s", published[0].Channel)
	}
}

func TestSendMessageValidDataWithLocalBroadcast(t *testing.T) {
	r, h, broker := setupRouter()

	// Register a receiver
	receiver := &SSEClient{PublicKey: "r", Messages: make(chan Message, 10)}
	h.registerClient("r", receiver)
	defer h.unregisterClient("r", receiver)

	body := `{"protocol_version":"0.1.0","type":"data","sender_id":"s","receiver_id":"r","session_id":"sid","ciphertext":"enc","counter":0}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-ID", "s")
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify local broadcast
	select {
	case msg := <-receiver.Messages:
		if msg.Ciphertext != "enc" {
			t.Errorf("expected ciphertext 'enc', got %s", msg.Ciphertext)
		}
		if msg.ID == "" {
			t.Error("expected message ID to be set")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("receiver did not get message")
	}

	// Verify broker publish
	published := broker.GetPublished()
	if len(published) != 1 {
		t.Errorf("expected 1 publish, got %d", len(published))
	}
}

func TestSendMessageValidReset(t *testing.T) {
	r, _, _ := setupRouter()
	body := `{"protocol_version":"0.1.0","type":"reset","sender_id":"s","receiver_id":"r","session_id":"sid","sender_public_key":"pub","reset_signature":"sig","reason":1}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-ID", "s")
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

// --- SSE Stream Test ---

func TestStreamMessagesMissingHeader(t *testing.T) {
	r, _, _ := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/messages/stream", nil)
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestStreamMessagesConnectAndReceive(t *testing.T) {
	broker := NewMockBroker()
	h := New(broker)

	r := gin.New()
	r.GET("/v1/messages/stream", h.StreamMessages)

	// Create a cancellable request
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", "/v1/messages/stream", nil)
	req.Header.Set("X-Agent-ID", "stream-test-key")
	w := httptest.NewRecorder()

	// Run SSE handler in goroutine
	done := make(chan struct{})
	go func() {
		r.ServeHTTP(w, req)
		close(done)
	}()

	// Wait for client to register
	time.Sleep(100 * time.Millisecond)

	// Send a message to the registered client
	h.broadcastToAgent("stream-test-key", Message{
		Type:       MsgTypeData,
		SenderID:   "peer",
		Ciphertext: "hello-stream",
	})

	// Give time for message to be written
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	// Parse SSE output
	body := w.Body.String()

	// Should have Content-Type header
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %s", ct)
	}

	// Should contain connected event
	if !strings.Contains(body, "connected") {
		t.Error("expected 'connected' in SSE output")
	}

	// Should contain our message
	if !strings.Contains(body, "hello-stream") {
		t.Errorf("expected 'hello-stream' in SSE output, got:\n%s", body)
	}

	// Parse the SSE data lines
	scanner := bufio.NewScanner(strings.NewReader(body))
	messageCount := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			messageCount++
		}
	}
	if messageCount < 2 {
		t.Errorf("expected at least 2 SSE data events (connected + message), got %d", messageCount)
	}

	// Verify presence was set
	presence := broker.GetPresence()
	// After cancel, the client should be unregistered, presence removed
	if _, ok := presence["agent_server:stream-test-key"]; ok {
		t.Error("expected presence to be removed after disconnect")
	}
}

// --- New() with nil broker ---

func TestNewWithNilBrokerUsesLocal(t *testing.T) {
	h := New(nil)
	if h == nil {
		t.Fatal("expected non-nil handler")
	}
	// Should work without panicking
	h.broadcastToAgent("nobody", Message{Type: MsgTypeData})
}

func TestGetServerID(t *testing.T) {
	h := New(nil)
	id := h.getServerID()
	if id == "" {
		t.Error("expected non-empty server ID")
	}
	// Should be cached
	if h.getServerID() != id {
		t.Error("expected same server ID on second call")
	}
}
