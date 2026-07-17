package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestParseArtifactSchema(t *testing.T) {
	t.Run("valid nested schema with annotations", func(t *testing.T) {
		data := []byte(`{
			"$schema": "https://json-schema.org/draft/2020-12/schema",
			"title": "ignored",
			"type": "object",
			"required": ["status", "items"],
			"properties": {
				"status": {"type": "string", "enum": ["passed", "failed"]},
				"count": {"type": "integer"},
				"items": {"type": "array", "items": {"type": "object", "required": ["name"]}}
			}
		}`)
		schema, err := parseArtifactSchema(data)
		if err != nil {
			t.Fatalf("parseArtifactSchema() unexpected error: %v", err)
		}
		if violations := validateJSONAgainstSchema([]byte(`{"status":"passed","items":[{"name":"x"}]}`), schema); len(violations) != 0 {
			t.Fatalf("compiled schema rejected valid document: %v", violations)
		}
	})

	t.Run("invalid JSON is rejected", func(t *testing.T) {
		if _, err := parseArtifactSchema([]byte(`{"type":`)); err == nil {
			t.Fatalf("expected error for malformed schema JSON")
		}
	})

	t.Run("trailing content is rejected", func(t *testing.T) {
		for _, data := range []string{
			`{"type":"object"}{"type":"string"}`,
			`{"type":"object"} trailing`,
		} {
			if _, err := parseArtifactSchema([]byte(data)); err == nil {
				t.Fatalf("expected trailing schema content to be rejected: %q", data)
			}
		}
	})

	t.Run("unknown type keyword is rejected", func(t *testing.T) {
		_, err := parseArtifactSchema([]byte(`{"type": "timestamp"}`))
		if err == nil || !strings.Contains(err.Error(), "/type") {
			t.Fatalf("expected unknown-type error, got %v", err)
		}
	})

	t.Run("unknown nested type keyword is rejected", func(t *testing.T) {
		_, err := parseArtifactSchema([]byte(`{"type":"object","properties":{"x":{"type":"date"}}}`))
		if err == nil || !strings.Contains(err.Error(), "x") {
			t.Fatalf("expected nested unknown-type error naming path, got %v", err)
		}
	})
}

