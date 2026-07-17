package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

type artifactSchema struct {
	Type       string
	Required   []string
	Properties map[string]artifactSchema
	Items      *artifactSchema
	Enum       []interface{}
	hasType    bool
	hasEnum    bool
	enumKeys   map[string]struct{}
}

type artifactSchemaWire struct {
	Type                  json.RawMessage `json:"type"`
	Required              json.RawMessage `json:"required"`
	Properties            json.RawMessage `json:"properties"`
	Items                 json.RawMessage `json:"items"`
	Enum                  json.RawMessage `json:"enum"`
	AnnotationSchema      json.RawMessage `json:"$schema"`
	AnnotationID          json.RawMessage `json:"$id"`
	AnnotationComment     json.RawMessage `json:"$comment"`
	AnnotationTitle       json.RawMessage `json:"title"`
	AnnotationDescription json.RawMessage `json:"description"`
	AnnotationExamples    json.RawMessage `json:"examples"`
	AnnotationDefault     json.RawMessage `json:"default"`
	AnnotationDeprecated  json.RawMessage `json:"deprecated"`
}

type schemaViolation struct {
	Path    string
	Keyword string
	Message string
}

const maxSchemaViolations = 100

type schemaViolationAccumulator struct {
	violations []schemaViolation
	truncated  bool
}

func (a *schemaViolationAccumulator) add(violation schemaViolation) {
	if a.truncated {
		return
	}
	if len(a.violations) >= maxSchemaViolations {
		a.truncated = true
		return
	}
	a.violations = append(a.violations, violation)
}

func (a *schemaViolationAccumulator) full() bool {
	return a.truncated
}

func (a *schemaViolationAccumulator) result() []schemaViolation {
	if a.truncated {
		a.violations = append(a.violations, schemaViolation{
			Keyword: "truncated",
			Message: fmt.Sprintf("additional violations omitted after the first %d", maxSchemaViolations),
		})
	}
	return a.violations
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
	if err := rejectDuplicateJSONNames(data); err != nil {
		return artifactSchema{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var s artifactSchema
	if err := decoder.Decode(&s); err != nil {
		return artifactSchema{}, fmt.Errorf("%w (supported keywords: type, required, properties, items, enum)", err)
	}
	if err := requireJSONDecoderEOF(decoder); err != nil {
		return artifactSchema{}, fmt.Errorf("%w (schema must contain exactly one JSON value)", err)
	}
	if err := validateSchemaShape(s, ""); err != nil {
		return artifactSchema{}, err
	}
	return s, nil
}

func (s *artifactSchema) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("schema must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire artifactSchemaWire
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	if err := requireJSONDecoderEOF(decoder); err != nil {
		return err
	}

	if wire.Type != nil {
		s.hasType = true
		if err := decodeSchemaKeyword("type", wire.Type, &s.Type, false); err != nil {
			return err
		}
	}
	if wire.Required != nil {
		if err := decodeSchemaKeyword("required", wire.Required, &s.Required, false); err != nil {
			return err
		}
	}
	if wire.Properties != nil {
		var properties map[string]json.RawMessage
		if err := decodeSchemaKeyword("properties", wire.Properties, &properties, false); err != nil {
			return err
		}
		s.Properties = make(map[string]artifactSchema, len(properties))
		for key, raw := range properties {
			var child artifactSchema
			if err := decodeSchemaKeyword("properties."+key, raw, &child, false); err != nil {
				return err
			}
			s.Properties[key] = child
		}
	}
	if wire.Items != nil {
		var items artifactSchema
		if err := decodeSchemaKeyword("items", wire.Items, &items, false); err != nil {
			return err
		}
		s.Items = &items
	}
	if wire.Enum != nil {
		s.hasEnum = true
		if err := decodeSchemaKeyword("enum", wire.Enum, &s.Enum, true); err != nil {
			return err
		}
		s.enumKeys = make(map[string]struct{}, len(s.Enum))
		for _, value := range s.Enum {
			key := schemaJSONKey(value)
			if _, exists := s.enumKeys[key]; exists {
				return fmt.Errorf("schema keyword %q must contain unique values", "enum")
			}
			s.enumKeys[key] = struct{}{}
		}
	}
	return nil
}

func rejectDuplicateJSONNames(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	return requireJSONDecoderEOF(decoder)
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("schema object contains a non-string key")
			}
			if seen[key] {
				return fmt.Errorf("schema contains duplicate object name %q", key)
			}
			seen[key] = true
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	return nil
}

func decodeSchemaKeyword(keyword string, raw json.RawMessage, dst interface{}, useNumber bool) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("schema keyword %q must not be null", keyword)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if useNumber {
		decoder.UseNumber()
	}
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid schema keyword %q: %w", keyword, err)
	}
	if err := requireJSONDecoderEOF(decoder); err != nil {
		return fmt.Errorf("invalid schema keyword %q: %w", keyword, err)
	}
	return nil
}

