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

	"ghealth/pkg/mcpauth"
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

// readAnnotations describe every tool this server exposes: each one reads, none
// of them writes, all of them reach Google's servers rather than answering from a
// closed world of the client's own state, and repeating any of them changes
// nothing — so a client is free to retry a call it did not get an answer to.
func readAnnotations() *mcp.ToolAnnotations {
	openWorld := true
	return &mcp.ToolAnnotations{
		ReadOnlyHint:   true,
		IdempotentHint: true,
		OpenWorldHint:  &openWorld,
	}
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
			"operations each one supports.\n\n" +
			"Start here whenever you do not already know the type ID for a health question: " +
			"IDs are hyphenated and not always guessable ('heart-rate', 'oxygen-saturation', " +
			"'daily-resting-heart-rate'), and passing a wrong one costs a round trip. Filter by " +
			"category to narrow a broad question — 'activity_and_fitness' for movement and " +
			"exercise, 'sleep', 'nutrition', 'health_metrics_and_measurements' for vitals and " +
			"body measurements. Answers from a local registry, so it costs no API call.",
		Annotations:  readAnnotations(),
		OutputSchema: listDataTypesOutputSchema,
	}, listDataTypes)

	mcp.AddTool(s, &mcp.Tool{
		Name:  "describe_data_type",
		Title: "Describe a health data type",
		Description: "Show the fields one data type returns, the parameters each of its " +
			"operations takes, its filter template and the OAuth scope it needs.\n\n" +
			"Call this between list_data_types and query_data when the answer depends on knowing " +
			"the data rather than just fetching it: which field carries the measurement you want " +
			"and in what unit, whether the type supports rollup at all, or what a filter " +
			"expression for it has to look like. Skip it for a straightforward read of a " +
			"familiar type. Also the way to find the scope to check against auth_status when a " +
			"query fails with a permission error.",
		Annotations:  readAnnotations(),
		OutputSchema: describeDataTypeOutputSchema,
	}, describeDataType(r))

	mcp.AddTool(s, &mcp.Tool{
		Name:  "query_data",
		Title: "Query health data",
		Description: "Read health data of one type over one date range. This is the tool that " +
			"answers health questions; the others exist to tell you how to call it.\n\n" +
			"Pick the operation by the question, not by habit:\n" +
			"• 'daily-rollup' — daily totals and averages ('how many steps yesterday', 'resting " +
			"heart rate this month'). Use it instead of listing points and adding them up: it " +
			"aggregates in the user's local days, which a sum over UTC timestamps gets wrong " +
			"around midnight, and it returns days instead of thousands of rows.\n" +
			"• 'list' — individual readings, when timestamps or per-point detail matter ('when " +
			"did I run today', 'my heart rate during that workout'). Auto-paginates.\n" +
			"• 'rollup' — fixed windows other than a day, set by window_size ('steps per hour').\n" +
			"• 'get' — one data point by id, from a previous list.\n" +
			"• 'reconcile' — which points changed, for syncing a local copy.\n\n" +
			"from/to accept YYYY-MM-DD, a full ISO 8601 timestamp, or 'today'/'yesterday', and " +
			"'to' includes the day it names. Both are required for the rollup operations, which " +
			"the API caps at 90 days per call (14 for heart-rate, total-calories, active-minutes " +
			"and calories-in-heart-rate-zone) — split a longer question into several calls.\n\n" +
			"Days with no recorded data are absent from the result, not zero. An empty " +
			"dataPoints array means nothing was recorded, or the range predates the user's " +
			"device — it does not mean the request was wrong.",
		Annotations:  readAnnotations(),
		OutputSchema: queryDataOutputSchema,
	}, queryData(r))

	mcp.AddTool(s, &mcp.Tool{
		Name:  "get_user_info",
		Title: "Get user profile and devices",
		Description: "Read account-level information rather than measurements: the user's " +
			"identity, their profile (height, date of birth, sex), their Health app settings, " +
			"their irregular-rhythm-notification profile, or the devices paired with the " +
			"account.\n\n" +
			"Reach for it when a health answer needs context the measurements do not carry — " +
			"height to interpret a weight, age for heart-rate zones, or which watch was " +
			"recording. Also the way to answer 'which device did this come from' when a data " +
			"point's 'source' field is not specific enough.",
		Annotations:  readAnnotations(),
		OutputSchema: getUserInfoOutputSchema,
	}, getUserInfo(r))

	mcp.AddTool(s, &mcp.Tool{
		Name:  "auth_status",
		Title: "Check authentication status",
		Description: "Report which credentials this server is using, which Google account they " +
			"belong to, the OAuth scopes granted and when the access token expires.\n\n" +
			"Call it when a query fails with an authentication or permission error: the usual " +
			"cause is a scope that was never granted, and comparing the scopes listed here " +
			"against the one describe_data_type reports for the type says so definitively. Also " +
			"answers 'whose data am I reading?', which matters on a deployment several people " +
			"share. Not a health query — do not call it before an ordinary read.",
		Annotations:  readAnnotations(),
		OutputSchema: authStatusOutputSchema,
	}, authStatus(r))

	mcp.AddTool(s, &mcp.Tool{
		Name:  "export_exercise_tcx",
		Title: "Export an exercise track",
		Description: "Export one exercise session's recorded track: the second-by-second " +
			"trackpoints behind a workout, as CSV (time, lap, latitude, longitude, altitude, " +
			"distance, heart rate) or as the raw TCX XML Google returns.\n\n" +
			"Use it for questions about what happened *during* a session — pace over distance, " +
			"the route taken, how heart rate moved across it. For a session's totals, the " +
			"exercise data point from query_data already has them, and is far cheaper.\n\n" +
			"Needs an exercise id: call query_data with data_type 'exercise' and operation " +
			"'list' first, and take 'id' from the session you want. Indoor sessions have no " +
			"track and return a header-only CSV.\n\n" +
			"Returns text — CSV rows or XML — not JSON, and one outdoor session can run to " +
			"thousands of rows.",
		Annotations: readAnnotations(),
	}, exportExerciseTCX(r))
}

