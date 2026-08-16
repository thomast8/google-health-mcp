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
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"ghealth/pkg/types"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// readOnlyOperations are the data operations the MCP server will run.
//
// create, update and delete are deliberately absent. The server is built to be
// reachable over the network, where the blast radius of a mistaken or hostile
// call is someone's health record; the CLI remains the way to write. export-tcx
// is also absent here because it has its own tool.
var readOnlyOperations = map[string]bool{
	"list":         true,
	"get":          true,
	"rollup":       true,
	"daily-rollup": true,
	"reconcile":    true,
}

// userResources maps a get_user_info resource name onto its CLI subcommand.
var userResources = map[string][]string{
	"identity":       {"user", "identity"},
	"profile":        {"user", "profile", "get"},
	"settings":       {"user", "settings", "get"},
	"irn-profile":    {"user", "irn-profile"},
	"paired-devices": {"user", "paired-devices", "list"},
}

// AddTools registers every tool on the server. Handlers translate arguments
// into a ghealth command line and hand back its stdout unchanged, so a tool
// result is exactly what the equivalent CLI command prints — including the
// _hints entries that suggest the next call.
func AddTools(s *mcp.Server, r Runner) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  "list_data_types",
		Title: "List health data types",
		Description: "List the Google Health data types this server can read, with the " +
			"operations each one supports. Start here when you do not already know the " +
			"type ID for a health question. Optionally filter by category.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, listDataTypes)

	mcp.AddTool(s, &mcp.Tool{
		Name:  "describe_data_type",
		Title: "Describe a health data type",
		Description: "Show the fields, operation parameters, filter template and OAuth scope " +
			"for one data type. Call this before query_data when you need to know which " +
			"fields a type returns or which parameters an operation accepts.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, describeDataType(r))

	mcp.AddTool(s, &mcp.Tool{
		Name:  "query_data",
		Title: "Query health data",
		Description: "Read health data for one type. Operations: 'list' returns individual " +
			"data points; 'rollup' aggregates into fixed windows; 'daily-rollup' aggregates " +
			"per day and is the right choice for daily totals such as steps; 'get' fetches one " +
			"data point by id; 'reconcile' reports what changed. Prefer daily-rollup over " +
			"summing a list yourself. Responses are simplified JSON and may carry a '_hints' " +
			"array suggesting the next call, plus 'nextPageToken' when more data is available.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, queryData(r))

	mcp.AddTool(s, &mcp.Tool{
		Name:  "get_user_info",
		Title: "Get user profile and devices",
		Description: "Read account-level information: the user's identity, profile " +
			"(height, birthdate, sex), app settings, irregular-rhythm-notification profile, " +
			"or the list of paired devices.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, getUserInfo(r))

	mcp.AddTool(s, &mcp.Tool{
		Name:  "auth_status",
		Title: "Check authentication status",
		Description: "Report which credentials the server is using, the authenticated account, " +
			"the granted OAuth scopes and the token expiry. Call this first when a data query " +
			"fails with an authentication or permission error.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, authStatus(r))

	mcp.AddTool(s, &mcp.Tool{
		Name:  "export_exercise_tcx",
		Title: "Export an exercise track",
		Description: "Export one exercise session's GPS/sensor track, either as raw TCX XML or " +
			"as a trackpoint CSV (time, lap, position, altitude, distance, heart rate). Find the " +
			"exercise id first with query_data on data_type 'exercise'. Indoor sessions with no " +
			"track return a header-only CSV — their summary is in the exercise data point itself.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, exportExerciseTCX(r))
}

// ─── list_data_types ─────────────────────────────────────────────

type listDataTypesInput struct {
	Category string `json:"category,omitempty" jsonschema:"Only return types in this category, e.g. activity_and_fitness, sleep, nutrition, health_metrics_and_measurements. Omit to list every type."`
}

// listDataTypes answers from the in-process registry. The registry is the
// authoritative source for type metadata — 'ghealth schema types' builds its
// output from the same map — so there is nothing to gain from a subprocess,
// and answering directly lets the tool support a category filter the CLI has
// no flag for.
func listDataTypes(_ context.Context, _ *mcp.CallToolRequest, in listDataTypesInput) (*mcp.CallToolResult, any, error) {
	category := strings.TrimSpace(in.Category)
	if category != "" && !knownCategory(category) {
		return nil, nil, fmt.Errorf("unknown category %q — known categories are: %s",
			category, strings.Join(allCategories(), ", "))
	}

	list := make([]map[string]any, 0, len(types.Registry))
	for _, id := range types.IDs() {
		dt := types.Get(id)
		if category != "" && dt.Category != category {
			continue
		}
		list = append(list, map[string]any{
			"id":          dt.ID,
			"category":    dt.Category,
			"description": dt.Description,
			"operations":  dt.Operations,
			"writable":    dt.Writable,
			"rollupOnly":  dt.RollupOnly,
		})
	}

	payload := map[string]any{
		"count":     len(list),
		"dataTypes": list,
	}
	if category != "" {
		payload["category"] = category
	}
	return jsonResult(payload)
}

func allCategories() []string {
	seen := map[string]bool{}
	var out []string
	for _, dt := range types.Registry {
		if !seen[dt.Category] {
			seen[dt.Category] = true
			out = append(out, dt.Category)
		}
	}
	sort.Strings(out)
	return out
}

func knownCategory(c string) bool {
	for _, dt := range types.Registry {
		if dt.Category == c {
			return true
		}
	}
	return false
}

// ─── describe_data_type ──────────────────────────────────────────

type describeDataTypeInput struct {
	DataType string `json:"data_type" jsonschema:"The data type ID, e.g. steps, heart-rate, sleep, weight. Use list_data_types to discover valid IDs."`
}

func describeDataType(r Runner) mcp.ToolHandlerFor[describeDataTypeInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in describeDataTypeInput) (*mcp.CallToolResult, any, error) {
		id, err := resolveDataType(in.DataType)
		if err != nil {
			return nil, nil, err
		}
		out, err := r.Run(ctx, "schema", "type", id, "--format", "json")
		if err != nil {
			return nil, nil, err
		}
		return textResult(out)
	}
}

// ─── query_data ──────────────────────────────────────────────────

type queryDataInput struct {
	DataType   string `json:"data_type" jsonschema:"The data type ID, e.g. steps, heart-rate, sleep, weight. Use list_data_types to discover valid IDs."`
	Operation  string `json:"operation" jsonschema:"One of list, get, rollup, daily-rollup, reconcile. Must be an operation the type supports — list_data_types reports which."`
	From       string `json:"from,omitempty" jsonschema:"Start of the range: YYYY-MM-DD, an ISO 8601 timestamp, 'today' or 'yesterday'. Required for rollup and daily-rollup."`
	To         string `json:"to,omitempty" jsonschema:"End of the range, inclusive of the named day: YYYY-MM-DD, an ISO 8601 timestamp, 'today' or 'yesterday'. Required for rollup and daily-rollup."`
	Limit      int    `json:"limit,omitempty" jsonschema:"list only: maximum data points to return across all pages. Defaults to 500."`
	PageToken  string `json:"page_token,omitempty" jsonschema:"list only: resume from a previous response's nextPageToken to fetch the following page."`
	Filter     string `json:"filter,omitempty" jsonschema:"list and reconcile only: a raw API filter expression, which overrides from/to. Use describe_data_type for the filter template."`
	WindowSize string `json:"window_size,omitempty" jsonschema:"rollup only: aggregation window as a duration, e.g. 3600s or 86400s. Defaults to 86400s."`
	WindowDays int    `json:"window_days,omitempty" jsonschema:"daily-rollup only: aggregation window in days. Defaults to 1."`
	ID         string `json:"id,omitempty" jsonschema:"get only: the data point ID to fetch. Required for get."`
	Detail     bool   `json:"detail,omitempty" jsonschema:"sleep list only: include the per-stage time breakdown."`
	Raw        bool   `json:"raw,omitempty" jsonschema:"Return the original API response instead of the simplified one. Use when you need a field the simplified shape drops."`
}

func queryData(r Runner) mcp.ToolHandlerFor[queryDataInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in queryDataInput) (*mcp.CallToolResult, any, error) {
		args, err := buildQueryArgs(in)
		if err != nil {
			return nil, nil, err
		}
		out, err := r.Run(ctx, args...)
		if err != nil {
			return nil, nil, err
		}
		return textResult(out)
	}
}

// buildQueryArgs validates the request against the registry and renders it as a
// ghealth command line. Flags are gated by operation so the child process never
// receives a flag its subcommand has not registered — an agent gets a targeted
// message here instead of a cobra usage dump.
func buildQueryArgs(in queryDataInput) ([]string, error) {
	id, err := resolveDataType(in.DataType)
	if err != nil {
		return nil, err
	}
	dt := types.Get(id)

	op := strings.TrimSpace(in.Operation)
	if op == "" {
		return nil, fmt.Errorf("operation is required — %s supports: %s", id, strings.Join(dt.Operations, ", "))
	}
	if !readOnlyOperations[op] {
		return nil, fmt.Errorf("operation %q is not available over MCP — this server is read-only and supports: %s",
			op, strings.Join(sortedKeys(readOnlyOperations), ", "))
	}
	if !supportsOperation(dt, op) {
		return nil, fmt.Errorf("%s does not support %q — it supports: %s",
			id, op, strings.Join(dt.Operations, ", "))
	}

	args := []string{"data", id, op}

	switch op {
	case "get":
		if strings.TrimSpace(in.ID) == "" {
			return nil, fmt.Errorf("id is required for get — find one with operation 'list' on %s", id)
		}
		args = append(args, "--id", in.ID)

	case "list", "reconcile":
		args = appendIfSet(args, "--from", in.From)
		args = appendIfSet(args, "--to", in.To)
		args = appendIfSet(args, "--filter", in.Filter)
		if op == "list" {
			if in.Limit < 0 {
				return nil, fmt.Errorf("limit must not be negative")
			}
			if in.Limit > 0 {
				args = append(args, "--limit", strconv.Itoa(in.Limit))
			}
			args = appendIfSet(args, "--page-token", in.PageToken)
			if in.Detail {
				if id != "sleep" {
					return nil, fmt.Errorf("detail is only supported for sleep list")
				}
				args = append(args, "--detail")
			}
		}

	case "rollup":
		if err := requireRange(in.From, in.To, id, op); err != nil {
			return nil, err
		}
		args = append(args, "--from", in.From, "--to", in.To)
		args = appendIfSet(args, "--window-size", in.WindowSize)

	case "daily-rollup":
		if err := requireRange(in.From, in.To, id, op); err != nil {
			return nil, err
		}
		args = append(args, "--from", in.From, "--to", in.To)
		if in.WindowDays < 0 {
			return nil, fmt.Errorf("window_days must not be negative")
		}
		if in.WindowDays > 0 {
			args = append(args, "--window-days", strconv.Itoa(in.WindowDays))
		}
	}

	if in.Raw {
		args = append(args, "--raw")
	}
	return append(args, "--format", "json"), nil
}

func requireRange(from, to, id, op string) error {
	if strings.TrimSpace(from) == "" || strings.TrimSpace(to) == "" {
		return fmt.Errorf("from and to are both required for %s on %s (the API caps the range at %d days)",
			op, id, types.Get(id).RollupRangeCapDays())
	}
	return nil
}

// ─── get_user_info ───────────────────────────────────────────────

type getUserInfoInput struct {
	Resource string `json:"resource" jsonschema:"One of identity, profile, settings, irn-profile, paired-devices."`
}

func getUserInfo(r Runner) mcp.ToolHandlerFor[getUserInfoInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in getUserInfoInput) (*mcp.CallToolResult, any, error) {
		sub, ok := userResources[strings.TrimSpace(in.Resource)]
		if !ok {
			return nil, nil, fmt.Errorf("unknown resource %q — use one of: %s",
				in.Resource, strings.Join(sortedKeys(userResources), ", "))
		}
		out, err := r.Run(ctx, append(append([]string{}, sub...), "--format", "json")...)
		if err != nil {
			return nil, nil, err
		}
		return textResult(out)
	}
}

// ─── auth_status ─────────────────────────────────────────────────

type authStatusInput struct{}

func authStatus(r Runner) mcp.ToolHandlerFor[authStatusInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ authStatusInput) (*mcp.CallToolResult, any, error) {
		out, err := r.Run(ctx, "auth", "status", "--format", "json")
		if err != nil {
			return nil, nil, err
		}
		return textResult(out)
	}
}

// ─── export_exercise_tcx ─────────────────────────────────────────

type exportExerciseTCXInput struct {
	ID string `json:"id" jsonschema:"The exercise data point ID. Find it with query_data on data_type 'exercise'."`
	As string `json:"as,omitempty" jsonschema:"Output format: 'csv' for one row per trackpoint (default), or 'tcx' for the raw TCX XML Google returns."`
}

func exportExerciseTCX(r Runner) mcp.ToolHandlerFor[exportExerciseTCXInput, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in exportExerciseTCXInput) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(in.ID) == "" {
			return nil, nil, fmt.Errorf("id is required — find one with query_data on data_type 'exercise'")
		}
		// CSV is the default here, unlike the CLI: a trackpoint table is far
		// cheaper to put in a model's context than raw TCX XML.
		as := strings.TrimSpace(in.As)
		if as == "" {
			as = "csv"
		}
		if as != "csv" && as != "tcx" {
			return nil, nil, fmt.Errorf("as must be 'csv' or 'tcx', got %q", as)
		}
		// --output - writes the export to stdout instead of a file; a container
		// filesystem is not somewhere the caller could retrieve it from.
		out, err := r.Run(ctx, "data", "exercise", "export-tcx", "--id", in.ID, "--output", "-", "--as", as)
		if err != nil {
			return nil, nil, err
		}
		return textResult(out)
	}
}

// ─── helpers ─────────────────────────────────────────────────────

// resolveDataType validates a type ID against the registry, suggesting near
// matches when an agent guesses a plausible but wrong name (e.g. "heart_rate").
func resolveDataType(raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", fmt.Errorf("data_type is required — call list_data_types to see the available IDs")
	}
	if types.Get(id) != nil {
		return id, nil
	}
	if suggestions := suggestTypes(id); len(suggestions) > 0 {
		return "", fmt.Errorf("unknown data type %q — did you mean: %s?", id, strings.Join(suggestions, ", "))
	}
	return "", fmt.Errorf("unknown data type %q — call list_data_types to see the available IDs", id)
}

// suggestTypes offers up to five registry IDs related to a miss: an exact hit
// once underscores are normalised to hyphens, then any ID sharing a word with
// the input.
func suggestTypes(id string) []string {
	normalized := strings.ReplaceAll(strings.ToLower(id), "_", "-")
	if types.Get(normalized) != nil {
		return []string{normalized}
	}
	var out []string
	for _, candidate := range types.IDs() {
		if strings.Contains(candidate, normalized) || strings.Contains(normalized, candidate) {
			out = append(out, candidate)
			continue
		}
		for _, word := range strings.Split(normalized, "-") {
			if len(word) > 3 && strings.Contains(candidate, word) {
				out = append(out, candidate)
				break
			}
		}
	}
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

func supportsOperation(dt *types.DataType, op string) bool {
	for _, o := range dt.Operations {
		if o == op {
			return true
		}
	}
	return false
}

func appendIfSet(args []string, flag, value string) []string {
	if strings.TrimSpace(value) == "" {
		return args
	}
	return append(args, flag, value)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// textResult returns CLI stdout to the client unchanged.
func textResult(out []byte) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(out)}},
	}, nil, nil
}

// jsonResult renders a locally built payload in the same indented JSON the CLI
// emits, so every tool's output reads the same way.
func jsonResult(payload any) (*mcp.CallToolResult, any, error) {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to encode result: %w", err)
	}
	return textResult(data)
}
