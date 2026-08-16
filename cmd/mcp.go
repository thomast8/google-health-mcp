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
	"ghealth/pkg/mcpauth"
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

	// Google sign-in (multi-user mode).
	envMCPSecret          = "GHEALTH_MCP_SECRET"
	envGoogleClientID     = "GHEALTH_MCP_GOOGLE_CLIENT_ID"
	envGoogleClientSecret = "GHEALTH_MCP_GOOGLE_CLIENT_SECRET"
	envPublicURL          = "GHEALTH_MCP_PUBLIC_URL"
	envRailwayDomain      = "RAILWAY_PUBLIC_DOMAIN"
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
                   launches this process and speaks JSON-RPC over stdin/stdout. It
                   uses the credentials the CLI already has.
  --http           Streamable HTTP for a remote client, served at /mcp with a
                   /healthz probe alongside it.

Tools: list_data_types, describe_data_type, query_data, get_user_info, auth_status,
export_exercise_tcx. Writes are not exposed — use the data subcommands for those.

HTTP authentication — one of two modes, and the server will not start without one:

  Google sign-in (multi-user). Set GHEALTH_MCP_GOOGLE_CLIENT_ID,
  GHEALTH_MCP_GOOGLE_CLIENT_SECRET and GHEALTH_MCP_SECRET. Each user signs in with
  their own Google account and sees only their own data. This server then acts as
  the OAuth authorization server the MCP client registers with, so clients such as
  ChatGPT and Claude can connect with no shared secret. Also set
  GHEALTH_MCP_PUBLIC_URL to the deployment's public origin, and register
  <public-url>/oauth/callback as an authorized redirect URI on the Google client.

  Shared token (single account). Set GHEALTH_MCP_TOKEN. Every caller presenting it
  reads the one account this server is authenticated as, using the CLI's own
  credentials. Suitable for a private deployment, not for sharing.

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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// stdio serves whoever launched the process, using the credentials the CLI
	// already has. There is no second party to authenticate.
	if !mcpHTTP && !envEnabled(envMCPHTTP) {
		logf("ghealth mcp: serving over stdio (config dir: %s)", config.ConfigDir())
		return mcpserver.ServeStdio(ctx, mcpserver.New(mcpserver.Options{
			Runner:  &mcpserver.ExecRunner{Timeout: timeout},
			Version: version.Full(),
		}))
	}

	opts, err := httpAuthOptions(logf)
	if err != nil {
		return err
	}

	runner := &mcpserver.ExecRunner{Timeout: timeout}
	if opts.OAuth != nil {
		// Multi-user: each call runs as the caller, and child processes get an
		// empty config directory so there are no stored credentials to fall
		// back to if a request somehow arrives without a session.
		empty, err := os.MkdirTemp("", "ghealth-mcp-tenant")
		if err != nil {
			return client.NewConfigError(fmt.Sprintf("cannot create the isolation directory: %v", err), "")
		}
		defer os.RemoveAll(empty)
		runner.PerRequestAuth = true
		runner.ConfigDir = empty
	}

	srv := mcpserver.New(mcpserver.Options{Runner: runner, Version: version.Full()})
	handler, err := mcpserver.Handler(srv, opts)
	if err != nil {
		return client.NewConfigError(err.Error(),
			"For Google sign-in set "+envGoogleClientID+", "+envGoogleClientSecret+" and "+envMCPSecret+
				"; for single-account access set "+envMCPToken)
	}

	addr := resolveAddr()
	logf("ghealth mcp: serving Streamable HTTP on %s%s (health: %s)", addr, mcpserver.MCPPath, mcpserver.HealthPath)
	if err := mcpserver.ServeHTTP(ctx, addr, handler); err != nil {
		return client.NewNetworkError(fmt.Sprintf("MCP HTTP server failed: %v", err))
	}
	logf("ghealth mcp: shut down")
	return nil
}

// httpAuthOptions decides how callers authenticate.
//
// Google sign-in wins when it is configured, because it is the mode that serves
// more than one person; the shared token remains for a private, single-account
// deployment. Configuring neither is an error rather than a default, and a
// half-configured Google client is reported rather than silently ignored — a
// deployment that quietly fell back to a shared token would expose the
// operator's own health record to everyone holding it.
func httpAuthOptions(logf func(string, ...any)) (mcpserver.HTTPOptions, error) {
	clientID := strings.TrimSpace(os.Getenv(envGoogleClientID))
	clientSecret := strings.TrimSpace(os.Getenv(envGoogleClientSecret))
	secret := strings.TrimSpace(os.Getenv(envMCPSecret))
	token := strings.TrimSpace(os.Getenv(envMCPToken))

	if clientID == "" && clientSecret == "" && secret == "" {
		if token == "" {
			return mcpserver.HTTPOptions{}, nil // Handler reports the failure.
		}
		logf("ghealth mcp: single-account mode — every caller presenting the shared token sees this server's own health data")
		return mcpserver.HTTPOptions{Token: token}, nil
	}

	var missing []string
	for _, v := range []struct{ name, value string }{
		{envGoogleClientID, clientID},
		{envGoogleClientSecret, clientSecret},
		{envMCPSecret, secret},
	} {
		if v.value == "" {
			missing = append(missing, v.name)
		}
	}
	if len(missing) > 0 {
		return mcpserver.HTTPOptions{}, client.NewConfigError(
			"Google sign-in is partly configured; missing: "+strings.Join(missing, ", "),
			"Set all three, or unset them all to fall back to "+envMCPToken)
	}

	provider, err := mcpauth.NewProvider(mcpauth.Config{
		Secret:    secret,
		PublicURL: publicURL(),
		MCPPath:   mcpserver.MCPPath,
		Google: mcpauth.GoogleConfig{
			ClientID:     clientID,
			ClientSecret: clientSecret,
		},
	})
	if err != nil {
		return mcpserver.HTTPOptions{}, client.NewConfigError(err.Error(), "")
	}

	if url := publicURL(); url != "" {
		logf("ghealth mcp: Google sign-in enabled — connector URL %s%s, redirect URI %s%s",
			url, mcpserver.MCPPath, url, mcpauth.CallbackPath)
	} else {
		logf("ghealth mcp: Google sign-in enabled — set %s so the OAuth URLs do not depend on forwarded headers", envPublicURL)
	}
	return mcpserver.HTTPOptions{OAuth: provider}, nil
}

// publicURL is the externally reachable origin. Railway supplies the domain but
// not the scheme, so it is completed here.
func publicURL() string {
	if explicit := strings.TrimSpace(os.Getenv(envPublicURL)); explicit != "" {
		return strings.TrimRight(explicit, "/")
	}
	if domain := strings.TrimSpace(os.Getenv(envRailwayDomain)); domain != "" {
		return "https://" + domain
	}
	return ""
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
