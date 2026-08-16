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

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPPath is the endpoint the Streamable HTTP transport is served from. A
// remote client's connector URL is https://<host>/mcp.
const MCPPath = "/mcp"

// HealthPath is an unauthenticated liveness endpoint for the container host.
const HealthPath = "/healthz"

// Instructions tells the client what this server is for and how to approach it.
// It mirrors the CLI's progressive-disclosure design: discover the type, read
// its schema if needed, then query — and reach for daily-rollup rather than
// adding up a list.
const Instructions = `Personal health data from the Google Health API: steps, heart rate, sleep,
exercise, weight, SpO2, HRV, ECG, blood glucose, nutrition and more, for the single account this
server is authenticated as.

Start with list_data_types to find the type ID for a question, then query_data to read it. Use
describe_data_type when you need the fields or parameters of a type you have not used before.

For daily totals and averages use the daily-rollup operation rather than listing points and
summing them yourself — the API aggregates in the user's local days, which a naive sum over UTC
timestamps gets wrong. Rollup ranges are capped by the API (14 days for heart-rate, total-calories,
active-minutes and calories-in-heart-rate-zone; 90 days for everything else), so split longer
questions into several calls.

Responses are simplified JSON. A '_hints' array, when present, suggests the next call. A
'nextPageToken' means more data is available — pass it back as page_token to continue.

This server is read-only: it cannot create, update or delete health data.`

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

// ErrNoToken is returned when HTTP mode is requested without a bearer token.
//
// The server exposes one person's health record, so it fails closed rather than
// starting an unauthenticated listener: a Railway or Fly deployment gets a
// public HTTPS URL the moment it boots, and an unguarded /mcp there is an open
// door to that record.
var ErrNoToken = errors.New("refusing to serve MCP over HTTP without a bearer token: set GHEALTH_MCP_TOKEN to a long random secret")

// Handler builds the HTTP handler: the MCP endpoint behind bearer auth, plus an
// unauthenticated health check for the container host's probes.
func Handler(s *mcp.Server, token string) (http.Handler, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrNoToken
	}

	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s },
		// Stateless: every request stands alone, so the server survives a
		// restart or a second replica without stranding client sessions.
		&mcp.StreamableHTTPOptions{Stateless: true},
	)

	mux := http.NewServeMux()
	mux.Handle(MCPPath, requireBearer(token, streamable))
	mux.Handle(MCPPath+"/", requireBearer(token, streamable))
	mux.HandleFunc(HealthPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"status":"ok"}`)
	})

	// Reject cross-site browser requests. The bearer token already prevents a
	// drive-by call from succeeding, but this refuses them before they reach
	// the MCP machinery at all.
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
