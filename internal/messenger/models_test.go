package messenger

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMessageConstants(t *testing.T) {
	if ProtocolVersion != "0.1.0" {
		t.Errorf("ProtocolVersion = %s, want 0.1.0", ProtocolVersion)
	}
	if MsgTypeHello != "hello" {
		t.Errorf("MsgTypeHello = %s, want hello", MsgTypeHello)
	}
	if MsgTypeHelloAck != "hello_ack" {
		t.Errorf("MsgTypeHelloAck = %s, want hello_ack", MsgTypeHelloAck)
	}
	if MsgTypeData != "data" {
		t.Errorf("MsgTypeData = %s, want data", MsgTypeData)
	}
	if MsgTypeReset != "reset" {
		t.Errorf("MsgTypeReset = %s, want reset", MsgTypeReset)
	}
}

func TestMessageJSONOmitsEmpty(t *testing.T) {
	msg := Message{
		ID:              "test-id",
		ProtocolVersion: ProtocolVersion,
		Type:            MsgTypeHello,
		SenderID:        "sender",
		ReceiverID:      "receiver",
		CreatedAt:       time.Now(),
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var raw map[string]interface{}
	json.Unmarshal(data, &raw)

	// These optional fields should be omitted when empty
	omittedFields := []string{
		"sender_public_key",
		"ephemeral_public_key",
		"peer_ephemeral_public_key",
		"signature",
		"sender_messenger_url",
		"session_id",
		"counter",
		"ciphertext",
		"reset_signature",
		"reason",
	}
	for _, field := range omittedFields {
		if _, exists := raw[field]; exists {
			t.Errorf("expected %s to be omitted when empty", field)
		}
	}

	// These required fields should always be present
	requiredFields := []string{"id", "protocol_version", "type", "sender_id", "receiver_id", "created_at"}
	for _, field := range requiredFields {
		if _, exists := raw[field]; !exists {
			t.Errorf("expected %s to be present", field)
		}
	}
}

func TestMessageJSONIncludesOptionalWhenSet(t *testing.T) {
	counter := uint64(3)
	reason := 1
	msg := Message{
		ID:                  "test-id",
		ProtocolVersion:     ProtocolVersion,
		Type:                MsgTypeData,
		SenderID:            "sender",
		ReceiverID:          "receiver",
		SenderPublicKey:     "pub",
		EphemeralPubKey:     "eph-key",
		PeerEphemeralPubKey: "peer-eph-key",
		Signature:           "sig",
		SenderMessengerURL:  "https://m.example",
		SessionID:           "session",
		Ciphertext:          "ct",
		Counter:             &counter,
		ResetSignature:      "reset-sig",
		Reason:              &reason,
		CreatedAt:           time.Now(),
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal: %v", err)
	}

	var raw map[string]interface{}
	json.Unmarshal(data, &raw)

	if raw["ephemeral_public_key"] != "eph-key" {
		t.Errorf("expected ephemeral_public_key=eph-key, got %v", raw["ephemeral_public_key"])
	}
	if raw["ciphertext"] != "ct" {
		t.Errorf("expected ciphertext=ct, got %v", raw["ciphertext"])
	}
	if raw["counter"].(float64) != 3 {
		t.Errorf("expected counter=3, got %v", raw["counter"])
	}
	if raw["reason"].(float64) != 1 {
		t.Errorf("expected reason=1, got %v", raw["reason"])
	}
}

func TestMessageJSONRoundtrip(t *testing.T) {
	reason := 2
	original := Message{
		ID:              "msg-123",
		ProtocolVersion: ProtocolVersion,
		Type:            MsgTypeReset,
		SenderID:        "sender",
		ReceiverID:      "receiver",
		SessionID:       "session-id",
		ResetSignature:  "reset-sig",
		Reason:          &reason,
		CreatedAt:       time.Now().Truncate(time.Second),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var decoded Message
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if decoded.ID != original.ID {
		t.Errorf("ID mismatch: %s vs %s", decoded.ID, original.ID)
	}
	if decoded.Type != original.Type {
		t.Errorf("Type mismatch: %s vs %s", decoded.Type, original.Type)
	}
	if decoded.SessionID != original.SessionID {
		t.Errorf("SessionID mismatch")
	}
	if decoded.Reason == nil || *decoded.Reason != *original.Reason {
		t.Errorf("Reason mismatch")
	}
}

func TestSendRequestJSON(t *testing.T) {
	jsonStr := `{
		"protocol_version": "0.1.0",
		"type": "data",
		"sender_id": "sender",
		"receiver_id": "receiver",
		"session_id": "session",
		"ciphertext": "encrypted",
		"counter": 5
	}`

	var req SendRequest
	if err := json.Unmarshal([]byte(jsonStr), &req); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if req.Type != "data" {
		t.Errorf("Type = %s, want data", req.Type)
	}
	if req.ProtocolVersion != ProtocolVersion {
		t.Errorf("ProtocolVersion = %s, want %s", req.ProtocolVersion, ProtocolVersion)
	}
	if req.Counter == nil || *req.Counter != 5 {
		t.Errorf("Counter = %v, want 5", req.Counter)
	}
	if req.SessionID != "session" {
		t.Errorf("SessionID = %s, want session", req.SessionID)
	}
	if req.Ciphertext != "encrypted" {
		t.Errorf("Ciphertext = %s, want encrypted", req.Ciphertext)
	}
}

func TestSendResponseJSON(t *testing.T) {
	resp := SendResponse{Status: "ok"}
	data, _ := json.Marshal(resp)

	var decoded map[string]string
	json.Unmarshal(data, &decoded)

	if decoded["status"] != "ok" {
		t.Errorf("expected status=ok, got %s", decoded["status"])
	}
}