func TestParseArtifactSchemaRejectsInvalidKeywordShapes(t *testing.T) {
	cases := []struct {
		name   string
		schema string
	}{
		{"empty type", `{"type":""}`},
		{"null type", `{"type":null}`},
		{"null root", `null`},
		{"array root", `[]`},
		{"null required", `{"required":null}`},
		{"duplicate required", `{"required":["x","x"]}`},
		{"null properties", `{"properties":null}`},
		{"null property schema", `{"properties":{"x":null}}`},
		{"null items", `{"items":null}`},
		{"null enum", `{"enum":null}`},
		{"duplicate root keyword", `{"type":"object","type":"string"}`},
		{"duplicate nested keyword", `{"properties":{"x":{"required":["a"],"required":[]}}}`},
		{"duplicate property schema", `{"properties":{"x":{"type":"string"},"x":{"type":"number"}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseArtifactSchema([]byte(tc.schema)); err == nil {
				t.Fatalf("expected invalid schema keyword shape to be rejected: %s", tc.schema)
			}
		})
	}
}

func TestParseArtifactSchemaMisCasedKeywordCannotOverrideRequired(t *testing.T) {
	schema, err := parseArtifactSchema([]byte(`{"type":"object","required":["proof"],"REQUIRED":[]}`))
	if err != nil {
		t.Fatalf("standard JSON Schema extension keyword should compile: %v", err)
	}
	if violations := validateJSONAgainstSchema([]byte(`{}`), schema); len(violations) == 0 {
		t.Fatal("mis-cased extension keyword unexpectedly disabled required validation")
	}
}

func TestValidateJSONAgainstSchema(t *testing.T) {
	schema, err := parseArtifactSchema([]byte(`{
		"type": "object",
		"required": ["status", "count", "items"],
		"properties": {
			"status": {"type": "string", "enum": ["passed", "failed"]},
			"count": {"type": "integer"},
			"config": {"type": "object", "required": ["retries"], "properties": {"retries": {"type": "number"}}},
			"items": {"type": "array", "items": {"type": "object", "required": ["name"], "properties": {"name": {"type": "string"}}}}
		}
	}`))
	if err != nil {
		t.Fatalf("schema parse failed: %v", err)
	}

	tests := []struct {
		name     string
		doc      string
		wantOK   bool
		wantPath string
	}{
		{
			name:   "valid document",
			doc:    `{"status":"passed","count":3,"items":[{"name":"a"},{"name":"b"}]}`,
			wantOK: true,
		},
		{
			name:     "missing required field",
			doc:      `{"status":"passed","items":[]}`,
			wantPath: "",
		},
		{
			name:     "wrong scalar type",
			doc:      `{"status":"passed","count":"three","items":[]}`,
			wantPath: "/count",
		},
		{
			name:     "enum mismatch",
			doc:      `{"status":"skipped","count":1,"items":[]}`,
			wantPath: "/status",
		},
		{
			name:     "nested object property wrong type",
			doc:      `{"status":"passed","count":1,"items":[],"config":{"retries":"nope"}}`,
			wantPath: "/config/retries",
		},
		{
			name:     "array element violation reports index path",
			doc:      `{"status":"passed","count":1,"items":[{"name":"ok"},{"nope":true}]}`,
			wantPath: "/items/1",
		},
		{
			name:     "non-JSON document is a single violation, not a crash",
			doc:      `this is not json`,
			wantPath: "",
		},
		{
			name:     "empty document is a violation",
			doc:      ``,
			wantPath: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			violations := validateJSONAgainstSchema([]byte(tc.doc), schema)
			if tc.wantOK {
				if len(violations) != 0 {
					t.Fatalf("expected no violations, got %v", violations)
				}
				return
			}
			if len(violations) == 0 {
				t.Fatalf("expected a violation, got none")
			}
			found := false
			for _, v := range violations {
				if v.Path == tc.wantPath {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected a violation at path %q, got %v", tc.wantPath, violations)
			}
		})
	}
}

func TestValidateJSONAgainstSchemaTypeMismatchDoesNotCascade(t *testing.T) {
	schema, err := parseArtifactSchema([]byte(`{"type":"object","required":["a"],"properties":{"a":{"type":"string"}}}`))
	if err != nil {
		t.Fatalf("schema parse failed: %v", err)
	}
	violations := validateJSONAgainstSchema([]byte(`"a bare string"`), schema)
	if len(violations) != 1 || violations[0].Keyword != "/type" {
		t.Fatalf("expected exactly one type violation, got %v", violations)
	}
}

func TestValidateJSONAgainstSchemaPreservesExactNumbers(t *testing.T) {
	t.Run("large integers do not collapse in enum", func(t *testing.T) {
		schema, err := parseArtifactSchema([]byte(`{"enum":[9007199254740992]}`))
		if err != nil {
			t.Fatalf("schema parse failed: %v", err)
		}
		violations := validateJSONAgainstSchema([]byte(`9007199254740993`), schema)
		if len(violations) != 1 || violations[0].Keyword != "/enum" {
			t.Fatalf("expected exact enum mismatch, got %v", violations)
		}
	})

	t.Run("equivalent JSON number spellings compare equal", func(t *testing.T) {
		schema, err := parseArtifactSchema([]byte(`{"enum":[1]}`))
		if err != nil {
			t.Fatalf("schema parse failed: %v", err)
		}
		if violations := validateJSONAgainstSchema([]byte(`1.0`), schema); len(violations) != 0 {
			t.Fatalf("expected numerically equal enum value, got %v", violations)
		}
	})

	t.Run("fraction beyond float64 precision is not integer", func(t *testing.T) {
		schema, err := parseArtifactSchema([]byte(`{"type":"integer"}`))
		if err != nil {
			t.Fatalf("schema parse failed: %v", err)
		}
		violations := validateJSONAgainstSchema([]byte(`1.0000000000000001`), schema)
		if len(violations) != 1 || violations[0].Keyword != "/type" {
			t.Fatalf("expected exact integer mismatch, got %v", violations)
		}
	})

	t.Run("large exponent remains exact without expansion", func(t *testing.T) {
		schema, err := parseArtifactSchema([]byte(`{"type":"integer","enum":[1e10000]}`))
		if err != nil {
			t.Fatalf("schema parse failed: %v", err)
		}
		if violations := validateJSONAgainstSchema([]byte(`10e9999`), schema); len(violations) != 0 {
			t.Fatalf("expected equivalent large-exponent integer to pass, got %v", violations)
		}
		if violations := validateJSONAgainstSchema([]byte(`1.1e-10000`), schema); len(violations) == 0 {
			t.Fatalf("expected distinct large-exponent fraction to fail")
		}
	})
}

func TestValidateJSONAgainstSchemaEnumDiagnosticDoesNotIncludeValue(t *testing.T) {
	schema, err := parseArtifactSchema([]byte(`{"type":"string","enum":["allowed"],"pattern":"^allowed$"}`))
	if err != nil {
		t.Fatalf("schema parse failed: %v", err)
	}
	violations := validateJSONAgainstSchema([]byte(`"sensitive-value"`), schema)
	if len(violations) != 1 || violations[0].Keyword != "/enum" {
		t.Fatalf("expected one enum violation, got %v", violations)
	}
	if strings.Contains(violations[0].String(), "sensitive-value") {
		t.Fatalf("schema diagnostic leaked rejected value: %s", violations[0])
	}
	for _, violation := range violations {
		if strings.Contains(violation.String(), "sensitive-value") {
			t.Fatalf("schema diagnostic leaked rejected value: %s", violation)
		}
	}
}

func TestValidateJSONAgainstSchemaRejectsDuplicateObjectNamesWithoutLeakingThem(t *testing.T) {
	schema, err := parseArtifactSchema([]byte(`{"type":"object"}`))
	if err != nil {
		t.Fatalf("schema parse failed: %v", err)
	}
	violations := validateJSONAgainstSchema([]byte(`{"private-name":1,"private-name":2}`), schema)
	if len(violations) != 1 || !strings.Contains(violations[0].Message, "unambiguous JSON") {
		t.Fatalf("expected one duplicate-name violation, got %v", violations)
	}
	if strings.Contains(violations[0].String(), "private-name") {
		t.Fatalf("duplicate-name diagnostic leaked artifact name: %s", violations[0])
	}
	for _, name := range []string{"more than", "number exceeds", "nesting-depth"} {
		doc := `{` + strconv.Quote(name) + `:1,` + strconv.Quote(name) + `:2}`
		violations := validateJSONAgainstSchema([]byte(doc), schema)
		if len(violations) != 1 || !strings.Contains(violations[0].Message, "unambiguous JSON") {
			t.Fatalf("duplicate name %q selected a limit diagnostic: %v", name, violations)
		}
	}
}

func TestValidateJSONAgainstSchemaBoundsViolations(t *testing.T) {
	schema, err := parseArtifactSchema([]byte(`{"type":"array","items":{"type":"string"}}`))
	if err != nil {
		t.Fatalf("schema parse failed: %v", err)
	}
	doc := "[" + strings.Repeat("0,", maxSchemaViolations+20) + "0]"
	violations := validateJSONAgainstSchema([]byte(doc), schema)
	if len(violations) != maxSchemaViolations+1 {
		t.Fatalf("violations=%d, want %d retained plus truncation marker", len(violations), maxSchemaViolations)
	}
	last := violations[len(violations)-1]
	if last.Keyword != "truncated" || !strings.Contains(last.Message, "additional violations omitted") {
		t.Fatalf("missing truncation marker: %v", last)
	}
	if got := schemaViolationSummary(violations); got != "at least 101 checks" {
		t.Fatalf("summary=%q, want bounded count", got)
	}
}

func TestSchemaViolationStringDistinguishesRootFromEmptyProperty(t *testing.T) {
	if got := (schemaViolation{Keyword: "/required"}).String(); !strings.HasPrefix(got, "(root):") {
		t.Fatalf("root diagnostic=%q", got)
	}
	if got := (schemaViolation{Path: "/", Keyword: "/properties//type"}).String(); !strings.HasPrefix(got, "/:") {
		t.Fatalf("empty-name property diagnostic=%q", got)
	}
}

func TestValidateJSONAgainstSchemaEscapesUnsafeLocationCharacters(t *testing.T) {
	schema, err := parseArtifactSchema([]byte(`{"additionalProperties":{"type":"string"}}`))
	if err != nil {
		t.Fatalf("schema parse failed: %v", err)
	}
	violations := validateJSONAgainstSchema([]byte(`{"line\n\u001b\u2028\u2029\u202e":1}`), schema)
	if len(violations) != 1 {
		t.Fatalf("violations=%v", violations)
	}
	rendered := violations[0].String()
	for _, unsafe := range []string{"\n", "\x1b", "\u2028", "\u2029", "\u202e"} {
		if strings.Contains(rendered, unsafe) {
			t.Fatalf("unsafe location character leaked in %q", rendered)
		}
	}
	if !strings.Contains(rendered, `\u000A`) || !strings.Contains(rendered, `\u001B`) ||
		!strings.Contains(rendered, `\u2028`) || !strings.Contains(rendered, `\u2029`) ||
		!strings.Contains(rendered, `\u202E`) {
		t.Fatalf("unsafe location characters not visibly escaped: %q", rendered)
	}
}

func TestValidateJSONAgainstSchemaBoundsDiagnosticBytes(t *testing.T) {
	schemaJSON := `{"additionalProperties":{"allOf":[` + strings.Repeat(`false,`, 149) + `false]}}`
	schema, err := parseArtifactSchema([]byte(schemaJSON))
	if err != nil {
		t.Fatalf("schema parse failed: %v", err)
	}
	longName := strings.Repeat("x", 100_000)
	violations := validateJSONAgainstSchema([]byte(`{`+strconv.Quote(longName)+`:1}`), schema)
	if len(violations) < 2 || violations[len(violations)-1].Keyword != "truncated" {
		t.Fatalf("expected diagnostic-byte truncation, got %d violations", len(violations))
	}
	totalBytes := 0
	for _, violation := range violations {
		if len(violation.Path) > maxSchemaLocationBytes || len(violation.Keyword) > maxSchemaLocationBytes {
			t.Fatalf("oversized diagnostic location: path=%d keyword=%d", len(violation.Path), len(violation.Keyword))
		}
		totalBytes += len(violation.String())
	}
	if totalBytes > maxSchemaDiagnosticBytes {
		t.Fatalf("diagnostic bytes=%d, want <=%d", totalBytes, maxSchemaDiagnosticBytes)
	}
}

func TestValidateJSONAgainstSchemaBoundsStringByteWork(t *testing.T) {
	schemaJSON := `{"allOf":[` + strings.Repeat(`{"minLength":0},`, 2_046) + `{"minLength":0}]}`
	schema, err := parseArtifactSchema([]byte(schemaJSON))
	if err != nil {
		t.Fatalf("schema parse failed: %v", err)
	}
	violations := validateJSONAgainstSchema([]byte(strconv.Quote(strings.Repeat("x", 10_000))), schema)
	if len(violations) != 1 || violations[0].Keyword != "complexity" || !strings.Contains(violations[0].Message, "byte validation safety budget") {
		t.Fatalf("expected string-byte work bound, got %v", violations)
	}
}

func TestValidateJSONAgainstSchemaBoundsRegexByteWork(t *testing.T) {
	schema, err := parseArtifactSchema([]byte(`{"pattern":` + strconv.Quote(strings.Repeat("a", 60_000)) + `}`))
	if err != nil {
		t.Fatalf("schema parse failed: %v", err)
	}
	violations := validateJSONAgainstSchema([]byte(strconv.Quote(strings.Repeat("a", 1_000))), schema)
	if len(violations) != 1 || violations[0].Keyword != "complexity" || !strings.Contains(violations[0].Message, "byte validation safety budget") {
		t.Fatalf("expected regex byte-work bound, got %v", violations)
	}
}

func TestValidateJSONAgainstSchemaChargesExpandedRegexWork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		schema string
		doc    string
	}{
		{"pattern", `{"pattern":"(a?){1000}$"}`, strconv.Quote(strings.Repeat("a", 5_000))},
		{"patternProperties", `{"patternProperties":{"(a?){1000}$":{}}}`, `{` + strconv.Quote(strings.Repeat("a", 5_000)) + `:1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema, err := parseArtifactSchema([]byte(tc.schema))
			if err != nil {
				t.Fatalf("schema parse failed: %v", err)
			}
			violations := validateJSONAgainstSchema([]byte(tc.doc), schema)
			if len(violations) != 1 || violations[0].Keyword != "complexity" || !strings.Contains(violations[0].Message, "byte validation safety budget") {
				t.Fatalf("expected expanded-regex work bound, got %v", violations)
			}
		})
	}
}