func requireJSONDecoderEOF(decoder *json.Decoder) error {
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return fmt.Errorf("unexpected trailing content: %w", err)
	}
	return nil
}

func validateSchemaShape(s artifactSchema, path string) error {
	if s.hasType && !knownSchemaTypes[s.Type] {
		return fmt.Errorf("unknown type %q at %s", s.Type, schemaPathOrRoot(path))
	}
	seenRequired := make(map[string]bool, len(s.Required))
	for _, key := range s.Required {
		if seenRequired[key] {
			return fmt.Errorf("duplicate required property %q at %s", key, schemaPathOrRoot(path))
		}
		seenRequired[key] = true
	}
	if s.hasEnum && len(s.Enum) == 0 {
		return fmt.Errorf("enum must contain at least one value at %s", schemaPathOrRoot(path))
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
	decoder := json.NewDecoder(bytes.NewReader(doc))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return []schemaViolation{{Keyword: "json", Message: fmt.Sprintf("artifact is not valid JSON: %v", err)}}
	}
	if err := requireJSONDecoderEOF(decoder); err != nil {
		return []schemaViolation{{Keyword: "json", Message: fmt.Sprintf("artifact is not valid JSON: %v", err)}}
	}
	var out schemaViolationAccumulator
	validateSchemaValue(value, schema, "", &out)
	return out.result()
}

