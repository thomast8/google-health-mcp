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

import "github.com/google/jsonschema-go/jsonschema"

// Output schemas for the tools that answer in JSON.
//
// They are written by hand rather than reflected from a Go type because the
// responses are not Go types: a tool result is the CLI's stdout, and the CLI's
// shape is set by the Health API, whose per-type metric fields are open-ended.
// Declaring them still earns its keep twice over — a client sees the shape of an
// answer in tools/list before spending a call to find out, and the SDK validates
// every result against the declaration, so a drift between this file and the CLI
// surfaces as a failing test rather than as a client quietly misreading a field.
//
// They are deliberately permissive. Nothing is required and additional
// properties are allowed everywhere, because `raw: true` returns the untouched
// Health API response and a schema that rejected it would turn a working call
// into an error. What the schemas do is name and explain the fields an agent
// will actually meet, which is the part it cannot guess.

// anySchema is the always-valid schema, used where a value's shape is set by the
// Health API rather than by this server.
func anySchema(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Description: description}
}

func stringSchema(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: description}
}

func hintsSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:  "array",
		Items: &jsonschema.Schema{Type: "string"},
		Description: "Contextual next-step suggestions for this exact result — a better " +
			"operation to use, a related type worth correlating, or how to continue a " +
			"truncated read. Present only when the server has something useful to say; " +
			"each entry names the tool and arguments to call.",
	}
}

// queryDataOutputSchema describes the one envelope every query_data operation
// answers in. list, get, rollup, daily-rollup and reconcile all normalise to it
// (see output.EnsureEnvelope), so a caller indexes dataPoints without first
// working out which operation it ran.
var queryDataOutputSchema = &jsonschema.Schema{
	Type: "object",
	Description: "A page of health data points. Every operation answers in this shape, so " +
		"dataPoints is always present and always an array — an empty one when the range " +
		"holds no data. Note that missing days are absent rather than zero: a day with no " +
		"recorded steps has no row, it does not have a row of 0.",
	Properties: map[string]*jsonschema.Schema{
		"dataPoints": {
			Type:        "array",
			Description: "The rows. One entry per data point for list/get/reconcile, one per aggregation window for rollup/daily-rollup.",
			Items: &jsonschema.Schema{
				Type: "object",
				Description: "One data point or aggregation window. The time fields below identify it; " +
					"the remaining fields are the measurements, and which ones appear depends on the " +
					"data type — call describe_data_type to see them ahead of time.",
				Properties: map[string]*jsonschema.Schema{
					"date":      stringSchema("YYYY-MM-DD. A daily-rollup window, or a data type recorded per calendar day. Aggregation happens in the user's local days, not UTC."),
					"startDate": stringSchema("YYYY-MM-DD, inclusive. Present instead of 'date' when a daily-rollup window spans several days (window_days > 1)."),
					"endDate":   stringSchema("YYYY-MM-DD, inclusive. The last day of a multi-day daily-rollup window."),
					"start":     stringSchema("ISO 8601 with the user's UTC offset, e.g. 2026-08-17T07:12:04+01:00. Start of an interval data point (steps, exercise, sleep) or of a rollup window."),
					"end":       stringSchema("ISO 8601 with the user's UTC offset. End of an interval data point or rollup window."),
					"time":      stringSchema("ISO 8601 with the user's UTC offset. The instant of a sampled measurement (heart rate, weight, SpO2)."),
					"source":    stringSchema("Where the measurement came from: a device display name, an app package name, or the recording platform."),
					"id":        stringSchema("The data point's identifier. Pass it back as query_data's 'id' with operation 'get', or to export_exercise_tcx for an exercise."),
				},
				AdditionalProperties: anySchema(
					"The measurements themselves, named as the Health API names them. Numeric values are " +
						"JSON numbers. list and get carry the type's own field names (steps → count, " +
						"heart-rate → beatsPerMinute); rollup and daily-rollup suffix them with the " +
						"aggregation applied (count → countSum, beatsPerMinute → beatsPerMinuteAvg, " +
						"beatsPerMinuteMin, beatsPerMinuteMax). Units are documented per field by " +
						"describe_data_type."),
			},
		},
		"nextPageToken": stringSchema(
			"More data is available. Call query_data again with the same arguments plus " +
				"page_token set to this value. Absent when the result is complete."),
		"_hints": hintsSchema(),
	},
	AdditionalProperties: anySchema("With raw: true the untouched Health API response is returned, which carries its own fields."),
}

// listDataTypesOutputSchema describes the registry listing. Unlike the other
// tools this payload is built in-process, so its shape is fully known.
var listDataTypesOutputSchema = &jsonschema.Schema{
	Type:        "object",
	Description: "The health data types this server can read.",
	Properties: map[string]*jsonschema.Schema{
		"count":    {Type: "integer", Description: "How many types are listed."},
		"category": stringSchema("Echoed back when the request filtered by category; absent otherwise."),
		"dataTypes": {
			Type: "array",
			Items: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"id":          stringSchema("The type ID to pass as query_data's data_type, e.g. steps, heart-rate, sleep."),
					"category":    stringSchema("The OAuth scope group this type belongs to, e.g. activity_and_fitness, sleep, health_metrics_and_measurements."),
					"description": stringSchema("What the type records."),
					"operations": {
						Type:        "array",
						Items:       &jsonschema.Schema{Type: "string"},
						Description: "The operations this type supports. Only list, get, rollup, daily-rollup and reconcile are reachable over MCP; create, update and delete appear here for completeness but this server refuses them.",
					},
					"writable":   {Type: "boolean", Description: "Whether the Health API accepts writes for this type. Always irrelevant over MCP, which is read-only."},
					"rollupOnly": {Type: "boolean", Description: "True when the type has no per-point read at all and must be queried with rollup or daily-rollup."},
				},
				Required: []string{"id", "category", "description", "operations"},
			},
		},
	},
	Required:             []string{"count", "dataTypes"},
	AdditionalProperties: anySchema(""),
}

