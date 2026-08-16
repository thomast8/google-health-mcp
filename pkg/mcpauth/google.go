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

package mcpauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"ghealth/pkg/auth"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// GoogleConfig is the server's own OAuth client at Google — one client shared by
// every MCP client that registers here. That sharing is why the server shows its
// own consent screen before federating: see Provider.handleAuthorize.
type GoogleConfig struct {
	ClientID     string
	ClientSecret string
	// Scopes requested from the user. Defaults to DefaultScopes.
	Scopes []string
	// Endpoint is overridable so tests can point at a stub Google.
	Endpoint oauth2.Endpoint
	// TokenInfo resolves an access token to its account. Overridable for tests.
	TokenInfo func(ctx context.Context, accessToken string) (*auth.TokenInfo, error)
}

// DefaultScopes covers read access to every health category the CLI can query,
// plus the identity scopes needed to name the connected account.
//
// Requesting the read/write scopes would be pointless here — the MCP surface is
// read-only — and asking for more than is used makes Google's review harder and
// the consent screen more alarming than it needs to be.
func DefaultScopes() []string {
	scopes := []string{"openid", "email"}
	for _, s := range auth.AllScopes {
		if strings.HasSuffix(s.Suffix, ".readonly") {
			scopes = append(scopes, auth.FullScope(s.Suffix))
		}
	}
	return scopes
}

func (g *GoogleConfig) scopes() []string {
	if len(g.Scopes) > 0 {
		return g.Scopes
	}
	return DefaultScopes()
}

func (g *GoogleConfig) endpoint() oauth2.Endpoint {
	if g.Endpoint.AuthURL != "" {
		return g.Endpoint
	}
	return google.Endpoint
}

func (g *GoogleConfig) oauth(redirectURL string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     g.ClientID,
		ClientSecret: g.ClientSecret,
		Endpoint:     g.endpoint(),
		RedirectURL:  redirectURL,
		Scopes:       g.scopes(),
	}
}

// authCodeURL builds the Google consent URL.
//
// access_type=offline plus prompt=consent is what guarantees a refresh token on
// every authorization. Without prompt=consent Google omits it for an account
// that has already granted these scopes, and the session would die an hour
// later with no way to renew it.
func (g *GoogleConfig) authCodeURL(redirectURL, state string) string {
	return g.oauth(redirectURL).AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.SetAuthURLParam("include_granted_scopes", "true"),
	)
}

func (g *GoogleConfig) exchange(ctx context.Context, redirectURL, code string) (*oauth2.Token, error) {
	tok, err := g.oauth(redirectURL).Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("Google rejected the authorization code: %w", err)
	}
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("Google returned no refresh token, so the connection could not be kept alive; " +
			"remove this app at https://myaccount.google.com/permissions and connect again")
	}
	return tok, nil
}

func (g *GoogleConfig) refresh(ctx context.Context, refreshToken string) (*oauth2.Token, error) {
	cfg := g.oauth("")
	tok, err := cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		return nil, fmt.Errorf("Google refused to refresh the connection (it may have been revoked): %w", err)
	}
	return tok, nil
}

// accountEmail names the authorizing account, for the consent screen and the
// auth_status tool. A failure here is not fatal — it is a label, not a
// permission — so the caller falls back to an empty string.
func (g *GoogleConfig) accountEmail(ctx context.Context, accessToken string) string {
	lookup := g.TokenInfo
	if lookup == nil {
		lookup = auth.ValidateAccessToken
	}
	info, err := lookup(ctx, accessToken)
	if err != nil || info == nil {
		return ""
	}
	return info.Email
}

// tokenCache keeps live Google access tokens in memory, keyed by a hash of the
// refresh token that produced them.
//
// The session credential the MCP client holds carries only the refresh token,
// which keeps it small and lets the access token rotate underneath it. Without
// this cache every tool call would spend a round trip at Google's token
// endpoint refreshing a token that is still perfectly valid.
type tokenCache struct {
	mu     sync.Mutex
	tokens map[string]cachedToken
}

type cachedToken struct {
	accessToken string
	expiry      time.Time
}

// refreshSkew renews a token slightly before it expires, so a call that takes a
// moment to reach Google does not arrive with a just-expired token.
const refreshSkew = 2 * time.Minute

func newTokenCache() *tokenCache {
	return &tokenCache{tokens: map[string]cachedToken{}}
}

func cacheKey(refreshToken string) string {
	sum := sha256.Sum256([]byte(refreshToken))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (c *tokenCache) get(refreshToken string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.tokens[cacheKey(refreshToken)]
	if !ok || time.Now().Add(refreshSkew).After(entry.expiry) {
		return "", false
	}
	return entry.accessToken, true
}

func (c *tokenCache) put(refreshToken, accessToken string, expiry time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A token with no stated expiry is treated as short-lived rather than
	// eternal; guessing long here would cache a dead token.
	if expiry.IsZero() {
		expiry = time.Now().Add(30 * time.Minute)
	}
	c.tokens[cacheKey(refreshToken)] = cachedToken{accessToken: accessToken, expiry: expiry}
}

// accessTokenFor returns a live Google access token for a session, refreshing
// through Google only when the cached one is missing or close to expiry.
func (p *Provider) accessTokenFor(ctx context.Context, refreshToken string) (string, error) {
	if tok, ok := p.tokens.get(refreshToken); ok {
		return tok, nil
	}
	tok, err := p.google.refresh(ctx, refreshToken)
	if err != nil {
		return "", err
	}
	p.tokens.put(refreshToken, tok.AccessToken, tok.Expiry)
	return tok.AccessToken, nil
}

// replayGuard enforces one-time use of authorization codes.
//
// A sealed code cannot record its own redemption, so redemptions are remembered
// here for as long as a code could still be valid. This holds within one
// process; codes also carry a short TTL, which is what limits the exposure if a
// deployment ever runs more than one replica.
type replayGuard struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newReplayGuard() *replayGuard {
	return &replayGuard{seen: map[string]time.Time{}}
}

// claim records a code as redeemed, reporting false if it already was.
func (r *replayGuard) claim(code string, ttl time.Duration) bool {
	key := cacheKey(code)
	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()
	for k, expiry := range r.seen {
		if now.After(expiry) {
			delete(r.seen, k)
		}
	}
	if _, used := r.seen[key]; used {
		return false
	}
	r.seen[key] = now.Add(ttl)
	return true
}
