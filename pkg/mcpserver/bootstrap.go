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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ghealth/pkg/auth"
	"ghealth/pkg/config"
)

// Environment variables that seed the config directory on a headless host.
const (
	// EnvClientSecret carries the OAuth client_secret.json contents.
	EnvClientSecret = "GHEALTH_CLIENT_SECRET_JSON"
	// EnvCredentials carries the output of 'ghealth auth export'.
	EnvCredentials = "GHEALTH_CREDENTIALS_JSON"
)

// BootstrapCredentials writes credentials supplied through the environment into
// the config directory, so a container with no interactive login and no
// persistent disk can still authenticate.
//
// Both variables accept either raw JSON or standard base64 of that JSON, which
// keeps them pasteable into hosting dashboards that mangle multi-line values.
// Existing files are left alone: a mounted volume or a real login wins over the
// environment. It returns the names of the files it wrote.
//
// The refresh token needs the matching client_secret to be usable at all —
// oauth2 requires the client credentials to redeem it — so supplying one
// without the other is reported rather than left to fail later as a confusing
// auth error on the first tool call.
func BootstrapCredentials() ([]string, error) {
	rawSecret, hasSecret := lookupNonEmpty(EnvClientSecret)
	rawCreds, hasCreds := lookupNonEmpty(EnvCredentials)
	if !hasSecret && !hasCreds {
		return nil, nil
	}

	var written []string

	if hasSecret {
		decoded, err := decodeJSONEnv(EnvClientSecret, rawSecret)
		if err != nil {
			return nil, err
		}
		path := config.ClientSecretPath()
		if wrote, err := writeIfAbsent(path, decoded); err != nil {
			return nil, err
		} else if wrote {
			written = append(written, path)
		}
	}

	if hasCreds {
		decoded, err := decodeJSONEnv(EnvCredentials, rawCreds)
		if err != nil {
			return nil, err
		}
		var creds auth.StoredCredentials
		if err := json.Unmarshal(decoded, &creds); err != nil {
			return nil, fmt.Errorf("%s is not valid credentials JSON (%v) — it must be the output of 'ghealth auth export'", EnvCredentials, err)
		}
		if err := creds.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", EnvCredentials, err)
		}
		if creds.RefreshToken != "" && !auth.HasClientSecret() {
			return nil, fmt.Errorf(
				"%s carries a refresh_token but no OAuth client_secret is available to redeem it — set %s to the contents of your client_secret.json",
				EnvCredentials, EnvClientSecret)
		}
		path := config.CredentialsPath()
		if wrote, err := writeIfAbsent(path, decoded); err != nil {
			return nil, err
		} else if wrote {
			written = append(written, path)
		}
	}

	return written, nil
}

func lookupNonEmpty(name string) (string, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	return v, v != ""
}

// decodeJSONEnv accepts raw JSON or base64-encoded JSON and returns the JSON.
func decodeJSONEnv(name, value string) ([]byte, error) {
	if strings.HasPrefix(value, "{") {
		if !json.Valid([]byte(value)) {
			return nil, fmt.Errorf("%s is not valid JSON", name)
		}
		return []byte(value), nil
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("%s is neither JSON nor valid base64: %w", name, err)
	}
	if !json.Valid(decoded) {
		return nil, fmt.Errorf("%s decoded from base64 but the result is not valid JSON", name)
	}
	return decoded, nil
}

// writeIfAbsent creates path with owner-only permissions, reporting whether it
// wrote. An existing file is never overwritten — these are credentials, and a
// stale environment variable must not clobber a refresh token the CLI rotated.
func writeIfAbsent(path string, data []byte) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false, fmt.Errorf("cannot create the config directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return false, fmt.Errorf("cannot write %s: %w", path, err)
	}
	return true, nil
}
