package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	jsonschema "github.com/steipete/jsonschema/v6"
)

func TestAWSQualificationSchemaRejectsInvalidOverlayAndCacheTuples(t *testing.T) {
	schemaPath := filepath.Join("..", "..", "recipes", "aws", "v1", "qualification.schema.json")
	schemaBytes, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	schemaDocument, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	const schemaURL = "https://crabbox.invalid/aws-qualification.schema.json"
	if err := compiler.AddResource(schemaURL, schemaDocument); err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile(schemaURL)
	if err != nil {
		t.Fatal(err)
	}

	valid := validAWSQualificationDocument()
	if err := compiled.Validate(valid); err != nil {
		t.Fatalf("valid qualification rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "clean files", mutate: func(value map[string]any) {
			overlay(value, "cleanOverlay")["trackedTransfer"].(map[string]any)["files"] = float64(1)
		}},
		{name: "clean bytes", mutate: func(value map[string]any) {
			overlay(value, "cleanOverlay")["trackedTransfer"].(map[string]any)["bytes"] = float64(1)
		}},
		{name: "clean fixture digest", mutate: func(value map[string]any) {
			overlay(value, "cleanOverlay")["fixtureDigest"] = digest("9")
		}},
		{name: "dirty files", mutate: func(value map[string]any) {
			overlay(value, "dirtyOverlay")["trackedTransfer"].(map[string]any)["files"] = float64(0)
		}},
		{name: "dirty bytes", mutate: func(value map[string]any) {
			overlay(value, "dirtyOverlay")["trackedTransfer"].(map[string]any)["bytes"] = float64(0)
		}},
		{name: "dirty fixture digest", mutate: func(value map[string]any) {
			delete(overlay(value, "dirtyOverlay"), "fixtureDigest")
		}},
		{name: "advertised cache skip", mutate: func(value map[string]any) {
			value["cache"] = map[string]any{
				"advertised": true,
				"status":     "skipped",
				"reason":     "verified",
			}
		}},
		{name: "unadvertised cache pass", mutate: func(value map[string]any) {
			value["cache"] = map[string]any{
				"advertised": false,
				"status":     "passed",
				"reason":     "verified",
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := cloneQualificationDocument(t, valid)
			test.mutate(value)
			if err := compiled.Validate(value); err == nil {
				t.Fatal("invalid qualification unexpectedly matched schema")
			}
		})
	}
}

func validAWSQualificationDocument() map[string]any {
	identity := map[string]any{
		"schema":          "crabbox-ready-pool-identity/v1",
		"profile":         "linux-builder",
		"recipeDigest":    digest("2"),
		"inventoryDigest": digest("4"),
		"imageID":         "ami-123abc",
		"architecture":    "amd64",
		"seedDigest":      digest("5"),
		"cacheABIDigest":  digest("6"),
	}
	return map[string]any{
		"schema":    "crabbox-aws-image-qualification/v1",
		"createdAt": "2026-08-21T00:00:00Z",
		"candidate": map[string]any{
			"artifactDigest":           digest("1"),
			"candidateRecordDigest":    digest("3"),
			"qualificationInputDigest": digest("7"),
		},
		"target": map[string]any{
			"amiId": "ami-123abc", "region": "us-west-2", "instanceType": "m7i.large",
			"architecture": "x86_64", "osSelector": "ubuntu:26.04", "market": "on-demand",
			"profile": "linux-builder", "recipeDigest": digest("2"),
		},
		"pool": map[string]any{"identity": identity, "identityDigest": digest("8")},
		"gates": map[string]any{
			"negative": []any{
				map[string]any{"dimension": "ami", "status": "passed"},
				map[string]any{"dimension": "architecture", "status": "passed"},
				map[string]any{"dimension": "recipe", "status": "passed"},
				map[string]any{"dimension": "type", "status": "passed"},
			},
			"positive":   map[string]any{"status": "passed"},
			"osSelector": map[string]any{"status": "passed"},
			"cleanOverlay": map[string]any{
				"status": "passed", "mode": "overlay", "fallback": false,
				"trackedTransfer": map[string]any{"files": float64(0), "bytes": float64(0)},
			},
			"dirtyOverlay": map[string]any{
				"status": "passed", "mode": "overlay", "fallback": false,
				"trackedTransfer": map[string]any{"files": float64(1), "bytes": float64(40)},
				"fixtureDigest":   digest("9"),
			},
		},
		"cache":   map[string]any{"advertised": false, "status": "skipped", "reason": "provider_capability_not_advertised"},
		"cleanup": map[string]any{"status": "passed"},
		"workflow": map[string]any{
			"identity":   "example-org/crabbox/.github/workflows/devtools-image-qualify.yml@refs/heads/main",
			"runId":      "123",
			"runAttempt": "1",
		},
	}
}

func overlay(value map[string]any, name string) map[string]any {
	return value["gates"].(map[string]any)[name].(map[string]any)
}

func cloneQualificationDocument(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func digest(digit string) string {
	return "sha256:" + strings.Repeat(digit, 64)
}
