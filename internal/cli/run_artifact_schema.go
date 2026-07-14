package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"sort"
	"strings"
)

type artifactSchema struct {
	Type       string                    `json:"type"`
	Required   []string                  `json:"required"`
	Properties map[string]artifactSchema `json:"properties"`
	Items      *artifactSchema           `json:"items"`
	Enum       []interface{}             `json:"enum"`
	AnnotationSchema      json.RawMessage `json:"$schema,omitempty"`
	AnnotationID          json.RawMessage `json:"$id,omitempty"`
	AnnotationComment     json.RawMessage `json:"$comment,omitempty"`
	AnnotationTitle       json.RawMessage `json:"title,omitempty"`
	AnnotationDescription json.RawMessage `json:"description,omitempty"`
	AnnotationExamples    json.RawMessage `json:"examples,omitempty"`
	AnnotationDefault     json.RawMessage `json:"default,omitempty"`
	AnnotationDeprecated  json.RawMessage `json:"deprecated,omitempty"`
}

type schemaViolation struct {
	Path    string
	Keyword string
	Message string
}

func (v schemaViolation) String() string {
	loc := v.Path
	if loc == "" {
		loc = "(root)"
	}
	return loc + ": " + v.Message
}

type SchemaValidationResult struct {
	Artifact   string   `json:"artifact"`
	Schema     string   `json:"schema,omitempty"`
	Valid      bool     `json:"valid"`
	Violations []string `json:"violations,omitempty"`
	Error      string   `json:"error,omitempty"`
}

var knownSchemaTypes = map[string]bool{
	"object":  true,
	"array":   true,
	"string":  true,
	"number":  true,
	"integer": true,
	"boolean": true,
	"null":    true,
}

func parseArtifactSchema(data []byte) (artifactSchema, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var s artifactSchema
	if err := decoder.Decode(&s); err != nil {
		return artifactSchema{}, fmt.Errorf("%w (supported keywords: type, required, properties, items, enum)", err)
	}
	if err := validateSchemaShape(s, ""); err != nil {
		return artifactSchema{}, err
	}
	return s, nil
}

func validateSchemaShape(s artifactSchema, path string) error {
	if s.Type != "" && !knownSchemaTypes[s.Type] {
		return fmt.Errorf("unknown type %q at %s", s.Type, schemaPathOrRoot(path))
	}
	for _, key := range sortedSchemaKeys(s.Properties) {
		if err := validateSchemaShape(s.Properties[key], schemaJoinPath(path, key)); err != nil {
			return err
		}
	}
	if s.Items != nil {
		if err := validateSchemaShape(*s.Items, path+"[]"); err != nil {
			return err
		}
	}
	return nil
}

func validateJSONAgainstSchema(doc []byte, schema artifactSchema) []schemaViolation {
	var value interface{}
	if err := json.Unmarshal(doc, &value); err != nil {
		return []schemaViolation{{Keyword: "json", Message: fmt.Sprintf("artifact is not valid JSON: %v", err)}}
	}
	var out []schemaViolation
	validateSchemaValue(value, schema, "", &out)
	return out
}

func validateSchemaValue(value interface{}, schema artifactSchema, path string, out *[]schemaViolation) {
	if schema.Type != "" && !schemaTypeMatches(schema.Type, value) {
		*out = append(*out, schemaViolation{
			Path:    path,
			Keyword: "type",
			Message: fmt.Sprintf("expected type %s, got %s", schema.Type, schemaTypeName(value)),
		})
		return
	}
	if len(schema.Enum) > 0 && !schemaEnumContains(schema.Enum, value) {
		*out = append(*out, schemaViolation{
			Path:    path,
			Keyword: "enum",
			Message: fmt.Sprintf("value %s is not one of the allowed values", schemaCompactJSON(value)),
		})
		return
	}
	switch v := value.(type) {
	case map[string]interface{}:
		for _, req := range schema.Required {
			if _, ok := v[req]; !ok {
				*out = append(*out, schemaViolation{
					Path:    schemaJoinPath(path, req),
					Keyword: "required",
					Message: fmt.Sprintf("missing required property %q", req),
				})
			}
		}
		for _, key := range sortedSchemaKeys(schema.Properties) {
			if child, ok := v[key]; ok {
				validateSchemaValue(child, schema.Properties[key], schemaJoinPath(path, key), out)
			}
		}
	case []interface{}:
		if schema.Items != nil {
			for i, item := range v {
				validateSchemaValue(item, *schema.Items, fmt.Sprintf("%s[%d]", path, i), out)
			}
		}
	}
}

func schemaTypeMatches(t string, value interface{}) bool {
	switch t {
	case "object":
		_, ok := value.(map[string]interface{})
		return ok
	case "array":
		_, ok := value.([]interface{})
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		f, ok := value.(float64)
		return ok && f == math.Trunc(f)
	default:
		return true
	}
}

