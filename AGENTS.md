# OpenNexus Development Guide

## For AI Agents

This document outlines how other AI agents can contribute to OpenNexus.

---

## First Message

If the user gives you a task related to OpenNexus:
1. Read this AGENTS.md
2. Understand the project structure
3. Ask clarifying questions if needed

---

## Project Structure

```
OpenNexus/
├── cmd/
│   └── messenger/        # Messenger server (Go)
├── clients/
│   └── python/           # Python client
├── internal/
│   └── messenger/        # Messenger handlers & logic
├── docs/
│   ├── API.md            # API reference
│   ├── PROTOCOL.md       # Protocol specification
├── README.md             # Main documentation
├── website/
│   └── skill.md          # How agents use OpenNexus
└── docker-compose.yml    # Local development
```

---

## Protocol Design

### Key Features

| Feature | Description |
|---------|-------------|
| **Stateless Handshake** | Fresh ephemeral keys per request, optional session key caching |
| **1-RTT Handshake** | HELLO → HELLO_ACK → DATA |
| **Forward Secrecy** | New ephemeral key per request |
| **Streaming Ready** | Chunked encryption with AAD |
| **RESET Recovery** | Automatic re-handshake on decryption failure |

### Message Types

- **HELLO**: Initiate handshake (type=hello)
- **HELLO_ACK**: Response (type=hello_ack)
- **DATA**: Encrypted data (type=data)
- **RESET**: Error recovery (type=reset)

---

## Development Workflow

### Prerequisites
- Go 1.21+
- Python 3.x
- Redis (for local development)

### Run Locally

```bash
# Start services
docker compose up -d

# Or run individually
REDIS_ADDR=localhost:6379 go run ./cmd/messenger    # Port 8080
```

### Code Quality
- Follow Go standard formatting (`go fmt`)
- Add comments for exported functions
- Keep functions small and focused
- Test changes before committing

### Testing

```bash
# Run all Go tests
go test -v -race -cover ./...

# Run a specific test
go test -v -run TestSendMessageValidHello ./internal/messenger/...

# End-to-end test (requires running server)
./scripts/test.sh
```

---

## Key Cryptographic Concepts

### Identity
- Ed25519 public key = Agent ID
- Private key stays local

### Handshake (1-RTT)
```
1. Alice generates X25519 ephemeral key
2. Alice sends HELLO (signs ephemeral_pub)
3. Bob verifies, generates his ephemeral key
4. Bob sends HELLO_ACK (signs both ephemeral keys)
5. Both derive directional write keys + session_id via ECDH + transcript-bound HKDF
```

### Encryption
- **Key Exchange**: X25519 ECDH
- **Key Derivation**: HKDF (SHA-256)
- **Encryption**: AES-256-GCM with AAD

---

## Making Changes

### For External Contributors

1. Create an issue describing the change
2. Fork the repository
3. Make your changes in a feature branch
4. Test locally
5. Commit with clear message
6. Create a Pull Request

### For Maintainers

1. Create an issue
2. Make changes in a feature branch
3. Test locally
4. Commit and push to main

### Documentation Changes
- Keep README.md up to date
- Update website/skill.md if API changes
- Update docs/PROTOCOL.md if protocol changes
- Use English only

---

## Commands

```bash
# Run messenger (needs Redis)
REDIS_ADDR=localhost:6379 go run ./cmd/messenger

# Run with custom rate limits (default: 100 req/s, burst 200)
RATE_LIMIT=50 RATE_BURST=100 REDIS_ADDR=localhost:6379 go run ./cmd/messenger

# Disable rate limiting
RATE_LIMIT=0 REDIS_ADDR=localhost:6379 go run ./cmd/messenger

# Test Python client
cd clients/python
python opennexus.py --help
```

---

## Style

- Keep answers concise
- No emojis in commits
- Technical prose only
- Be direct and kind

---

## Questions?

If unsure about something, ask before making assumptions.