// ─── list_data_types ─────────────────────────────────────────────

type listDataTypesInput struct {
	Category string `json:"category,omitempty" jsonschema:"Only return types in this category. Known categories: activity_and_fitness, sleep, nutrition, health_metrics_and_measurements. Omit to list every type, which is the right choice when you are not sure which category a question falls in."`
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
	return localJSONResult(payload)
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
	DataType string `json:"data_type" jsonschema:"The data type ID to describe, e.g. steps, heart-rate, sleep, weight. IDs are hyphenated; list_data_types is the authoritative list."`
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
		return jsonResult(out)
	}
}

// ─── query_data ──────────────────────────────────────────────────

type queryDataInput struct {
	DataType   string `json:"data_type" jsonschema:"The data type to read, e.g. steps, heart-rate, sleep, weight. IDs are hyphenated; list_data_types is the authoritative list."`
	Operation  string `json:"operation" jsonschema:"How to read it. 'daily-rollup' for daily totals and averages (the right choice for 'how many steps yesterday'); 'list' for individual timestamped readings; 'rollup' for fixed windows other than a day; 'get' for one point by id; 'reconcile' for what changed since a sync. Must be an operation the type supports — list_data_types reports which."`
	From       string `json:"from,omitempty" jsonschema:"Start of the range: YYYY-MM-DD, a full ISO 8601 timestamp, 'today' or 'yesterday'. Required for rollup and daily-rollup, optional for list and reconcile."`
	To         string `json:"to,omitempty" jsonschema:"End of the range, including the whole day it names: YYYY-MM-DD, a full ISO 8601 timestamp, 'today' or 'yesterday'. Required for rollup and daily-rollup. Use the same value as 'from' for a single day."`
	Limit      int    `json:"limit,omitempty" jsonschema:"list only: how many data points to return in total, across as many pages as it takes. Defaults to 500. Sampled types run to thousands of points a day, so leave this low unless you need every reading."`
	PageToken  string `json:"page_token,omitempty" jsonschema:"list only: continue a previous read by passing back its response's nextPageToken, keeping every other argument the same."`
	Filter     string `json:"filter,omitempty" jsonschema:"list and reconcile only: a raw Health API filter expression, which replaces from/to entirely. Only needed for bounds from/to cannot express, such as an exact sub-day window; call describe_data_type for this type's filter template, since field names are snake_case and differ from the type ID."`
	WindowSize string `json:"window_size,omitempty" jsonschema:"rollup only: the aggregation window as a duration string, e.g. '3600s' for hourly. Defaults to 86400s (a day), but prefer the daily-rollup operation for whole days — it aggregates in the user's local days rather than fixed 24-hour blocks."`
	WindowDays int    `json:"window_days,omitempty" jsonschema:"daily-rollup only: how many days each aggregation window covers. Defaults to 1. Set it to 7 for weekly totals; windows wider than a day report startDate and endDate instead of date."`
	ID         string `json:"id,omitempty" jsonschema:"get only, and required there: the data point id to fetch, taken from the 'id' field of a previous list result."`
	Detail     bool   `json:"detail,omitempty" jsonschema:"sleep list only: also return the per-stage breakdown, with a timestamped entry for every AWAKE, LIGHT, DEEP and REM stage. The per-night summary is included either way."`
	Raw        bool   `json:"raw,omitempty" jsonschema:"Return the untouched Health API response instead of the simplified one, with no _hints and no normalised envelope. Only worth setting when you need a field simplification drops; the simplified shape is smaller and easier to read."`
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
		return jsonResult(out)
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
	Resource string `json:"resource" jsonschema:"Which account resource to read. 'identity' for who the account is; 'profile' for height, date of birth and sex; 'settings' for Health app settings; 'irn-profile' for irregular-rhythm-notification state; 'paired-devices' for the devices linked to the account."`
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
		return jsonResult(out)
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
		return jsonResult(withConnectedAccount(ctx, out))
	}
}