func validateSchemaValue(value interface{}, schema artifactSchema, path string, out *schemaViolationAccumulator) {
	if schema.hasType && !schemaTypeMatches(schema.Type, value) {
		out.add(schemaViolation{
			Path:    path,
			Keyword: "type",
			Message: fmt.Sprintf("expected type %s, got %s", schema.Type, schemaTypeName(value)),
		})
		return
	}
	if schema.hasEnum && !schemaEnumContains(schema.enumKeys, value) {
		out.add(schemaViolation{
			Path:    path,
			Keyword: "enum",
			Message: "value is not one of the allowed values",
		})
		return
	}
	switch v := value.(type) {
	case map[string]interface{}:
		for _, req := range schema.Required {
			if out.full() {
				return
			}
			if _, ok := v[req]; !ok {
				out.add(schemaViolation{
					Path:    schemaJoinPath(path, req),
					Keyword: "required",
					Message: fmt.Sprintf("missing required property %q", req),
				})
			}
		}
		for _, key := range sortedSchemaKeys(schema.Properties) {
			if out.full() {
				return
			}
			if child, ok := v[key]; ok {
				validateSchemaValue(child, schema.Properties[key], schemaJoinPath(path, key), out)
			}
		}
	case []interface{}:
		if schema.Items != nil {
			for i, item := range v {
				if out.full() {
					return
				}
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
		_, ok := value.(json.Number)
		return ok
	case "integer":
		n, ok := value.(json.Number)
		if !ok {
			return false
		}
		canonical, ok := canonicalizeJSONNumber(n)
		return ok && canonical.isInteger()
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
	case json.Number:
		return "number"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", value)
	}
}

func schemaEnumContains(enumKeys map[string]struct{}, value interface{}) bool {
	_, ok := enumKeys[schemaJSONKey(value)]
	return ok
}

func schemaJSONKey(value interface{}) string {
	var out strings.Builder
	appendSchemaJSONKey(&out, value)
	return out.String()
}

func appendSchemaJSONKey(out *strings.Builder, value interface{}) {
	switch typed := value.(type) {
	case nil:
		out.WriteByte('z')
	case bool:
		if typed {
			out.WriteString("b1")
		} else {
			out.WriteString("b0")
		}
	case string:
		out.WriteByte('s')
		out.WriteString(strconv.Quote(typed))
	case json.Number:
		canonical, ok := canonicalizeJSONNumber(typed)
		if !ok {
			out.WriteString("invalid-number:")
			out.WriteString(typed.String())
			return
		}
		out.WriteByte('n')
		if canonical.negative {
			out.WriteByte('-')
		}
		out.WriteString(canonical.digits)
		out.WriteByte('e')
		out.WriteString(canonical.exponent.String())
		out.WriteByte(';')
	case []interface{}:
		out.WriteByte('[')
		for _, item := range typed {
			appendSchemaJSONKey(out, item)
			out.WriteByte(',')
		}
		out.WriteByte(']')
	case map[string]interface{}:
		out.WriteByte('{')
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			out.WriteString(strconv.Quote(key))
			out.WriteByte(':')
			appendSchemaJSONKey(out, typed[key])
			out.WriteByte(',')
		}
		out.WriteByte('}')
	}
}

type canonicalJSONNumber struct {
	negative bool
	digits   string
	exponent big.Int
}

func canonicalizeJSONNumber(number json.Number) (canonicalJSONNumber, bool) {
	text := number.String()
	negative := strings.HasPrefix(text, "-")
	if negative {
		text = text[1:]
	}

	exponent := new(big.Int)
	if index := strings.IndexAny(text, "eE"); index >= 0 {
		parsed, ok := new(big.Int).SetString(text[index+1:], 10)
		if !ok {
			return canonicalJSONNumber{}, false
		}
		exponent.Set(parsed)
		text = text[:index]
	}

	fractionDigits := 0
	if index := strings.IndexByte(text, '.'); index >= 0 {
		fractionDigits = len(text) - index - 1
		text = text[:index] + text[index+1:]
	}
	text = strings.TrimLeft(text, "0")
	if text == "" {
		return canonicalJSONNumber{digits: "0"}, true
	}

	exponent.Sub(exponent, new(big.Int).SetInt64(int64(fractionDigits)))
	trimmed := strings.TrimRight(text, "0")
	exponent.Add(exponent, new(big.Int).SetInt64(int64(len(text)-len(trimmed))))
	return canonicalJSONNumber{negative: negative, digits: trimmed, exponent: *exponent}, true
}

func (n canonicalJSONNumber) isInteger() bool {
	return n.digits == "0" || n.exponent.Sign() >= 0
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
	remote, err = normalizeArtifactSchemaRemotePath(remote)
	if err != nil {
		return "", "", err
	}
	return remote, schemaPath, nil
}

func normalizeArtifactSchemaRemotePath(remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	clean := path.Clean(remote)
	if remote == "" || clean == "." || !safeArtifactGlob(remote) || strings.ContainsAny(remote, "*?:\\[]") || strings.HasPrefix(remote, "/") {
		return "", exit(2, "--require-artifact-schema requires a safe relative artifact path: %s", remote)
	}
	return clean, nil
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
		violationSummary := schemaViolationSummary(violations)
		lines = append(lines, fmt.Sprintf("schema %s: failed %s against %s:", s.remote, violationSummary, s.schemaPath))
		for _, v := range violations {
			lines = append(lines, "  - "+v.String())
		}
		if firstFailure == nil {
			firstFailure = exit(7, "artifact schema validation failed: %s (%s)", s.remote, violationSummary)
		}
	}
	return results, strings.Join(lines, "\n"), firstFailure
}

func schemaViolationSummary(violations []schemaViolation) string {
	if len(violations) > 0 && violations[len(violations)-1].Keyword == "truncated" {
		return fmt.Sprintf("at least %d checks", maxSchemaViolations+1)
	}
	return fmt.Sprintf("%d check(s)", len(violations))
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
  $offset = 0
  while ($offset -lt $buffer.Length) {
    $read = $stream.Read($buffer, $offset, $buffer.Length - $offset)
    if ($read -eq 0) { break }
    $offset += $read
  }
  [Convert]::ToBase64String($buffer, 0, $offset)
} finally { $stream.Dispose() }`)
	}
	return fmt.Sprintf("cd %s && test -f %s && head -c %d %s | base64", shellQuote(workdir), shellQuote(remotePath), limit, shellQuote(remotePath))
}