func TestValidateJSONAgainstSchemaUsesECMA262RegexpSemantics(t *testing.T) {
	dotSchema, err := parseArtifactSchema([]byte(`{"pattern":"^.$"}`))
	if err != nil {
		t.Fatalf("dot schema parse failed: %v", err)
	}
	if violations := validateJSONAgainstSchema([]byte(`"x"`), dotSchema); len(violations) != 0 {
		t.Fatalf("ordinary character rejected: %v", violations)
	}
	if violations := validateJSONAgainstSchema([]byte(`"\r"`), dotSchema); len(violations) != 1 {
		t.Fatalf("ECMA line terminator accepted by dot: %v", violations)
	}

	controlSchema, err := parseArtifactSchema([]byte(`{"pattern":"^\\cC$"}`))
	if err != nil {
		t.Fatalf("ECMA control escape rejected: %v", err)
	}
	if violations := validateJSONAgainstSchema([]byte(`"\u0003"`), controlSchema); len(violations) != 0 {
		t.Fatalf("ECMA control escape not enforced: %v", violations)
	}

	unicodeSchema, err := parseArtifactSchema([]byte(`{"pattern":"^\\u{1F600}$"}`))
	if err != nil {
		t.Fatalf("ECMA Unicode escape rejected: %v", err)
	}
	if violations := validateJSONAgainstSchema([]byte(`"😀"`), unicodeSchema); len(violations) != 0 {
		t.Fatalf("ECMA Unicode escape not enforced: %v", violations)
	}
	surrogateSchema, err := parseArtifactSchema([]byte(`{"pattern":"^\\uD83D\\uDE00$"}`))
	if err != nil {
		t.Fatalf("ECMA surrogate-pair escape rejected: %v", err)
	}
	if violations := validateJSONAgainstSchema([]byte(`"😀"`), surrogateSchema); len(violations) != 0 {
		t.Fatalf("ECMA surrogate-pair escape not enforced: %v", violations)
	}

	spaceSchema, err := parseArtifactSchema([]byte(`{"pattern":"^\\s$"}`))
	if err != nil {
		t.Fatalf("ECMA whitespace class rejected: %v", err)
	}
	if violations := validateJSONAgainstSchema([]byte(`"\uFEFF"`), spaceSchema); len(violations) != 0 {
		t.Fatalf("ECMA Unicode whitespace not enforced: %v", violations)
	}

	anchorSchema, err := parseArtifactSchema([]byte(`{"pattern":"^a$"}`))
	if err != nil {
		t.Fatalf("ECMA end anchor rejected: %v", err)
	}
	if violations := validateJSONAgainstSchema([]byte(`"a\n"`), anchorSchema); len(violations) != 1 {
		t.Fatalf("ECMA strict end anchor accepted trailing newline: %v", violations)
	}

	caretSchema, err := parseArtifactSchema([]byte(`{"pattern":"^[^^]$"}`))
	if err != nil {
		t.Fatalf("ECMA negated-caret class rejected: %v", err)
	}
	if violations := validateJSONAgainstSchema([]byte(`"a"`), caretSchema); len(violations) != 0 {
		t.Fatalf("ECMA negated-caret class rejected non-caret: %v", violations)
	}
	if violations := validateJSONAgainstSchema([]byte(`"^"`), caretSchema); len(violations) != 1 {
		t.Fatalf("ECMA negated-caret class accepted caret: %v", violations)
	}

	for _, unsupported := range []string{`(?=a)`, `(a)\1`, `\p{L}`, `[]]`, `[^]`} {
		_, err := parseArtifactSchema([]byte(`{"pattern":` + strconv.Quote(unsupported) + `}`))
		if err == nil || !strings.Contains(err.Error(), "not supported by bounded artifact validation") {
			t.Fatalf("unsafe ECMA expression accepted: %q: %v", unsupported, err)
		}
	}
}

func TestValidateJSONAgainstSchemaBoundsNumericWork(t *testing.T) {
	schema, err := parseArtifactSchema([]byte(`{"type":"array","items":{"multipleOf":1}}`))
	if err != nil {
		t.Fatalf("schema parse failed: %v", err)
	}
	doc := `[` + strings.Repeat(`1e10000,`, 1_999) + `1e10000]`
	violations := validateJSONAgainstSchema([]byte(doc), schema)
	if len(violations) != 1 || violations[0].Keyword != "complexity" || !strings.Contains(violations[0].Message, "numeric validation safety budget") {
		t.Fatalf("expected numeric-work bound, got %v", violations)
	}
}

func TestValidateJSONAgainstSchemaBoundsUniqueItemsCollisionWork(t *testing.T) {
	schema, err := parseArtifactSchema([]byte(`{"type":"array","uniqueItems":true}`))
	if err != nil {
		t.Fatalf("schema parse failed: %v", err)
	}
	var doc strings.Builder
	doc.WriteByte('[')
	for i := 0; i < 420; i++ {
		if i > 0 {
			doc.WriteByte(',')
		}
		doc.WriteString(strconv.Quote(strings.Repeat("x", 1_700) + strconv.Itoa(i)))
	}
	doc.WriteByte(']')
	violations := validateJSONAgainstSchema([]byte(doc.String()), schema)
	if len(violations) != 1 || violations[0].Keyword != "complexity" || !strings.Contains(violations[0].Message, "uniqueItems") {
		t.Fatalf("expected uniqueItems collision-work bound, got %v", violations)
	}
}

func TestValidateJSONAgainstSchemaBoundsUniqueItemsNumericWork(t *testing.T) {
	schema, err := parseArtifactSchema([]byte(`{"type":"array","uniqueItems":true}`))
	if err != nil {
		t.Fatalf("schema parse failed: %v", err)
	}
	doc := `[` + strings.Repeat(`1e10000,`, 3_999) + `2e10000]`
	violations := validateJSONAgainstSchema([]byte(doc), schema)
	if len(violations) != 1 || violations[0].Keyword != "complexity" || !strings.Contains(violations[0].Message, "numeric validation safety budget") {
		t.Fatalf("expected uniqueItems numeric-work bound, got %v", violations)
	}
}

