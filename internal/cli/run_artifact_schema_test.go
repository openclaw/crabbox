package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseArtifactSchema(t *testing.T) {
	t.Run("valid nested schema with ignored unknown keywords", func(t *testing.T) {
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
		if schema.Type != "object" || len(schema.Required) != 2 {
			t.Fatalf("parsed schema shape wrong: %+v", schema)
		}
		if schema.Properties["items"].Items == nil {
			t.Fatalf("nested array items schema not parsed")
		}
	})

	t.Run("invalid JSON is rejected", func(t *testing.T) {
		if _, err := parseArtifactSchema([]byte(`{"type":`)); err == nil {
			t.Fatalf("expected error for malformed schema JSON")
		}
	})

	t.Run("unknown type keyword is rejected", func(t *testing.T) {
		_, err := parseArtifactSchema([]byte(`{"type": "timestamp"}`))
		if err == nil || !strings.Contains(err.Error(), "unknown type") {
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
			wantPath: "count",
		},
		{
			name:     "wrong scalar type",
			doc:      `{"status":"passed","count":"three","items":[]}`,
			wantPath: "count",
		},
		{
			name:     "enum mismatch",
			doc:      `{"status":"skipped","count":1,"items":[]}`,
			wantPath: "status",
		},
		{
			name:     "nested object property wrong type",
			doc:      `{"status":"passed","count":1,"items":[],"config":{"retries":"nope"}}`,
			wantPath: "config.retries",
		},
		{
			name:     "array element violation reports index path",
			doc:      `{"status":"passed","count":1,"items":[{"name":"ok"},{"nope":true}]}`,
			wantPath: "items[1].name",
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
	if len(violations) != 1 || violations[0].Keyword != "type" {
		t.Fatalf("expected exactly one type violation, got %v", violations)
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

	t.Run("duplicate remote is exit 2", func(t *testing.T) {
		_, err := loadRequireArtifactSchemas([]string{"out.json=" + good, "out.json=" + good})
		assertExitCode(t, err, 2)
	})
}

func TestParseArtifactSchemaFailsClosedOnUnsupportedKeywords(t *testing.T) {
	cases := []struct {
		name   string
		schema string
	}{
		{"pattern", `{"type":"string","pattern":"^x"}`},
		{"minimum", `{"type":"number","minimum":0}`},
		{"additionalProperties", `{"type":"object","additionalProperties":false}`},
		{"anyOf", `{"anyOf":[{"type":"string"}]}`},
		{"ref", `{"$ref":"#/definitions/x"}`},
		{"nested unsupported keyword", `{"type":"object","properties":{"x":{"type":"string","maxLength":3}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseArtifactSchema([]byte(tc.schema)); err == nil {
				t.Fatalf("expected %s schema to be rejected fail-closed", tc.name)
			}
		})
	}
}

func TestParseArtifactSchemaAcceptsAnnotationKeywords(t *testing.T) {
	data := []byte(`{"$schema":"x","$id":"y","title":"t","description":"d","$comment":"c","examples":[1],"default":1,"deprecated":false,"type":"object","required":["a"]}`)
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