// describeDataTypeOutputSchema describes one type's reference page, as built by
// 'ghealth schema type' from the Health API's own discovery document.
var describeDataTypeOutputSchema = &jsonschema.Schema{
	Type:        "object",
	Description: "Everything needed to query one data type: the fields it returns, the parameters each operation takes, and the OAuth scope it needs.",
	Properties: map[string]*jsonschema.Schema{
		"id":          stringSchema("The type ID, as passed to query_data's data_type."),
		"category":    stringSchema("The OAuth scope group this type belongs to."),
		"description": stringSchema("What the type records."),
		"filterName":  stringSchema("The snake_case name this type uses inside filter expressions, which differs from the hyphenated id (heart-rate → heart_rate)."),
		"scope":       stringSchema("The OAuth scope a caller must have been granted to read this type. Compare it against auth_status when a read fails with a permission error."),
		"operations": {
			Type:        "array",
			Items:       &jsonschema.Schema{Type: "string"},
			Description: "The operations this type supports.",
		},
		"writable":   {Type: "boolean", Description: "Whether the Health API accepts writes for this type. This server does not."},
		"rollupOnly": {Type: "boolean", Description: "True when the type must be queried with rollup or daily-rollup."},
		"fields": {
			Type:        "array",
			Description: "The fields a data point of this type carries, straight from the API's discovery document.",
			Items: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"name":        stringSchema("The field name as it appears in a query_data result."),
					"description": stringSchema("What the field means, including its unit."),
					"type":        stringSchema("The API's wire type. int64 fields are declared as \"string\" here because the API serializes them quoted; query_data returns them as JSON numbers."),
					"format":      stringSchema("The wire format where the type alone is not specific enough, e.g. int64, double."),
					"required":    {Type: "boolean", Description: "Whether every data point of this type carries the field."},
					"properties": {
						Type:        "array",
						Items:       &jsonschema.Schema{Type: "string"},
						Description: "For an object-valued field, the names of its sub-fields.",
					},
				},
				Required:             []string{"name"},
				AdditionalProperties: anySchema(""),
			},
		},
		"parameters": {
			Type: "object",
			Description: "Per-operation request parameters, keyed by operation name. Mostly informational for MCP " +
				"callers, since query_data builds the request — the useful part is the 'list' entry's 'filter', " +
				"which is the template to follow when passing query_data's filter argument.",
			AdditionalProperties: anySchema("One operation's parameters."),
		},
		"source": stringSchema("Whether the discovery document behind this answer came from the local 24-hour cache or a live fetch."),
	},
	Required:             []string{"id", "operations"},
	AdditionalProperties: anySchema(""),
}

// authStatusOutputSchema describes the credential report. The CLI emits a
// different set of keys per credential source, so nothing is required.
var authStatusOutputSchema = &jsonschema.Schema{
	Type:        "object",
	Description: "Which credentials this server is using and what they permit. The exact keys depend on the credential source.",
	Properties: map[string]*jsonschema.Schema{
		"status":            stringSchema("authenticated or not_authenticated."),
		"authenticated":     {Type: "boolean", Description: "Whether usable credentials are present. Reflects local state — an unexpired token can still have been revoked upstream."},
		"account":           stringSchema("The Google account the stored credentials belong to, when known."),
		"credential_source": stringSchema("Where the credentials came from: stored tokens, an injected access token, or an environment variable."),
		"scopes": {
			Type:        "array",
			Items:       &jsonschema.Schema{Type: "string"},
			Description: "The OAuth scopes granted. A read fails with a permission error when the type's scope, as reported by describe_data_type, is missing from this list.",
		},
		"expiry": stringSchema("RFC 3339 expiry of the current access token."),
		"connected_google_account": stringSchema(
			"The email of the Google account this MCP connection signed in as. Added by the server, " +
				"and the field to check when several people share a deployment. Absent in " +
				"single-account mode, where every caller reads the server's own account."),
		"message": stringSchema("An explanation when there is nothing to report."),
	},
	AdditionalProperties: anySchema(""),
}

// getUserInfoOutputSchema covers five different Health API resources, so it
// documents the fields each one is known to carry and requires none of them.
var getUserInfoOutputSchema = &jsonschema.Schema{
	Type: "object",
	Description: "The requested account resource, as the Health API returns it. Which fields are " +
		"present depends on the resource asked for, and on what the user has filled in — an " +
		"unset profile field is absent rather than empty.",
	Properties: map[string]*jsonschema.Schema{
		"name":          stringSchema("The resource's own API name, e.g. users/me/profile."),
		"height":        anySchema("profile: the user's height, as a value and unit."),
		"dateOfBirth":   anySchema("profile: the user's date of birth as a civil date {year, month, day}."),
		"sex":           stringSchema("profile: the user's sex as recorded in the Health app."),
		"pairedDevices": anySchema("paired-devices: the devices linked to the account, each with its model, manufacturer and identifiers."),
	},
	AdditionalProperties: anySchema("Resource-specific fields, set by the Health API."),
}