func TestValidateJSONAgainstSchemaBoundsDocumentValues(t *testing.T) {
	schema, err := parseArtifactSchema([]byte(`{"type":"array","items":{"type":"integer"}}`))
	if err != nil {
		t.Fatalf("schema parse failed: %v", err)
	}
	doc := "[" + strings.Repeat("0,", maxSchemaArtifactValues) + "0]"
	violations := validateJSONAgainstSchema([]byte(doc), schema)
	if len(violations) != 1 || !strings.Contains(violations[0].Message, "100000-value validation limit") {
		t.Fatalf("expected bounded-document violation, got %v", violations)
	}
}

func TestValidateJSONAgainstSchemaBoundsDocumentDepth(t *testing.T) {
	schema, err := parseArtifactSchema([]byte(`{"type":"object"}`))
	if err != nil {
		t.Fatalf("schema parse failed: %v", err)
	}
	doc := strings.Repeat(`{"next":`, maxSchemaJSONDepth) + `{}` + strings.Repeat(`}`, maxSchemaJSONDepth)
	violations := validateJSONAgainstSchema([]byte(doc), schema)
	if len(violations) != 1 || !strings.Contains(violations[0].Message, "64-level nesting-depth limit") {
		t.Fatalf("expected nesting-depth violation, got %v", violations)
	}
}

func TestArtifactSchemaBoundsJSONNumbers(t *testing.T) {
	schema, err := parseArtifactSchema([]byte(`{"type":"number"}`))
	if err != nil {
		t.Fatalf("schema parse failed: %v", err)
	}
	violations := validateJSONAgainstSchema([]byte(`1e10001`), schema)
	if len(violations) != 1 || violations[0].Message != "artifact JSON exceeds the numeric complexity limit" {
		t.Fatalf("expected numeric-complexity violation, got %v", violations)
	}
	if _, err := parseArtifactSchema([]byte(`{"minimum":1e10001}`)); err == nil || !strings.Contains(err.Error(), "exponent limit") {
		t.Fatalf("expected schema numeric-complexity error, got %v", err)
	}
}

