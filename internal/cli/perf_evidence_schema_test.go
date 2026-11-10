package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestPerfEvidenceV1SchemaContract(t *testing.T) {
	schemaBytes, err := os.ReadFile("../../docs/schemas/perf-evidence-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	schema, err := parseArtifactSchema(schemaBytes)
	if err != nil {
		t.Fatalf("compile docs-owned schema: %v", err)
	}
	fixture, err := os.ReadFile("../../docs/schemas/examples/perf-evidence-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if violations := validateJSONAgainstSchema(fixture, schema); len(violations) != 0 {
		t.Fatalf("validate original fixture bytes: %v", violations)
	}

	tests := []struct {
		name   string
		target string
		field  string
		value  any
		remove bool
		valid  bool
	}{
		{name: "legacy v1 illustrative fixture", valid: true},
		{name: "current v2 meter configuration", target: "meter", field: "configuration", value: "crabbox-wasmtime-fuel/v2", valid: true},
		{name: "unknown meter configuration", target: "meter", field: "configuration", value: "crabbox-wasmtime-fuel/v3"},
		{name: "lease ID is optional", field: "leaseId", remove: true, valid: true},
		{name: "run ID is optional", field: "runId", remove: true, valid: true},
		{name: "root extension", field: "future", value: true, valid: true},
		{name: "workload extension", target: "workload", field: "future", value: true, valid: true},
		{name: "meter extension", target: "meter", field: "future", value: true, valid: true},
		{name: "result extension", target: "result", field: "future", value: true, valid: true},
		{name: "unsupported version", field: "schemaVersion", value: 2},
		{name: "missing timestamp", field: "generatedAt", remove: true},
		{name: "empty provider", field: "provider", value: ""},
		{name: "null workload", field: "workload", value: nil},
		{name: "missing module digest", target: "workload", field: "moduleSha256", remove: true},
		{name: "malformed module digest", target: "workload", field: "moduleSha256", value: "not-a-digest"},
		{name: "wrong target", target: "workload", field: "target", value: "wasip2/wasm"},
		{name: "non-string argument", target: "workload", field: "argv", value: []any{1}},
		{name: "too many arguments", target: "workload", field: "argv", value: make([]string, 129)},
		{name: "wrong mechanism", target: "meter", field: "mechanism", value: "native-instructions"},
		{name: "wrong engine", target: "meter", field: "engine", value: "another-engine"},
		{name: "missing engine revision", target: "meter", field: "engineRevision", remove: true},
		{name: "malformed engine revision", target: "meter", field: "engineRevision", value: "main"},
		{name: "wrong meter configuration", target: "meter", field: "configuration", value: "v2"},
		{name: "wrong WASI configuration", target: "meter", field: "wasiConfiguration", value: "ambient"},
		{name: "malformed comparison key", target: "meter", field: "comparisonKey", value: "sha256:abc"},
		{name: "non-boolean determinism", target: "meter", field: "deterministic", value: "true"},
		{name: "negative initial fuel", target: "meter", field: "initialFuel", value: -1},
		{name: "unsafe integer fuel", target: "meter", field: "initialFuel", value: json.Number("9007199254740992")},
		{name: "fractional remaining fuel", target: "meter", field: "remainingFuel", value: json.Number("0.5")},
		{name: "no metric", field: "results", value: []any{}},
		{name: "multiple metrics", field: "results", value: []any{
			map[string]any{"metric": "fuel", "unit": "fuel", "measured": 0, "budget": 0, "exceeded": false},
			map[string]any{"metric": "fuel", "unit": "fuel", "measured": 0, "budget": 0, "exceeded": false},
		}},
		{name: "wrong metric", target: "result", field: "metric", value: "instructions"},
		{name: "wrong unit", target: "result", field: "unit", value: "cycles"},
		{name: "negative measured fuel", target: "result", field: "measured", value: -1},
		{name: "unsafe integer budget", target: "result", field: "budget", value: json.Number("9007199254740992")},
		{name: "largest safe integer budget", target: "result", field: "budget", value: json.Number("9007199254740991"), valid: true},
		{name: "non-boolean result", target: "result", field: "exceeded", value: "false"},
		{name: "missing top-level result", field: "exceeded", remove: true},

		// Shape validation is deliberately not the semantic fuel gate. These
		// structurally valid claims still require independent runtime validation.
		{name: "timestamp format is an annotation", field: "generatedAt", value: "not-a-date", valid: true},
		{name: "shape permits nondeterminism", target: "meter", field: "deterministic", value: false, valid: true},
		{name: "shape permits inconsistent arithmetic", target: "result", field: "measured", value: 0, valid: true},
		{name: "shape permits remaining above initial", target: "meter", field: "initialFuel", value: 0, valid: true},
		{name: "shape permits inconsistent budget result", target: "result", field: "budget", value: 0, valid: true},
		{name: "shape permits inconsistent aggregate", field: "exceeded", value: true, valid: true},
		{name: "shape cannot authenticate a comparison key", target: "meter", field: "comparisonKey", value: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", valid: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var doc map[string]any
			decoder := json.NewDecoder(bytes.NewReader(fixture))
			decoder.UseNumber()
			if err := decoder.Decode(&doc); err != nil {
				t.Fatal(err)
			}
			object := doc
			switch tc.target {
			case "workload", "meter":
				object = doc[tc.target].(map[string]any)
			case "result":
				object = doc["results"].([]any)[0].(map[string]any)
			}
			if tc.remove {
				delete(object, tc.field)
			} else if tc.field != "" {
				object[tc.field] = tc.value
			}
			data, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			violations := validateJSONAgainstSchema(data, schema)
			if got := len(violations) == 0; got != tc.valid {
				t.Fatalf("valid = %v, want %v; violations: %v", got, tc.valid, violations)
			}
		})
	}
}
