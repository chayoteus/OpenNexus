package messenger

import (
	"time"
)

// Message types
const (
	ProtocolVersion = "0.1.0"

	MsgTypeHello    = "hello"
	MsgTypeHelloAck = "hello_ack"
	MsgTypeData     = "data"
	MsgTypeReset    = "reset"
)

// Message represents a message between agents
type Message struct {
	ID                  string    `json:"id"`
	ProtocolVersion     string    `json:"protocol_version"`
	Type                string    `json:"type"` // hello, hello_ack, data, reset
	SenderID            string    `json:"sender_id"`
	ReceiverID          string    `json:"receiver_id"`
	SenderPublicKey     string    `json:"sender_public_key,omitempty"`
	EphemeralPubKey     string    `json:"ephemeral_public_key,omitempty"`
	PeerEphemeralPubKey string    `json:"peer_ephemeral_public_key,omitempty"`
	Signature           string    `json:"signature,omitempty"`
	SenderMessengerURL  string    `json:"sender_messenger_url,omitempty"` // Sender's messenger URL (for reply routing)
	SessionID           string    `json:"session_id,omitempty"`
	Counter             *uint64   `json:"counter,omitempty"`
	Ciphertext          string    `json:"ciphertext,omitempty"`
	ResetSignature      string    `json:"reset_signature,omitempty"` // For signed RESET fallback
	Reason              *int      `json:"reason,omitempty"`          // For RESET reason_enum
	CreatedAt           time.Time `json:"created_at"`
}

// SendRequest is the request body for sending a message
type SendRequest struct {
	ProtocolVersion     string  `json:"protocol_version" binding:"required"`
	Type                string  `json:"type" binding:"required"`
	SenderID            string  `json:"sender_id" binding:"required"`
	ReceiverID          string  `json:"receiver_id" binding:"required"`
	SenderPublicKey     string  `json:"sender_public_key"`
	EphemeralPubKey     string  `json:"ephemeral_public_key"`
	PeerEphemeralPubKey string  `json:"peer_ephemeral_public_key"`
	Signature           string  `json:"signature"`
	SenderMessengerURL  string  `json:"sender_messenger_url"` // Sender's messenger URL
	SessionID           string  `json:"session_id"`
	Counter             *uint64 `json:"counter"`
	Ciphertext          string  `json:"ciphertext"`
	ResetSignature      string  `json:"reset_signature"` // For RESET
	Reason              *int    `json:"reason"`          // For RESET reason_enum
}

// SendResponse is the response for sending a message
type SendResponse struct {
	Status string `json:"status"`
}