func TestParseArtifactSchemaBoundsImplementationWork(t *testing.T) {
	t.Run("cardinality overflow is rejected", func(t *testing.T) {
		for _, keyword := range []string{"minItems", "maxItems", "minLength", "maxLength", "minProperties", "maxProperties", "minContains", "maxContains"} {
			_, err := parseArtifactSchema([]byte(`{"` + keyword + `":2147483648}`))
			if err == nil || !strings.Contains(err.Error(), "cardinality limit") {
				t.Fatalf("expected %s overflow to be rejected, got %v", keyword, err)
			}
		}
	})

	t.Run("subschema fanout is rejected before compile", func(t *testing.T) {
		schema := `{"allOf":[` + strings.Repeat(`{},`, maxSchemaSubschemas) + `{}` + `]}`
		_, err := parseArtifactSchema([]byte(schema))
		if err == nil || !strings.Contains(err.Error(), "subschema limit") {
			t.Fatalf("expected schema fanout to be bounded, got %v", err)
		}
	})

	t.Run("total schema values are bounded before compile", func(t *testing.T) {
		schema := `{"enum":[` + strings.Repeat(`0,`, maxSchemaDefinitionValues) + `0]}`
		_, err := parseArtifactSchema([]byte(schema))
		if err == nil || !strings.Contains(err.Error(), "more than 4096 values") {
			t.Fatalf("expected schema values to be bounded, got %v", err)
		}
	})

	t.Run("invalid subschema is rejected before meta-validation", func(t *testing.T) {
		_, err := parseArtifactSchema([]byte(`{"allOf":[0]}`))
		if err == nil || !strings.Contains(err.Error(), "subschema must be an object or boolean") {
			t.Fatalf("expected invalid subschema to fail early, got %v", err)
		}
	})

	t.Run("schema-like data inside const is not treated as a subschema", func(t *testing.T) {
		schema, err := parseArtifactSchema([]byte(`{"const":{"allOf":[0]}}`))
		if err != nil {
			t.Fatalf("schema data rejected as subschema: %v", err)
		}
		if violations := validateJSONAgainstSchema([]byte(`{"allOf":[0]}`), schema); len(violations) != 0 {
			t.Fatalf("matching const rejected: %v", violations)
		}
	})

	t.Run("schema and artifact work product is bounded", func(t *testing.T) {
		schemaJSON := `{"allOf":[` + strings.Repeat(`{},`, 100) + `{}` + `]}`
		schema, err := parseArtifactSchema([]byte(schemaJSON))
		if err != nil {
			t.Fatalf("schema parse failed: %v", err)
		}
		doc := `[` + strings.Repeat(`0,`, 999) + `0]`
		violations := validateJSONAgainstSchema([]byte(doc), schema)
		if len(violations) != 1 || violations[0].Keyword != "complexity" {
			t.Fatalf("expected validation-work bound, got %v", violations)
		}
	})

	t.Run("arbitrary local ref target contributes full document weight", func(t *testing.T) {
		schemaJSON := `{"$ref":"#/hidden","hidden":{"allOf":[` + strings.Repeat(`{"type":"integer"},`, 100) + `{"type":"integer"}]}}`
		schema, err := parseArtifactSchema([]byte(schemaJSON))
		if err != nil {
			t.Fatalf("schema parse failed: %v", err)
		}
		doc := `[` + strings.Repeat(`0,`, 999) + `0]`
		violations := validateJSONAgainstSchema([]byte(doc), schema)
		if len(violations) != 1 || violations[0].Keyword != "complexity" {
			t.Fatalf("expected local-ref work bound, got %v", violations)
		}
	})

	t.Run("reference fanout is expanded before validation", func(t *testing.T) {
		var schemaJSON strings.Builder
		schemaJSON.WriteString(`{"$ref":"#/$defs/d0","$defs":{`)
		for i := 0; i < 20; i++ {
			if i > 0 {
				schemaJSON.WriteByte(',')
			}
			schemaJSON.WriteString(strconv.Quote("d" + strconv.Itoa(i)))
			schemaJSON.WriteString(`:{"allOf":[{"$ref":"#/$defs/d`)
			schemaJSON.WriteString(strconv.Itoa(i + 1))
			schemaJSON.WriteString(`"},{"$ref":"#/$defs/d`)
			schemaJSON.WriteString(strconv.Itoa(i + 1))
			schemaJSON.WriteString(`"}]}`)
		}
		schemaJSON.WriteString(`,"d20":{"type":"integer"}}}`)
		if _, err := parseArtifactSchema([]byte(schemaJSON.String())); err == nil || !strings.Contains(err.Error(), "expanded validation safety budget") {
			t.Fatalf("reference fanout accepted: %v", err)
		}
	})

	t.Run("recursive reference cycles fail preflight", func(t *testing.T) {
		schemaJSON := `{
			"$defs":{"node":{
				"type":"object",
				"properties":{"next":{"allOf":[
					{"$ref":"#/$defs/node"},
					{"$ref":"#/$defs/node"}
				]}}
			}},
			"$ref":"#/$defs/node"
		}`
		if _, err := parseArtifactSchema([]byte(schemaJSON)); err == nil || !strings.Contains(err.Error(), "recursive schema references") {
			t.Fatalf("recursive reference cycle accepted: %v", err)
		}
	})

	t.Run("reference shaped literal data is not schema work", func(t *testing.T) {
		literal := strings.Repeat(`{"$ref":"literal"},`, 224) + `{"$ref":"literal"}`
		if _, err := parseArtifactSchema([]byte(`{"const":[` + literal + `]}`)); err != nil {
			t.Fatalf("reference-shaped const data rejected: %v", err)
		}
	})

	t.Run("referenced annotation target receives cardinality bounds", func(t *testing.T) {
		_, err := parseArtifactSchema([]byte(`{
			"$ref":"#/definitions/x",
			"definitions":{"x":{"type":"array","minItems":9223372036854775808}}
		}`))
		if err == nil || !strings.Contains(err.Error(), "cardinality limit") {
			t.Fatalf("oversized referenced cardinality accepted: %v", err)
		}
	})

	t.Run("referenced nested resource target receives cardinality bounds", func(t *testing.T) {
		_, err := parseArtifactSchema([]byte(`{
			"properties":{"nested":{
				"$id":"nested.json",
				"$ref":"#/hidden",
				"hidden":{"type":"array","minItems":18446744073709551616}
			}}
		}`))
		if err == nil || !strings.Contains(err.Error(), "cardinality limit") {
			t.Fatalf("oversized nested-resource cardinality accepted: %v", err)
		}
	})

	t.Run("static anchor references remain supported", func(t *testing.T) {
		schema, err := parseArtifactSchema([]byte(`{
			"$defs":{"value":{"$anchor":"value","type":"string"}},
			"$ref":"#value"
		}`))
		if err != nil {
			t.Fatalf("static anchor reference rejected: %v", err)
		}
		if violations := validateJSONAgainstSchema([]byte(`1`), schema); len(violations) != 1 {
			t.Fatalf("static anchor reference not enforced: %v", violations)
		}
	})

	t.Run("local reference registers hidden embedded resource", func(t *testing.T) {
		schema, err := parseArtifactSchema([]byte(`{
			"$ref":"#/hidden",
			"hidden":{"$id":"nested.json","type":"string"}
		}`))
		if err != nil {
			t.Fatalf("hidden embedded resource rejected: %v", err)
		}
		if violations := validateJSONAgainstSchema([]byte(`1`), schema); len(violations) != 1 || violations[0].Keyword != "/hidden/type" {
			t.Fatalf("hidden embedded resource not enforced: %v", violations)
		}
	})

	t.Run("local reference preserves embedded resource draft", func(t *testing.T) {
		schema, err := parseArtifactSchema([]byte(`{
			"$schema":"https://json-schema.org/draft/2020-12/schema",
			"$defs":{"legacy":{
				"$schema":"http://json-schema.org/draft-07/schema#",
				"$id":"legacy.json",
				"hidden":{"items":[{"type":"string"}]}
			}},
			"$ref":"legacy.json#/hidden"
		}`))
		if err != nil {
			t.Fatalf("mixed-draft local reference rejected: %v", err)
		}
		if violations := validateJSONAgainstSchema([]byte(`["ok"]`), schema); len(violations) != 0 {
			t.Fatalf("valid draft-07 tuple rejected: %v", violations)
		}
		if violations := validateJSONAgainstSchema([]byte(`[1]`), schema); len(violations) != 1 {
			t.Fatalf("invalid draft-07 tuple accepted: %v", violations)
		}
	})

	t.Run("embedded resource draft does not replace enclosing draft", func(t *testing.T) {
		schema, err := parseArtifactSchema([]byte(`{
			"$schema":"https://json-schema.org/draft/2020-12/schema",
			"$defs":{"legacy":{
				"$schema":"http://json-schema.org/draft-07/schema#",
				"$id":"legacy.json"
			}},
			"hidden":{"prefixItems":[{"$id":"nested.json","type":"string"}]},
			"$ref":"#/hidden"
		}`))
		if err != nil {
			t.Fatalf("mixed-draft enclosing reference rejected: %v", err)
		}
		if violations := validateJSONAgainstSchema([]byte(`["ok"]`), schema); len(violations) != 0 {
			t.Fatalf("valid enclosing-draft tuple rejected: %v", violations)
		}
		if violations := validateJSONAgainstSchema([]byte(`[1]`), schema); len(violations) != 1 {
			t.Fatalf("enclosing draft was not preserved: %v", violations)
		}
	})

	t.Run("draft-07 ref ignores sibling constraints", func(t *testing.T) {
		schema, err := parseArtifactSchema([]byte(`{
			"$schema":"http://json-schema.org/draft-07/schema#",
			"definitions":{"target":{"type":"string"}},
			"$ref":"#/definitions/target",
			"maxItems":2147483648,
			"if":{"$ref":"#"}
		}`))
		if err != nil {
			t.Fatalf("ignored draft-07 ref siblings rejected: %v", err)
		}
		if violations := validateJSONAgainstSchema([]byte(`"ok"`), schema); len(violations) != 0 {
			t.Fatalf("valid referenced value rejected: %v", violations)
		}
		if violations := validateJSONAgainstSchema([]byte(`1`), schema); len(violations) != 1 {
			t.Fatalf("draft-07 reference target not enforced: %v", violations)
		}
	})

	t.Run("draft-2020 ref retains sibling constraints", func(t *testing.T) {
		_, err := parseArtifactSchema([]byte(`{
			"$schema":"https://json-schema.org/draft/2020-12/schema",
			"$ref":"#/$defs/target",
			"$defs":{"target":{}},
			"maxItems":2147483648
		}`))
		if err == nil || !strings.Contains(err.Error(), "cardinality limit") {
			t.Fatalf("active draft-2020 ref sibling accepted: %v", err)
		}
	})

	t.Run("unreferenced instance data does not receive cardinality bounds", func(t *testing.T) {
		if _, err := parseArtifactSchema([]byte(`{"const":{"minItems":9223372036854775808}}`)); err != nil {
			t.Fatalf("instance data was treated as a schema: %v", err)
		}
	})

	t.Run("runtime rebound references fail preflight", func(t *testing.T) {
		cases := []string{
			`{"$schema":"https://json-schema.org/draft/2020-12/schema","$dynamicAnchor":"node","$dynamicRef":"#node"}`,
			`{"$schema":"https://json-schema.org/draft/2019-09/schema","$recursiveAnchor":true,"$recursiveRef":"#"}`,
		}
		for _, schemaJSON := range cases {
			if _, err := parseArtifactSchema([]byte(schemaJSON)); err == nil || !strings.Contains(err.Error(), "not supported by bounded artifact validation") {
				t.Fatalf("runtime-rebound reference accepted for %s: %v", schemaJSON, err)
			}
		}
	})

	t.Run("unique items equality work is bounded", func(t *testing.T) {
		schema, err := parseArtifactSchema([]byte(`{"type":"array","uniqueItems":true}`))
		if err != nil {
			t.Fatalf("schema parse failed: %v", err)
		}
		doc := `[` + strings.Repeat(`0,`, 4_999) + `1]`
		violations := validateJSONAgainstSchema([]byte(doc), schema)
		if len(violations) != 1 || violations[0].Keyword != "complexity" {
			t.Fatalf("expected uniqueItems work bound, got %v", violations)
		}
	})

	t.Run("aggregate schema numeric work fails before compilation", func(t *testing.T) {
		schemaJSON := `{"allOf":[` + strings.Repeat(`{"minimum":1e10000},`, 2_046) + `{"minimum":1e10000}]}`
		if _, err := parseArtifactSchema([]byte(schemaJSON)); err == nil || !strings.Contains(err.Error(), "numeric compilation safety budget") {
			t.Fatalf("aggregate schema numeric work accepted: %v", err)
		}
	})

	t.Run("schema object names bound compiled location growth", func(t *testing.T) {
		name := strings.Repeat("x", maxSchemaObjectNameBytes+1)
		_, err := parseArtifactSchema([]byte(`{"properties":{` + strconv.Quote(name) + `:{}}}`))
		if err == nil || !strings.Contains(err.Error(), "object name") {
			t.Fatalf("oversized schema object name accepted: %v", err)
		}
	})

	t.Run("schema resource URL length is bounded", func(t *testing.T) {
		id := "https://example.invalid/" + strings.Repeat("x", maxSchemaResourceURLBytes)
		_, err := parseArtifactSchema([]byte(`{"$id":` + strconv.Quote(id) + `}`))
		if err == nil || !strings.Contains(err.Error(), "resource ID") {
			t.Fatalf("oversized schema resource ID accepted: %v", err)
		}
	})

	t.Run("schema resource URL aggregate is bounded", func(t *testing.T) {
		rootID := "https://example.invalid/" + strings.Repeat("x", 1_700) + "/"
		var schemaJSON strings.Builder
		schemaJSON.WriteString(`{"$id":`)
		schemaJSON.WriteString(strconv.Quote(rootID))
		schemaJSON.WriteString(`,"allOf":[`)
		for i := 0; i < 700; i++ {
			if i > 0 {
				schemaJSON.WriteByte(',')
			}
			schemaJSON.WriteString(`{"$id":`)
			schemaJSON.WriteString(strconv.Quote("resource-" + strconv.Itoa(i)))
			schemaJSON.WriteByte('}')
		}
		schemaJSON.WriteString(`]}`)
		_, err := parseArtifactSchema([]byte(schemaJSON.String()))
		if err == nil || !strings.Contains(err.Error(), "aggregate limit") {
			t.Fatalf("oversized aggregate schema resource URLs accepted: %v", err)
		}
	})

	t.Run("schema reference URL length is bounded", func(t *testing.T) {
		reference := "https://example.invalid/" + strings.Repeat("x", maxSchemaResourceURLBytes)
		_, err := parseArtifactSchema([]byte(`{"$ref":` + strconv.Quote(reference) + `}`))
		if err == nil || !strings.Contains(err.Error(), "$ref URL") || strings.Contains(err.Error(), reference) {
			t.Fatalf("oversized schema reference URL accepted or echoed: %v", err)
		}
	})

	t.Run("schema reference URL aggregate is bounded", func(t *testing.T) {
		rootID := "https://example.invalid/" + strings.Repeat("x", 1_700) + "/"
		var schemaJSON strings.Builder
		schemaJSON.WriteString(`{"$id":`)
		schemaJSON.WriteString(strconv.Quote(rootID))
		schemaJSON.WriteString(`,"allOf":[`)
		for i := 0; i < 700; i++ {
			if i > 0 {
				schemaJSON.WriteByte(',')
			}
			schemaJSON.WriteString(`{"$ref":"target"}`)
		}
		schemaJSON.WriteString(`]}`)
		_, err := parseArtifactSchema([]byte(schemaJSON.String()))
		if err == nil || !strings.Contains(err.Error(), "reference URLs exceed") {
			t.Fatalf("oversized aggregate schema reference URLs accepted: %v", err)
		}
	})

	t.Run("required name bytes contribute validation weight", func(t *testing.T) {
		name := strings.Repeat("x", maxSchemaValidationWork)
		_, err := parseArtifactSchema([]byte(`{"required":[` + strconv.Quote(name) + `]}`))
		if err == nil || !strings.Contains(err.Error(), "validation safety budget") {
			t.Fatalf("oversized required operand accepted: %v", err)
		}
	})

	t.Run("regular expression program expansion is bounded", func(t *testing.T) {
		if _, err := parseArtifactSchema([]byte(`{"pattern":"(a?){1000}"}`)); err != nil {
			t.Fatalf("bounded repeated expression rejected: %v", err)
		}
		expression := strings.Repeat(`(a?){1000}`, 30)
		_, err := parseArtifactSchema([]byte(`{"pattern":` + strconv.Quote(expression) + `}`))
		if err == nil || !strings.Contains(err.Error(), "compiled-program safety limit") {
			t.Fatalf("expanded regular expression accepted: %v", err)
		}
	})

	t.Run("regular expression aggregate compilation is bounded", func(t *testing.T) {
		expression := `(a?){1000}`
		var schemaJSON strings.Builder
		schemaJSON.WriteString(`{"allOf":[`)
		for i := 0; i < 300; i++ {
			if i > 0 {
				schemaJSON.WriteByte(',')
			}
			schemaJSON.WriteString(`{"pattern":`)
			schemaJSON.WriteString(strconv.Quote(expression))
			schemaJSON.WriteByte('}')
		}
		schemaJSON.WriteString(`]}`)
		_, err := parseArtifactSchema([]byte(schemaJSON.String()))
		if err == nil || !strings.Contains(err.Error(), "aggregate compilation safety budget") {
			t.Fatalf("aggregate regular expression work accepted: %v", err)
		}
	})

	t.Run("artifact-controlled regex format is not asserted", func(t *testing.T) {
		_, err := parseArtifactSchema([]byte(`{
			"$schema":"http://json-schema.org/draft-07/schema#",
			"format":"regex"
		}`))
		if err == nil || !strings.Contains(err.Error(), `format "regex" is not supported`) {
			t.Fatalf("asserted legacy regex format accepted: %v", err)
		}
		_, err = parseArtifactSchema([]byte(`{
			"$schema":"http://json-schema.org/draft-07/schema#",
			"definitions":{"target":{"$id":"#target","format":"regex"}},
			"$ref":"#target"
		}`))
		if err == nil || !strings.Contains(err.Error(), `format "regex" is not supported`) {
			t.Fatalf("static-anchor regex format accepted: %v", err)
		}

		schema, err := parseArtifactSchema([]byte(`{
			"$schema":"https://json-schema.org/draft/2020-12/schema",
			"format":"regex"
		}`))
		if err != nil {
			t.Fatalf("modern regex annotation rejected: %v", err)
		}
		if violations := validateJSONAgainstSchema([]byte(`"["`), schema); len(violations) != 0 {
			t.Fatalf("modern regex annotation unexpectedly asserted: %v", violations)
		}
	})

	t.Run("referenced target obeys compiled subschema limit", func(t *testing.T) {
		schemaJSON := `{"$ref":"#/hidden","hidden":{"allOf":[` + strings.Repeat(`{},`, maxSchemaSubschemas) + `{ }]}}`
		_, err := parseArtifactSchema([]byte(schemaJSON))
		if err == nil || !strings.Contains(err.Error(), "2048-subschema") {
			t.Fatalf("referenced oversized compiled graph accepted: %v", err)
		}
	})

	t.Run("empty dependency entries still consume validation work", func(t *testing.T) {
		var schemaJSON strings.Builder
		schemaJSON.WriteString(`{"type":"array","items":{"type":"object","dependentRequired":{`)
		for i := 0; i < 1_000; i++ {
			if i > 0 {
				schemaJSON.WriteByte(',')
			}
			schemaJSON.WriteString(strconv.Quote("p" + strconv.Itoa(i)))
			schemaJSON.WriteString(`:[]`)
		}
		schemaJSON.WriteString(`}}}`)
		schema, err := parseArtifactSchema([]byte(schemaJSON.String()))
		if err != nil {
			t.Fatalf("schema parse failed: %v", err)
		}
		doc := `[` + strings.Repeat(`{},`, 99) + `{}` + `]`
		violations := validateJSONAgainstSchema([]byte(doc), schema)
		if len(violations) != 1 || violations[0].Keyword != "complexity" {
			t.Fatalf("expected dependency work bound, got %v", violations)
		}
	})

	t.Run("inactive keywords retain annotation semantics", func(t *testing.T) {
		cases := []struct {
			schema string
			doc    string
		}{
			{`{"$schema":"http://json-schema.org/draft-07/schema#","prefixItems":0}`, `null`},
			{`{"$schema":"https://json-schema.org/draft/2020-12/schema","additionalItems":0}`, `null`},
			{`{"$schema":"http://json-schema.org/draft-06/schema#","if":0}`, `null`},
			{`{"$schema":"https://json-schema.org/draft/2020-12/schema","dependencies":{"x":["y"]}}`, `{"x":1}`},
		}
		for _, tc := range cases {
			schema, err := parseArtifactSchema([]byte(tc.schema))
			if err != nil {
				t.Fatalf("inactive keyword rejected for %s: %v", tc.schema, err)
			}
			if violations := validateJSONAgainstSchema([]byte(tc.doc), schema); len(violations) != 0 {
				t.Fatalf("inactive keyword unexpectedly enforced for %s: %v", tc.schema, violations)
			}
		}
	})

	t.Run("inactive dependencies content does not trigger a schema load", func(t *testing.T) {
		schema, err := parseArtifactSchema([]byte(`{
			"$schema":"https://json-schema.org/draft/2020-12/schema",
			"dependencies":{"note":{"$schema":"https://invalid.example/schema"}}
		}`))
		if err != nil {
			t.Fatalf("inactive dependencies content was compiled: %v", err)
		}
		if violations := validateJSONAgainstSchema([]byte(`{"note":1}`), schema); len(violations) != 0 {
			t.Fatalf("inactive dependencies content was enforced: %v", violations)
		}
	})

	t.Run("legacy dependencies remain referenceable annotations", func(t *testing.T) {
		schema, err := parseArtifactSchema([]byte(`{
			"$schema":"https://json-schema.org/draft/2020-12/schema",
			"dependencies":{"legacy":{"type":"string"}},
			"$ref":"#/dependencies/legacy"
		}`))
		if err != nil {
			t.Fatalf("schema parse failed: %v", err)
		}
		violations := validateJSONAgainstSchema([]byte(`1`), schema)
		if len(violations) != 1 || violations[0].Keyword != "/dependencies/legacy/type" {
			t.Fatalf("expected referenced annotation schema to remain active, got %v", violations)
		}
	})

	t.Run("embedded older draft resource retains dependencies", func(t *testing.T) {
		schema, err := parseArtifactSchema([]byte(`{
			"$schema":"https://json-schema.org/draft/2020-12/schema",
			"properties":{"legacy":{
				"$schema":"http://json-schema.org/draft-07/schema#",
				"$id":"legacy.json",
				"dependencies":{"x":["y"]}
			}}
		}`))
		if err != nil {
			t.Fatalf("schema parse failed: %v", err)
		}
		violations := validateJSONAgainstSchema([]byte(`{"legacy":{"x":1}}`), schema)
		if len(violations) != 1 || violations[0].Keyword != "/properties/legacy/dependency/x" {
			t.Fatalf("expected embedded draft-07 dependency violation, got %v", violations)
		}
	})

	t.Run("active keyword shapes remain fail closed", func(t *testing.T) {
		cases := []string{
			`{"$schema":"https://json-schema.org/draft/2020-12/schema","prefixItems":0}`,
			`{"$schema":"https://json-schema.org/draft/2020-12/schema","items":[]}`,
			`{"$schema":"http://json-schema.org/draft-07/schema#","additionalItems":0}`,
		}
		for _, schemaJSON := range cases {
			if _, err := parseArtifactSchema([]byte(schemaJSON)); err == nil {
				t.Fatalf("active invalid keyword shape accepted: %s", schemaJSON)
			}
		}
	})
}

