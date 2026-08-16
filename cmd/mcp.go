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

package cmd

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ghealth/internal/version"
	"ghealth/pkg/client"
	"ghealth/pkg/config"
	"ghealth/pkg/mcpserver"

	"github.com/spf13/cobra"
)

// Environment variables that configure the MCP server.
const (
	envMCPHTTP    = "GHEALTH_MCP_HTTP"
	envMCPToken   = "GHEALTH_MCP_TOKEN"
	envMCPTimeout = "GHEALTH_MCP_TIMEOUT"
	envHost       = "HOST"
	envPort       = "PORT"
)

const (
	defaultHost = "0.0.0.0"
	defaultPort = "8000"
)

var (
	mcpHTTP bool
	mcpAddr string
)

var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Run the Model Context Protocol server",
	Long: `Serve ghealth's read-only data access to an MCP client.

Transports:
  stdio (default)  For a local client such as Claude Desktop or Claude Code, which
                   launches this process and speaks JSON-RPC over stdin/stdout.
  --http           Streamable HTTP for a remote client, served at /mcp with a
                   /healthz probe alongside it. Requires GHEALTH_MCP_TOKEN; the
                   client must send it as 'Authorization: Bearer <token>'.

Tools: list_data_types, describe_data_type, query_data, get_user_info, auth_status,
export_exercise_tcx. Writes are not exposed — use the data subcommands for those.

Authentication is the CLI's: stored credentials from 'ghealth auth login', or the
GHEALTH_ACCESS_TOKEN / GHEALTH_CREDENTIALS_FILE environment variables. On a host with
no interactive login, set GHEALTH_CLIENT_SECRET_JSON and GHEALTH_CREDENTIALS_JSON
(each raw JSON or base64) and the server writes them into the config directory at
startup.

Examples:
  ghealth mcp                          # stdio, for a local client
  ghealth mcp --http                   # Streamable HTTP on $HOST:$PORT (default 0.0.0.0:8000)
  ghealth mcp --http --addr :9000      # Streamable HTTP on an explicit address`,
	Args: cobra.NoArgs,
	RunE: runMCP,
}

func init() {
	rootCmd.AddCommand(mcpCmd)
	mcpCmd.Flags().BoolVar(&mcpHTTP, "http", false,
		"Serve Streamable HTTP at /mcp instead of stdio (also enabled by "+envMCPHTTP+"=1)")
	mcpCmd.Flags().StringVar(&mcpAddr, "addr", "",
		"Listen address for --http (default: $HOST:$PORT, else "+defaultHost+":"+defaultPort+")")
}

func runMCP(cmd *cobra.Command, args []string) error {
	// Every diagnostic goes to stderr: in stdio mode stdout carries the
	// JSON-RPC stream, and a stray line there corrupts the session.
	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", a...)
	}

	written, err := mcpserver.BootstrapCredentials()
	if err != nil {
		return client.NewConfigError(err.Error(), "Check the credential environment variables against 'ghealth auth export' output")
	}
	for _, path := range written {
		logf("wrote %s from the environment", path)
	}

	timeout, err := mcpTimeout()
	if err != nil {
		return client.NewValidationError(err.Error(), "Use a Go duration such as 60s or 3m")
	}

	srv := mcpserver.New(mcpserver.Options{
		Runner:  &mcpserver.ExecRunner{Timeout: timeout},
		Version: version.Full(),
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if !mcpHTTP && !envEnabled(envMCPHTTP) {
		logf("ghealth mcp: serving over stdio (config dir: %s)", config.ConfigDir())
		return mcpserver.ServeStdio(ctx, srv)
	}

	handler, err := mcpserver.Handler(srv, os.Getenv(envMCPToken))
	if err != nil {
		return client.NewConfigError(err.Error(),
			"Generate one with 'openssl rand -hex 32' and set it on both the server and the client")
	}

	addr := resolveAddr()
	logf("ghealth mcp: serving Streamable HTTP on %s%s (health: %s)", addr, mcpserver.MCPPath, mcpserver.HealthPath)
	if err := mcpserver.ServeHTTP(ctx, addr, handler); err != nil {
		return client.NewNetworkError(fmt.Sprintf("MCP HTTP server failed: %v", err))
	}
	logf("ghealth mcp: shut down")
	return nil
}

// resolveAddr picks the listen address: the --addr flag, else $HOST:$PORT as
// container hosts such as Railway, Fly and Cloud Run provide them.
func resolveAddr() string {
	if mcpAddr != "" {
		return mcpAddr
	}
	host := os.Getenv(envHost)
	if host == "" {
		host = defaultHost
	}
	port := os.Getenv(envPort)
	if port == "" {
		port = defaultPort
	}
	return net.JoinHostPort(host, port)
}

// mcpTimeout reads the per-tool-call timeout, defaulting to the package value.
func mcpTimeout() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(envMCPTimeout))
	if raw == "" {
		return mcpserver.DefaultTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s value %q: %v", envMCPTimeout, raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", envMCPTimeout, raw)
	}
	return d, nil
}

// envEnabled reports whether a boolean-ish environment variable is switched on.
func envEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
