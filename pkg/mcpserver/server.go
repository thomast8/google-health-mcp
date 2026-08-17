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

package mcpserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ghealth/pkg/mcpauth"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPPath is the endpoint the Streamable HTTP transport is served from. A
// remote client's connector URL is https://<host>/mcp.
const MCPPath = "/mcp"

// HealthPath is an unauthenticated liveness endpoint for the container host.
const HealthPath = "/healthz"

// Instructions tells the client what this server is for and how to approach it.
//
// It mirrors the CLI's progressive-disclosure design — discover the type, read
// its schema if needed, then query — and front-loads the handful of things an
// agent cannot work out from the tool schemas alone, because getting them wrong
// produces an answer that looks right: aggregating over UTC instead of the
// user's local days, reading a missing day as a zero, or mistaking a
// limit-capped page for a whole range.
const Instructions = `This server reads one person's health record from the Google Health API:
steps, distance, active minutes and exercise sessions; sleep with its stages; heart rate, HRV,
SpO2, ECG and blood pressure; weight, body fat and other body measurements; nutrition and
hydration. 40 data types in all, for the Google account this connection is signed in as.

# How to answer a question

1. list_data_types — find the type ID. IDs are hyphenated and not always guessable, and this
   costs no API call.
2. describe_data_type — only when you need the type's fields, units or filter template.
3. query_data — the read itself.

auth_status and get_user_info are for context, not for measurements: whose account this is, what
scopes were granted, the user's height and age, which devices are paired.

# Choosing the operation

daily-rollup for totals and averages per day, list for individual timestamped readings, rollup for
fixed windows other than a day, get for one point by id, reconcile for what changed since a sync.

Reach for daily-rollup before summing a list yourself. It aggregates in the user's local days;
adding up points by their UTC timestamps quietly misattributes everything either side of midnight,
and it returns one row per day where a list of a sampled type returns thousands.

# Things that mislead

- Missing days are absent, not zero. A day with no recorded steps has no row at all. Never read an
  absent day as a zero, and never average across days the record does not contain.
- An empty dataPoints array is a real answer. It means nothing was recorded in that range — not
  that the request was malformed.
- Rollup ranges are capped by the API: 14 days for heart-rate, total-calories, active-minutes and
  calories-in-heart-rate-zone, 90 days for everything else. Split a longer question into several
  calls rather than widening the range.
- A nextPageToken means the result is partial. Pass it back as page_token, with every other
  argument unchanged, before drawing a conclusion from what you have.
- Timestamps carry the user's UTC offset (2026-08-17T07:12:04+01:00). Report times as the user
  experienced them; do not convert to UTC in your answer.

# Responses

Simplified JSON in one stable envelope: a dataPoints array, plus nextPageToken when more data is
available and _hints when the server has a next step worth suggesting. Measurements come back as
JSON numbers, named as the Health API names them — rollup and daily-rollup suffix each field with
the aggregation applied, so a step count becomes countSum and a heart rate becomes
beatsPerMinuteAvg alongside beatsPerMinuteMin and beatsPerMinuteMax.

This server is read-only: it cannot create, update or delete health data. This is a medical
record, not a metrics dashboard — report what it says, and leave diagnosis to a clinician.`

// Options configures a server instance.
type Options struct {
	// Runner executes the underlying CLI. Nil means a default ExecRunner that
	// re-invokes this process's own binary.
	Runner Runner
	// Version is reported to clients as the server version.
	Version string
}

// New builds an MCP server with every tool registered.
func New(opts Options) *mcp.Server {
	runner := opts.Runner
	if runner == nil {
		runner = &ExecRunner{}
	}
	version := opts.Version
	if version == "" {
		version = "dev"
	}

	s := mcp.NewServer(&mcp.Implementation{
		Name:    "ghealth",
		Title:   "Google Health",
		Version: version,
	}, &mcp.ServerOptions{
		Instructions: Instructions,
	})
	AddTools(s, runner)
	return s
}

// ServeStdio runs the server over stdin/stdout for a local MCP client such as
// Claude Desktop or Claude Code. It blocks until the client disconnects or ctx
// is cancelled.
func ServeStdio(ctx context.Context, s *mcp.Server) error {
	return s.Run(ctx, &mcp.StdioTransport{})
}

// ErrNoAuth is returned when HTTP mode is requested with no way to authenticate
// callers.
//
// The server exposes personal health records, so it fails closed rather than
// starting an unauthenticated listener: a Railway or Fly deployment gets a
// public HTTPS URL the moment it boots, and an unguarded /mcp there is an open
// door to those records.
var ErrNoAuth = errors.New("refusing to serve MCP over HTTP without authentication: configure Google sign-in, or set GHEALTH_MCP_TOKEN for single-account access")

// HTTPOptions selects how callers authenticate. Exactly one mode applies.
type HTTPOptions struct {
	// OAuth serves multiple users, each signing in with their own Google
	// account. Takes precedence over Token when both are set.
	OAuth *mcpauth.Provider
	// Token is a shared secret granting access to the one Google account the
	// server itself is authenticated as.
	Token string
}

// Handler builds the HTTP handler: the MCP endpoint behind authentication, the
// OAuth endpoints when Google sign-in is enabled, and an unauthenticated health
// check for the container host's probes.
func Handler(s *mcp.Server, opts HTTPOptions) (http.Handler, error) {
	if opts.OAuth == nil && strings.TrimSpace(opts.Token) == "" {
		return nil, ErrNoAuth
	}

	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s },
		// Stateless: every request stands alone, so the server survives a
		// restart or a second replica without stranding client sessions.
		&mcp.StreamableHTTPOptions{Stateless: true},
	)

	mux := http.NewServeMux()

	var guard func(http.Handler) http.Handler
	if opts.OAuth != nil {
		opts.OAuth.Register(mux)
		guard = opts.OAuth.Middleware
	} else {
		guard = func(next http.Handler) http.Handler { return requireBearer(opts.Token, next) }
	}
	mux.Handle(MCPPath, guard(streamable))
	mux.Handle(MCPPath+"/", guard(streamable))

	mux.HandleFunc(HealthPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"status":"ok"}`)
	})

	// Reject cross-site browser requests. This guards unsafe methods only, so
	// the OAuth browser flow is unaffected — /oauth/authorize and the Google
	// callback are cross-site GET navigations, which are always allowed.
	//
	// It matters most for /oauth/consent. That form is what stands between a
	// user and a client they never chose, and it is submitted from this
	// server's own page; a POST auto-submitted by an attacker's page arrives
	// marked cross-site and is refused here.
	return http.NewCrossOriginProtection().Handler(mux), nil
}

// requireBearer enforces `Authorization: Bearer <token>`, answering a rejection
// the way the MCP spec expects so a connector prompts for credentials instead
// of reporting an opaque failure.
func requireBearer(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ghealth"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ServeHTTP runs the HTTP server until ctx is cancelled, then shuts it down
// gracefully so in-flight tool calls can finish.
func ServeHTTP(ctx context.Context, addr string, handler http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