func TestValidateJSONAgainstDraft7AdditionalItemsReportsAbsoluteIndex(t *testing.T) {
	schema, err := parseArtifactSchema([]byte(`{
		"$schema":"http://json-schema.org/draft-07/schema#",
		"items":[{"type":"string"},{"type":"string"}],
		"additionalItems":{"type":"integer"}
	}`))
	if err != nil {
		t.Fatalf("schema parse failed: %v", err)
	}
	violations := validateJSONAgainstSchema([]byte(`["a","b","wrong"]`), schema)
	if len(violations) != 1 || violations[0].Path != "/2" {
		t.Fatalf("expected absolute additionalItems path /2, got %v", violations)
	}
}

func TestParseRequireArtifactSchemaSpec(t *testing.T) {
	t.Run("valid spec", func(t *testing.T) {
		remote, schema, err := parseRequireArtifactSchemaSpec("reports/out.json=schema.json")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if remote != "reports/out.json" || schema != "schema.json" {
			t.Fatalf("parsed spec wrong: remote=%q schema=%q", remote, schema)
		}
	})

	t.Run("missing equals is rejected", func(t *testing.T) {
		_, _, err := parseRequireArtifactSchemaSpec("reports/out.json")
		if err == nil {
			t.Fatalf("expected error for spec without '='")
		}
		assertExitCode(t, err, 2)
	})

	t.Run("unsafe remote path is rejected", func(t *testing.T) {
		if _, _, err := parseRequireArtifactSchemaSpec("/etc/passwd=schema.json"); err == nil {
			t.Fatalf("expected error for absolute remote path")
		}
		if _, _, err := parseRequireArtifactSchemaSpec("../secret.json=schema.json"); err == nil {
			t.Fatalf("expected error for parent-escaping remote path")
		}
	})

	t.Run("glob and Windows absolute paths are rejected", func(t *testing.T) {
		for _, spec := range []string{"reports/*.json=schema.json", "reports/out?.json=schema.json", "reports/[0-9].json=schema.json", "C:/secrets.json=schema.json", "-report.json=schema.json"} {
			if _, _, err := parseRequireArtifactSchemaSpec(spec); err == nil {
				t.Fatalf("expected exact relative path requirement for %q", spec)
			}
		}
	})
}

