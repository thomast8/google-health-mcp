// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package mcpauth turns the MCP server into an OAuth 2.1 authorization server
// that federates to Google, so each user authorizes with their own Google
// account and sees only their own health data.
//
// Google cannot serve as the MCP authorization server directly: the MCP spec
// expects Dynamic Client Registration (RFC 7591) and resource indicators
// (RFC 8707), and Google supports neither. So this package is the
// authorization server the MCP client talks to, and Google is an upstream
// identity provider it federates to.
//
// # Why there is no database
//
// Every credential this package issues — client IDs, authorization codes,
// access and refresh tokens — is an authenticated-encrypted blob holding its
// own state, sealed with a server-side key. Nothing is stored, so the server
// keeps working across an ephemeral container's redeploys with no volume and
// no database to provision. The trade-offs are deliberate:
//
//   - Revocation is by expiry, or by rotating GHEALTH_MCP_SECRET, which
//     invalidates every outstanding credential at once.
//   - One-time use of authorization codes cannot be proven statelessly, so a
//     short TTL and an in-memory replay guard cover it instead.
package mcpauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// MinSecretLength is the shortest GHEALTH_MCP_SECRET accepted. Every credential
// the server issues is only as strong as this value.
const MinSecretLength = 32

// Sealed credential kinds. The kind is authenticated as associated data, so a
// value sealed as one kind can never be opened as another — an access token
// cannot be replayed as an authorization code, for instance.
const (
	kindClient  = "client"
	kindPending = "pending"
	kindCode    = "code"
	kindAccess  = "access"
	kindRefresh = "refresh"
)

// sealer seals and opens the server's credentials with AES-256-GCM.
type sealer struct {
	aead cipher.AEAD
}

func newSealer(secret string) (*sealer, error) {
	if len(secret) < MinSecretLength {
		return nil, fmt.Errorf("the signing secret must be at least %d characters (got %d) — generate one with 'openssl rand -hex 32'",
			MinSecretLength, len(secret))
	}
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("cannot build the cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cannot build the AEAD: %w", err)
	}
	return &sealer{aead: aead}, nil
}

// seal encodes v as a URL-safe opaque credential of the given kind.
func (s *sealer) seal(kind string, v any) (string, error) {
	plain, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("cannot encode the %s payload: %w", kind, err)
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("cannot generate a nonce: %w", err)
	}
	sealed := s.aead.Seal(nonce, nonce, plain, []byte(kind))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// open decodes a credential into v, failing if it was tampered with, sealed
// with a different key, or sealed as a different kind.
func (s *sealer) open(kind, token string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return fmt.Errorf("malformed %s", kind)
	}
	if len(raw) < s.aead.NonceSize() {
		return fmt.Errorf("malformed %s", kind)
	}
	nonce, body := raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():]
	plain, err := s.aead.Open(nil, nonce, body, []byte(kind))
	if err != nil {
		return fmt.Errorf("invalid %s", kind)
	}
	if err := json.Unmarshal(plain, v); err != nil {
		return fmt.Errorf("invalid %s", kind)
	}
	return nil
}

// randomToken returns a URL-safe random string of n bytes of entropy.
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