func schemaTypeName(value interface{}) string {
	switch value.(type) {
	case map[string]interface{}:
		return "object"
	case []interface{}:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func schemaEnumContains(enum []interface{}, value interface{}) bool {
	for _, candidate := range enum {
		if reflect.DeepEqual(candidate, value) {
			return true
		}
	}
	return false
}

func schemaCompactJSON(value interface{}) string {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(b)
}

func schemaJoinPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

func schemaPathOrRoot(path string) string {
	if path == "" {
		return "(root)"
	}
	return path
}

func sortedSchemaKeys(m map[string]artifactSchema) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type loadedArtifactSchema struct {
	remote     string
	schemaPath string
	schema     artifactSchema
}

func parseRequireArtifactSchemaSpec(value string) (remote, schemaPath string, err error) {
	remote, schemaPath, ok := strings.Cut(strings.TrimSpace(value), "=")
	remote = strings.TrimSpace(remote)
	schemaPath = strings.TrimSpace(schemaPath)
	if !ok || remote == "" || schemaPath == "" {
		return "", "", exit(2, "--require-artifact-schema expects remote=schema.json")
	}
	if err := validateRequiredRunArtifactGlobs([]string{remote}); err != nil {
		return "", "", err
	}
	return remote, schemaPath, nil
}

func loadRequireArtifactSchemas(values []string) ([]loadedArtifactSchema, error) {
	out := make([]loadedArtifactSchema, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		remote, schemaPath, err := parseRequireArtifactSchemaSpec(value)
		if err != nil {
			return nil, err
		}
		if seen[remote] {
			return nil, exit(2, "--require-artifact-schema lists %q more than once", remote)
		}
		seen[remote] = true
		data, err := os.ReadFile(schemaPath)
		if err != nil {
			return nil, exit(2, "--require-artifact-schema: read schema %s: %v", schemaPath, err)
		}
		schema, err := parseArtifactSchema(data)
		if err != nil {
			return nil, exit(2, "--require-artifact-schema: invalid schema %s: %v", schemaPath, err)
		}
		out = append(out, loadedArtifactSchema{remote: remote, schemaPath: schemaPath, schema: schema})
	}
	return out, nil
}

const maxSchemaArtifactBytes = 5 * 1024 * 1024

type remoteArtifactReader func(ctx context.Context, target SSHTarget, workdir, remote string, maxBytes int) ([]byte, error)

func validateRemoteArtifactSchemas(ctx context.Context, target SSHTarget, workdir string, schemas []loadedArtifactSchema) ([]SchemaValidationResult, string, error) {
	return validateArtifactSchemasWithReader(ctx, target, workdir, schemas, readRemoteArtifactBytes)
}

func validateArtifactSchemasWithReader(ctx context.Context, target SSHTarget, workdir string, schemas []loadedArtifactSchema, read remoteArtifactReader) ([]SchemaValidationResult, string, error) {
	results := make([]SchemaValidationResult, 0, len(schemas))
	var lines []string
	var firstFailure error
	for _, s := range schemas {
		result := SchemaValidationResult{Artifact: s.remote, Schema: s.schemaPath}
		data, err := read(ctx, target, workdir, s.remote, maxSchemaArtifactBytes)
		if err != nil {
			result.Valid = false
			result.Error = "fetch failed"
			results = append(results, result)
			lines = append(lines, fmt.Sprintf("schema %s: fetch failed: %v", s.remote, err))
			if firstFailure == nil {
				firstFailure = exit(7, "require artifact schema: fetch %s: %v", s.remote, err)
			}
			continue
		}
		violations := validateJSONAgainstSchema(data, s.schema)
		result.Valid = len(violations) == 0
		if result.Valid {
			results = append(results, result)
			lines = append(lines, fmt.Sprintf("schema %s: ok (%s)", s.remote, s.schemaPath))
			continue
		}
		for _, v := range violations {
			result.Violations = append(result.Violations, v.String())
		}
		results = append(results, result)
		lines = append(lines, fmt.Sprintf("schema %s: failed %d check(s) against %s:", s.remote, len(violations), s.schemaPath))
		for _, v := range violations {
			lines = append(lines, "  - "+v.String())
		}
		if firstFailure == nil {
			firstFailure = exit(7, "artifact schema validation failed: %s (%d violation(s))", s.remote, len(violations))
		}
	}
	return results, strings.Join(lines, "\n"), firstFailure
}

func readRemoteArtifactBytes(ctx context.Context, target SSHTarget, workdir, remote string, maxBytes int) ([]byte, error) {
	encoded, err := runSSHOutput(ctx, target, remoteBoundedReadBase64Command(target, workdir, remote, maxBytes))
	if err != nil {
		return nil, err
	}
	return decodeBoundedBase64(encoded, maxBytes)
}

func decodeBoundedBase64(encoded string, maxBytes int) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(encoded), ""))
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("artifact exceeds the %d-byte validation limit", maxBytes)
	}
	return data, nil
}

func remoteBoundedReadBase64Command(target SSHTarget, workdir, remotePath string, maxBytes int) string {
	limit := maxBytes + 1
	if isWindowsNativeTarget(target) {
		return powershellCommand(`$ErrorActionPreference = "Stop"
Set-Location -LiteralPath ` + psQuote(workdir) + `
$path = ` + psQuote(remotePath) + `
if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "artifact not found: $path" }
$stream = [System.IO.File]::OpenRead((Resolve-Path -LiteralPath $path).Path)
try {
  $buffer = New-Object byte[] ` + fmt.Sprint(limit) + `
  $read = $stream.Read($buffer, 0, $buffer.Length)
  [Convert]::ToBase64String($buffer, 0, $read)
} finally { $stream.Dispose() }`)
	}
	return fmt.Sprintf("cd %s && test -f %s && head -c %d %s | base64", shellQuote(workdir), shellQuote(remotePath), limit, shellQuote(remotePath))
}