func TestLoadRequireArtifactSchemas(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.schema.json")
	if err := os.WriteFile(good, []byte(`{"type":"object","required":["x"]}`), 0o600); err != nil {
		t.Fatalf("write schema: %v", err)
	}
	bad := filepath.Join(dir, "bad.schema.json")
	if err := os.WriteFile(bad, []byte(`{"type":`), 0o600); err != nil {
		t.Fatalf("write schema: %v", err)
	}
	invalidUTF8 := filepath.Join(dir, "invalid-utf8.schema.json")
	if err := os.WriteFile(invalidUTF8, []byte{'{', '"', 't', 'i', 't', 'l', 'e', '"', ':', '"', 0xff, '"', '}'}, 0o600); err != nil {
		t.Fatalf("write invalid UTF-8 schema: %v", err)
	}

	t.Run("loads valid schema", func(t *testing.T) {
		loaded, err := loadRequireArtifactSchemas([]string{"out.json=" + good})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(loaded) != 1 || loaded[0].remote != "out.json" {
			t.Fatalf("loaded wrong: %+v", loaded)
		}
	})

	t.Run("missing schema file is exit 2", func(t *testing.T) {
		_, err := loadRequireArtifactSchemas([]string{"out.json=" + filepath.Join(dir, "nope.json")})
		assertExitCode(t, err, 2)
	})

	t.Run("malformed schema file is exit 2", func(t *testing.T) {
		_, err := loadRequireArtifactSchemas([]string{"out.json=" + bad})
		assertExitCode(t, err, 2)
	})

	t.Run("invalid UTF-8 schema file is exit 2", func(t *testing.T) {
		_, err := loadRequireArtifactSchemas([]string{"out.json=" + invalidUTF8})
		assertExitCode(t, err, 2)
	})

	t.Run("duplicate remote is exit 2", func(t *testing.T) {
		_, err := loadRequireArtifactSchemas([]string{"out.json=" + good, "out.json=" + good})
		assertExitCode(t, err, 2)
	})

	t.Run("oversized schema is exit 2", func(t *testing.T) {
		oversized := filepath.Join(dir, "oversized.schema.json")
		if err := os.WriteFile(oversized, bytes.Repeat([]byte(" "), maxSchemaDefinitionBytes+1), 0o600); err != nil {
			t.Fatalf("write oversized schema: %v", err)
		}
		_, err := loadRequireArtifactSchemas([]string{"out.json=" + oversized})
		assertExitCode(t, err, 2)
		if !strings.Contains(err.Error(), "1048576-byte limit") {
			t.Fatalf("expected bounded-schema diagnostic, got %v", err)
		}
	})
}