// withConnectedAccount names the signed-in Google account in the auth_status
// result. When several people share a deployment, "which account am I seeing?"
// is the question this tool exists to answer, and the CLI cannot answer it — it
// only ever sees the access token the server injected.
//
// The CLI's own output is left untouched if it cannot be parsed; a cosmetic
// addition must not be able to break the tool.
func withConnectedAccount(ctx context.Context, out []byte) []byte {
	session, ok := mcpauth.SessionFromContext(ctx)
	if !ok || session.Email == "" {
		return out
	}
	var status map[string]any
	if err := json.Unmarshal(out, &status); err != nil {
		return out
	}
	status["connected_google_account"] = session.Email
	merged, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return out
	}
	return append(merged, '\n')
}

// ─── export_exercise_tcx ─────────────────────────────────────────

type exportExerciseTCXInput struct {
	ID string `json:"id" jsonschema:"The exercise session's data point id, taken from the 'id' field of a query_data list on data_type 'exercise'."`
	As string `json:"as,omitempty" jsonschema:"Output format: 'csv' for one row per trackpoint (the default, and far cheaper to read), or 'tcx' for the raw TCX XML Google returns."`
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

// textResult returns CLI stdout to the client unchanged, with no structured
// content. It is for the tools whose output is not JSON.
func textResult(out []byte) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(out)}},
	}, nil, nil
}

// jsonResult returns a JSON tool result twice over: as the text block, byte for
// byte what the CLI printed, and as structured content the SDK validates against
// the tool's declared output schema.
//
// Setting the text block explicitly is what keeps the CLI's own bytes — its key
// order and its indentation — in front of the caller. Left to itself the SDK
// would fill the block in from the structured value, which has been through a
// map and comes back alphabetised.
//
// Output that does not parse as JSON is still returned, just without structured
// content. A `--format json` command printing something else is a bug, and
// failing the call outright would hide the evidence of it.
func jsonResult(out []byte) (*mcp.CallToolResult, any, error) {
	res := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(out)}},
	}
	if !json.Valid(out) {
		return res, nil, nil
	}
	return res, json.RawMessage(out), nil
}

// localJSONResult renders a payload this server built itself in the same
// indented JSON the CLI emits, so every tool's output reads the same way.
func localJSONResult(payload any) (*mcp.CallToolResult, any, error) {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to encode result: %w", err)
	}
	return jsonResult(data)
}