func TestParseArtifactSchemaSupportsStandardKeywords(t *testing.T) {
	cases := []struct {
		name    string
		schema  string
		valid   string
		invalid string
	}{
		{"pattern", `{"type":"string","pattern":"^x"}`, `"xyz"`, `"no"`},
		{"minimum", `{"type":"number","minimum":0}`, `0`, `-1`},
		{"additionalProperties", `{"type":"object","additionalProperties":false}`, `{}`, `{"x":1}`},
		{"anyOf", `{"anyOf":[{"type":"string"},{"type":"number"}]}`, `1`, `false`},
		{"local ref", `{"$defs":{"x":{"type":"string","maxLength":3}},"$ref":"#/$defs/x"}`, `"abc"`, `"long"`},
		{"nested maxLength", `{"type":"object","properties":{"x":{"type":"string","maxLength":3}}}`, `{"x":"abc"}`, `{"x":"long"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			schema, err := parseArtifactSchema([]byte(tc.schema))
			if err != nil {
				t.Fatalf("compile standard schema: %v", err)
			}
			if violations := validateJSONAgainstSchema([]byte(tc.valid), schema); len(violations) != 0 {
				t.Fatalf("valid document rejected: %v", violations)
			}
			if violations := validateJSONAgainstSchema([]byte(tc.invalid), schema); len(violations) == 0 {
				t.Fatalf("invalid document accepted")
			}
		})
	}
}

func TestParseArtifactSchemaDefaultsToDraft2020(t *testing.T) {
	schema, err := parseArtifactSchema([]byte(`{"type":"array","prefixItems":[{"type":"string"}],"items":false}`))
	if err != nil {
		t.Fatalf("compile draft 2020-12 schema: %v", err)
	}
	if violations := validateJSONAgainstSchema([]byte(`["x"]`), schema); len(violations) != 0 {
		t.Fatalf("draft 2020-12 prefixItems rejected: %v", violations)
	}
	if violations := validateJSONAgainstSchema([]byte(`["x",1]`), schema); len(violations) == 0 {
		t.Fatalf("draft 2020-12 items=false was not enforced")
	}
}

func TestParseArtifactSchemaRejectsExternalReferences(t *testing.T) {
	for _, ref := range []string{"https://example.com/schema.json", "other.json"} {
		_, err := parseArtifactSchema([]byte(`{"$ref":` + strconv.Quote(ref) + `}`))
		if err == nil || !strings.Contains(err.Error(), "external schema reference") {
			t.Fatalf("expected external reference %q to fail closed, got %v", ref, err)
		}
	}
}

func TestParseArtifactSchemaAcceptsAnnotationKeywords(t *testing.T) {
	data := []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"urn:crabbox:test","title":"t","description":"d","$comment":"c","examples":[1],"default":1,"deprecated":false,"type":"object","required":["a"]}`)
	if _, err := parseArtifactSchema(data); err != nil {
		t.Fatalf("annotation keywords should be accepted, got: %v", err)
	}
}

func TestDecodeBoundedBase64(t *testing.T) {
	data, err := decodeBoundedBase64(base64.StdEncoding.EncodeToString([]byte("hello")), 1024)
	if err != nil || string(data) != "hello" {
		t.Fatalf("decode within limit: data=%q err=%v", data, err)
	}
	oversized := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), 11))
	if _, err := decodeBoundedBase64(oversized, 10); err == nil {
		t.Fatalf("expected oversized payload to be rejected")
	}
}

func TestRemoteBoundedReadBase64CommandBoundsBytes(t *testing.T) {
	cmd := remoteBoundedReadBase64Command(SSHTarget{}, "/work", "out.json", 10)
	if !strings.Contains(cmd, "head -c 11 ") {
		t.Fatalf("expected bounded `head -c 11` in command, got: %s", cmd)
	}
}

func TestRemoteBoundedReadBase64CommandWindowsReadsToEOFOrLimit(t *testing.T) {
	target := SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeNormal}
	cmd := decodePowerShellCommand(t, remoteBoundedReadBase64Command(target, `C:\work`, "out.json", 10))
	for _, want := range []string{
		"while ($offset -lt $buffer.Length)",
		"$stream.Read($buffer, $offset, $buffer.Length - $offset)",
		"if ($read -eq 0) { break }",
		"ToBase64String($buffer, 0, $offset)",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("expected %q in bounded Windows command, got: %s", want, cmd)
		}
	}
}

func TestValidateArtifactSchemasWithReaderBehaviour(t *testing.T) {
	schema, err := parseArtifactSchema([]byte(`{"type":"object","required":["ok"],"properties":{"ok":{"type":"boolean"}}}`))
	if err != nil {
		t.Fatalf("schema parse: %v", err)
	}
	load := []loadedArtifactSchema{{remote: "out.json", schemaPath: "s.json", schema: schema}}

	t.Run("valid artifact passes with no failure", func(t *testing.T) {
		reader := func(_ context.Context, _ SSHTarget, _, _ string, _ int) ([]byte, error) {
			return []byte(`{"ok":true}`), nil
		}
		results, _, err := validateArtifactSchemasWithReader(context.Background(), SSHTarget{}, "/work", load, reader)
		if err != nil {
			t.Fatalf("expected no gate failure, got %v", err)
		}
		if len(results) != 1 || !results[0].Valid {
			t.Fatalf("expected one valid result, got %+v", results)
		}
	})

	t.Run("invalid content fails exit 7 with violations", func(t *testing.T) {
		reader := func(_ context.Context, _ SSHTarget, _, _ string, _ int) ([]byte, error) {
			return []byte(`{"ok":"nope"}`), nil
		}
		results, _, err := validateArtifactSchemasWithReader(context.Background(), SSHTarget{}, "/work", load, reader)
		assertExitCode(t, err, 7)
		if len(results) != 1 || results[0].Valid || len(results[0].Violations) == 0 {
			t.Fatalf("expected one invalid result with violations, got %+v", results)
		}
	})

	t.Run("invalid UTF-8 artifact fails exit 7", func(t *testing.T) {
		reader := func(_ context.Context, _ SSHTarget, _, _ string, _ int) ([]byte, error) {
			return []byte{'{', '"', 'o', 'k', '"', ':', 't', 'r', 'u', 'e', ',', '"', 'n', 'o', 't', 'e', '"', ':', '"', 0xff, '"', '}'}, nil
		}
		results, _, err := validateArtifactSchemasWithReader(context.Background(), SSHTarget{}, "/work", load, reader)
		assertExitCode(t, err, 7)
		if len(results) != 1 || results[0].Valid || len(results[0].Violations) == 0 {
			t.Fatalf("expected invalid UTF-8 artifact violation, got %+v", results)
		}
	})

	t.Run("fetch error fails exit 7", func(t *testing.T) {
		reader := func(_ context.Context, _ SSHTarget, _, _ string, _ int) ([]byte, error) {
			return nil, errors.New("connection refused")
		}
		results, _, err := validateArtifactSchemasWithReader(context.Background(), SSHTarget{}, "/work", load, reader)
		assertExitCode(t, err, 7)
		if len(results) != 1 || results[0].Error == "" {
			t.Fatalf("expected one result recording the fetch error, got %+v", results)
		}
	})

	t.Run("oversized artifact fails exit 7", func(t *testing.T) {
		reader := func(_ context.Context, _ SSHTarget, _, _ string, maxBytes int) ([]byte, error) {
			return decodeBoundedBase64(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), maxBytes+1)), maxBytes)
		}
		_, _, err := validateArtifactSchemasWithReader(context.Background(), SSHTarget{}, "/work", load, reader)
		assertExitCode(t, err, 7)
	})
}

func assertExitCode(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with exit code %d, got nil", want)
	}
	var exitErr ExitError
	if !AsExitError(err, &exitErr) {
		t.Fatalf("expected ExitError, got %T: %v", err, err)
	}
	if exitErr.Code != want {
		t.Fatalf("exit code = %d, want %d (%v)", exitErr.Code, want, err)
	}
}
