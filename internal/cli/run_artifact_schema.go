package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"os"
	"path"
	"reflect"
	"regexp"
	regexpsyntax "regexp/syntax"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	regexp2syntax "github.com/dlclark/regexp2/syntax"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	artifactSchemaResourceURL  = "https://crabbox.invalid/artifact-schema.json"
	maxSchemaViolations        = 100
	maxSchemaDefinitionBytes   = 1 * 1024 * 1024
	maxSchemaDefinitionValues  = 4_096
	maxSchemaObjectNameBytes   = 128
	maxSchemaResourceURLBytes  = 2_048
	maxSchemaResourceURLTotal  = 1 * 1024 * 1024
	maxSchemaArtifactBytes     = 5 * 1024 * 1024
	maxSchemaArtifactValues    = 100_000
	maxSchemaJSONDepth         = 64
	maxSchemaNumberCharacters  = 1_024
	maxSchemaNumberExponent    = 10_000
	maxSchemaSubschemas        = 2_048
	maxSchemaValidationWork    = 100_000
	maxSchemaValidationBytes   = 16 * 1024 * 1024
	maxSchemaNumericWork       = 16 * 1024 * 1024
	maxSchemaCardinality       = 2_147_483_647
	maxSchemaDiagnosticBytes   = 32 * 1024
	maxSchemaLocationBytes     = 1_024
	maxSchemaRegexpSourceBytes = 64 * 1024
	maxSchemaRegexpProgramWork = 100_000
	maxSchemaRegexpTotalWork   = 1_000_000
)

type artifactSchema struct {
	compiled                *jsonschema.Schema
	validationWeight        int
	numericValidationWeight int
	uniqueItemsWeight       int
}

type boundedECMARegexp struct {
	source   string
	compiled *regexp.Regexp
}

func (r *boundedECMARegexp) String() string {
	return r.source
}

func (r *boundedECMARegexp) MatchString(value string) bool {
	return r.compiled.MatchString(value)
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
	if v.Message != "" {
		return loc + ": " + v.Message
	}
	keyword := v.Keyword
	if keyword == "" {
		keyword = "/"
	}
	return fmt.Sprintf("%s: does not satisfy JSON Schema constraint %s", loc, keyword)
}

type SchemaValidationResult struct {
	Artifact   string   `json:"artifact"`
	Schema     string   `json:"schema,omitempty"`
	Valid      bool     `json:"valid"`
	Violations []string `json:"violations,omitempty"`
	Error      string   `json:"error,omitempty"`
}

type rejectingSchemaLoader struct{}

func (rejectingSchemaLoader) Load(rawURL string) (any, error) {
	return nil, fmt.Errorf("external schema reference %q is not allowed", rawURL)
}

func parseArtifactSchema(data []byte) (*artifactSchema, error) {
	schemaStats, err := scanJSONDocument(data, maxSchemaDefinitionValues, maxSchemaJSONDepth)
	if err != nil {
		return nil, err
	}
	if schemaStats.numberWork > maxSchemaNumericWork {
		return nil, fmt.Errorf("schema exceeds the %d-unit numeric compilation safety budget", maxSchemaNumericWork)
	}
	if schemaStats.maxObjectNameBytes > maxSchemaObjectNameBytes {
		return nil, fmt.Errorf("schema object name exceeds the %d-byte compilation safety limit", maxSchemaObjectNameBytes)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	complexity, err := validateSchemaImplementationBounds(doc)
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(rejectingSchemaLoader{})
	regexpWorkRemaining := maxSchemaRegexpTotalWork
	compiler.UseRegexpEngine(func(expression string) (jsonschema.Regexp, error) {
		work, err := boundedRegexpProgramWork(expression)
		if err != nil {
			return nil, err
		}
		if work > regexpWorkRemaining {
			return nil, fmt.Errorf("schema regular expressions exceed the %d-unit aggregate compilation safety budget", maxSchemaRegexpTotalWork)
		}
		regexpWorkRemaining -= work
		translated, err := translateECMARegexp(expression)
		if err != nil {
			return nil, err
		}
		compiled, err := regexp.Compile(translated)
		if err != nil {
			return nil, err
		}
		return &boundedECMARegexp{source: expression, compiled: compiled}, nil
	})
	if err := compiler.AddResource(artifactSchemaResourceURL, doc); err != nil {
		return nil, err
	}
	schema, err := compiler.Compile(artifactSchemaResourceURL)
	if err != nil {
		return nil, err
	}
	if err := validateCompiledArtifactSchemaCount(schema); err != nil {
		return nil, err
	}
	if err := validateCompiledArtifactSchemaFormats(schema); err != nil {
		return nil, err
	}
	if err := validateCompiledArtifactSchemaCardinalities(schema, complexity.resources); err != nil {
		return nil, err
	}
	validationWeight, err := compiledArtifactSchemaValidationWeight(schema)
	if err != nil {
		return nil, err
	}
	numericValidationWeight, err := compiledArtifactSchemaNumericWeight(schema)
	if err != nil {
		return nil, err
	}
	uniqueItemsWeight, err := compiledArtifactSchemaAssertionWeight(schema, func(schema *jsonschema.Schema) int {
		if schema.UniqueItems {
			return 1
		}
		return 0
	})
	if err != nil {
		return nil, err
	}
	return &artifactSchema{
		compiled:                schema,
		validationWeight:        validationWeight,
		numericValidationWeight: numericValidationWeight,
		uniqueItemsWeight:       uniqueItemsWeight,
	}, nil
}

func compiledArtifactSchemaValidationWeight(root *jsonschema.Schema) (int, error) {
	active := make(map[*jsonschema.Schema]bool)
	var cost func(*jsonschema.Schema) (int, error)
	add := func(total *int, delta int) error {
		if delta > maxSchemaValidationWork-*total {
			return fmt.Errorf("schema exceeds the %d-unit expanded validation safety budget", maxSchemaValidationWork)
		}
		*total += delta
		return nil
	}
	addSchema := func(total *int, schema *jsonschema.Schema) error {
		delta, err := cost(schema)
		if err != nil {
			return err
		}
		return add(total, delta)
	}
	addSchemas := func(total *int, schemas []*jsonschema.Schema) error {
		for _, schema := range schemas {
			if err := addSchema(total, schema); err != nil {
				return err
			}
		}
		return nil
	}
	cost = func(schema *jsonschema.Schema) (int, error) {
		if schema == nil {
			return 0, nil
		}
		if active[schema] {
			return 0, fmt.Errorf("recursive schema references are not supported by bounded artifact validation")
		}
		active[schema] = true
		defer delete(active, schema)
		if schema.DynamicRef != nil {
			return 0, fmt.Errorf("schema keyword %q is not supported by bounded artifact validation", "$dynamicRef")
		}
		if schema.RecursiveRef != nil {
			return 0, fmt.Errorf("schema keyword %q is not supported by bounded artifact validation", "$recursiveRef")
		}
		if schema.DraftVersion < 2019 && schema.Ref != nil {
			total := 1
			if err := addSchema(&total, schema.Ref); err != nil {
				return 0, err
			}
			return total, nil
		}

		total := 1
		for _, delta := range []int{
			len(schema.Required) + schemaStringBytes(schema.Required),
			len(schema.DependentRequired), len(schema.Dependencies),
		} {
			if err := add(&total, delta); err != nil {
				return 0, err
			}
		}
		for property, names := range schema.DependentRequired {
			if err := add(&total, len(property)+len(names)+schemaStringBytes(names)); err != nil {
				return 0, err
			}
		}
		for property, dependency := range schema.Dependencies {
			if err := add(&total, len(property)); err != nil {
				return 0, err
			}
			switch dependency := dependency.(type) {
			case []string:
				if err := add(&total, len(dependency)+schemaStringBytes(dependency)); err != nil {
					return 0, err
				}
			}
		}
		if schema.Types != nil {
			if err := add(&total, len(schema.Types.ToStrings())); err != nil {
				return 0, err
			}
		}
		if schema.Enum != nil {
			if err := add(&total, countJSONValues(schema.Enum.Values)+countJSONNumericWork(schema.Enum.Values)+countJSONStringBytes(schema.Enum.Values)); err != nil {
				return 0, err
			}
		}
		if schema.Const != nil {
			if err := add(&total, countJSONValues(*schema.Const)+countJSONNumericWork(*schema.Const)+countJSONStringBytes(*schema.Const)); err != nil {
				return 0, err
			}
		}
		if schema.Pattern != nil {
			work, err := boundedRegexpProgramWork(schema.Pattern.String())
			if err != nil {
				return 0, err
			}
			if err := add(&total, work); err != nil {
				return 0, err
			}
		}
		for expression := range schema.PatternProperties {
			work, err := boundedRegexpProgramWork(expression.String())
			if err != nil {
				return 0, err
			}
			if err := add(&total, work); err != nil {
				return 0, err
			}
		}
		for _, number := range []*big.Rat{
			schema.Maximum, schema.Minimum, schema.ExclusiveMaximum,
			schema.ExclusiveMinimum, schema.MultipleOf,
		} {
			if number == nil {
				continue
			}
			bytes := (number.Num().BitLen() + number.Denom().BitLen() + 7) / 8
			if err := add(&total, max(1, bytes)); err != nil {
				return 0, err
			}
		}
		if schema.UniqueItems {
			// jsonschema/v6 uses pairwise equality through 20 items, then a
			// structural hash. Charge the full small-array comparison window;
			// artifact values cover hashing and deep equality input size.
			if err := add(&total, 20); err != nil {
				return 0, err
			}
		}

		if err := addSchemas(&total, compiledArtifactSchemaChildren(schema)); err != nil {
			return 0, err
		}
		return total, nil
	}
	return cost(root)
}

func compiledArtifactSchemaNumericWeight(root *jsonschema.Schema) (int, error) {
	return compiledArtifactSchemaAssertionWeight(root, func(schema *jsonschema.Schema) int {
		total := 0
		if schema.Types != nil {
			for _, name := range schema.Types.ToStrings() {
				if name == "integer" || name == "number" {
					total++
				}
			}
		}
		if schema.Enum != nil {
			total += len(schema.Enum.Values)
		}
		if schema.Const != nil {
			total++
		}
		if schema.UniqueItems {
			total++
		}
		for _, number := range []*big.Rat{
			schema.Maximum, schema.Minimum, schema.ExclusiveMaximum,
			schema.ExclusiveMinimum, schema.MultipleOf,
		} {
			if number != nil {
				total++
			}
		}
		return total
	})
}

func compiledArtifactSchemaAssertionWeight(root *jsonschema.Schema, ownWeight func(*jsonschema.Schema) int) (int, error) {
	active := make(map[*jsonschema.Schema]bool)
	var cost func(*jsonschema.Schema) (int, error)
	cost = func(schema *jsonschema.Schema) (int, error) {
		if schema == nil {
			return 0, nil
		}
		if active[schema] {
			return 0, fmt.Errorf("recursive schema references are not supported by bounded artifact validation")
		}
		active[schema] = true
		defer delete(active, schema)

		total := 0
		if schema.DraftVersion >= 2019 || schema.Ref == nil {
			total = ownWeight(schema)
		}
		for _, child := range compiledArtifactSchemaChildren(schema) {
			childCost, err := cost(child)
			if err != nil {
				return 0, err
			}
			if childCost > maxSchemaValidationWork-total {
				return 0, fmt.Errorf("schema exceeds the %d-unit assertion validation safety budget", maxSchemaValidationWork)
			}
			total += childCost
		}
		return total, nil
	}
	return cost(root)
}

func compiledArtifactSchemaChildren(schema *jsonschema.Schema) []*jsonschema.Schema {
	if schema.DraftVersion < 2019 && schema.Ref != nil {
		return []*jsonschema.Schema{schema.Ref}
	}
	children := []*jsonschema.Schema{
		schema.Ref, schema.RecursiveRef, schema.Not, schema.If, schema.Then, schema.Else,
		schema.PropertyNames, schema.UnevaluatedProperties, schema.Contains,
		schema.Items2020, schema.UnevaluatedItems, schema.ContentSchema,
	}
	if schema.DynamicRef != nil {
		children = append(children, schema.DynamicRef.Ref)
	}
	children = append(children, schema.AllOf...)
	children = append(children, schema.AnyOf...)
	children = append(children, schema.OneOf...)
	children = append(children, schema.PrefixItems...)
	for _, schemas := range []map[string]*jsonschema.Schema{
		schema.Properties, schema.DependentSchemas,
	} {
		for _, child := range schemas {
			children = append(children, child)
		}
	}
	for _, child := range schema.PatternProperties {
		children = append(children, child)
	}
	for _, value := range []any{schema.AdditionalProperties, schema.Items, schema.AdditionalItems} {
		switch value := value.(type) {
		case *jsonschema.Schema:
			children = append(children, value)
		case []*jsonschema.Schema:
			children = append(children, value...)
		}
	}
	for _, dependency := range schema.Dependencies {
		if child, ok := dependency.(*jsonschema.Schema); ok {
			children = append(children, child)
		}
	}
	return children
}

func walkCompiledArtifactSchemaGraph(root *jsonschema.Schema, visit func(*jsonschema.Schema) error) error {
	visited := make(map[*jsonschema.Schema]bool)
	var walk func(*jsonschema.Schema) error
	walk = func(schema *jsonschema.Schema) error {
		if schema == nil || visited[schema] {
			return nil
		}
		visited[schema] = true
		for _, child := range compiledArtifactSchemaChildren(schema) {
			if err := walk(child); err != nil {
				return err
			}
		}
		return visit(schema)
	}
	return walk(root)
}

func validateCompiledArtifactSchemaCount(root *jsonschema.Schema) error {
	count := 0
	return walkCompiledArtifactSchemaGraph(root, func(*jsonschema.Schema) error {
		count++
		if count > maxSchemaSubschemas {
			return fmt.Errorf("schema exceeds the %d-subschema compiled-graph limit", maxSchemaSubschemas)
		}
		return nil
	})
}

func validateCompiledArtifactSchemaFormats(root *jsonschema.Schema) error {
	return walkCompiledArtifactSchemaGraph(root, func(schema *jsonschema.Schema) error {
		if schema.DraftVersion < 2019 && schema.Format != nil && schema.Format.Name == "regex" {
			return fmt.Errorf("schema format %q is not supported by bounded artifact validation", "regex")
		}
		return nil
	})
}

func validateCompiledArtifactSchemaCardinalities(root *jsonschema.Schema, resources map[string]any) error {
	return walkCompiledArtifactSchemaGraph(root, func(schema *jsonschema.Schema) error {
		raw, err := artifactSchemaAtCompiledLocation(resources, schema.Location)
		if err != nil {
			return err
		}
		object, ok := raw.(map[string]any)
		if !ok {
			return nil
		}
		if schema.DraftVersion < 2019 && schema.Ref != nil {
			return nil
		}
		for keyword := range cardinalitySchemaKeywords {
			if (keyword == "minContains" || keyword == "maxContains") && schema.DraftVersion < 2019 {
				continue
			}
			if err := validateSchemaCardinality(keyword, object[keyword]); err != nil {
				return fmt.Errorf("%s at %s", err, schema.Location)
			}
		}
		return nil
	})
}

func artifactSchemaAtCompiledLocation(resources map[string]any, location string) (any, error) {
	parsed, err := url.Parse(location)
	if err != nil {
		return nil, fmt.Errorf("invalid compiled schema location %q: %w", location, err)
	}
	pointer := parsed.Fragment
	parsed.Fragment = ""
	parsed.RawFragment = ""
	root, ok := resources[parsed.String()]
	if !ok {
		return nil, fmt.Errorf("compiled schema resource %q is outside the bounded schema document", parsed.String())
	}
	value, err := artifactSchemaAtJSONPointer(root, pointer)
	if err != nil {
		return nil, fmt.Errorf("compiled schema location %q is missing from the bounded schema document: %w", location, err)
	}
	return value, nil
}

func artifactSchemaAtJSONPointer(root any, pointer string) (any, error) {
	if pointer == "" {
		return root, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("location is not a JSON Pointer")
	}
	value := root
	for _, token := range strings.Split(pointer[1:], "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		ok := false
		switch current := value.(type) {
		case map[string]any:
			value, ok = current[token]
		case []any:
			index, err := strconv.Atoi(token)
			if err == nil && index >= 0 && index < len(current) {
				value = current[index]
				ok = true
			} else {
				ok = false
			}
		default:
			ok = false
		}
		if !ok {
			return nil, fmt.Errorf("JSON Pointer target is missing")
		}
	}
	return value, nil
}

type jsonDocumentStats struct {
	values             int
	stringBytes        int
	numberWork         int
	maxArrayLen        int
	maxObjectNameBytes int
}

var (
	errJSONValueLimit   = errors.New("JSON value limit")
	errJSONDepthLimit   = errors.New("JSON nesting-depth limit")
	errJSONNumericLimit = errors.New("JSON numeric complexity limit")
)

func validateArtifactJSONShape(data []byte) (jsonDocumentStats, error) {
	return scanJSONDocument(data, maxSchemaArtifactValues, maxSchemaJSONDepth)
}

func scanJSONDocument(data []byte, maxValues, maxDepth int) (jsonDocumentStats, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	stats := jsonDocumentStats{}
	if err := scanJSONValue(decoder, &stats, maxValues, maxDepth, 1); err != nil {
		return jsonDocumentStats{}, err
	}
	if err := requireJSONDecoderEOF(decoder); err != nil {
		return jsonDocumentStats{}, err
	}
	return stats, nil
}

func scanJSONValue(decoder *json.Decoder, stats *jsonDocumentStats, maxValues, maxDepth, depth int) error {
	if maxDepth > 0 && depth > maxDepth {
		return fmt.Errorf("%w: JSON exceeds the %d-level nesting-depth limit", errJSONDepthLimit, maxDepth)
	}
	stats.values++
	if maxValues > 0 && stats.values > maxValues {
		return fmt.Errorf("%w: JSON contains more than %d values", errJSONValueLimit, maxValues)
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if number, ok := token.(json.Number); ok {
		work, err := boundedJSONNumberWork(number)
		if err != nil {
			return fmt.Errorf("%w: %v", errJSONNumericLimit, err)
		}
		stats.numberWork += work
	}
	if text, ok := token.(string); ok {
		stats.stringBytes += len(text)
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
				return fmt.Errorf("JSON object contains a non-string name")
			}
			stats.stringBytes += len(key)
			stats.maxObjectNameBytes = max(stats.maxObjectNameBytes, len(key))
			if seen[key] {
				return fmt.Errorf("JSON contains duplicate object name %q", key)
			}
			seen[key] = true
			if err := scanJSONValue(decoder, stats, maxValues, maxDepth, depth+1); err != nil {
				return err
			}
		}
	case '[':
		length := 0
		for decoder.More() {
			length++
			if err := scanJSONValue(decoder, stats, maxValues, maxDepth, depth+1); err != nil {
				return err
			}
		}
		stats.maxArrayLen = max(stats.maxArrayLen, length)
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	return nil
}

func boundedJSONNumberWork(number json.Number) (int, error) {
	text := number.String()
	if len(text) > maxSchemaNumberCharacters {
		return 0, fmt.Errorf("JSON number exceeds the %d-character numeric complexity limit", maxSchemaNumberCharacters)
	}
	work := len(text)
	exponentAt := strings.IndexAny(text, "eE")
	if exponentAt < 0 {
		return work, nil
	}
	exponent, err := strconv.ParseInt(text[exponentAt+1:], 10, 64)
	if err != nil || exponent < -maxSchemaNumberExponent || exponent > maxSchemaNumberExponent {
		return 0, fmt.Errorf("JSON number exceeds the absolute exponent limit of %d", maxSchemaNumberExponent)
	}
	if exponent < 0 {
		exponent = -exponent
	}
	return work + int(exponent), nil
}

func boundedRegexpProgramWork(expression string) (int, error) {
	if len(expression) > maxSchemaRegexpSourceBytes {
		return 0, fmt.Errorf("regular expression exceeds the %d-byte compilation safety limit", maxSchemaRegexpSourceBytes)
	}
	translated, err := translateECMARegexp(expression)
	if err != nil {
		return 0, err
	}
	parsed, err := regexpsyntax.Parse(translated, regexpsyntax.Perl)
	if err != nil {
		return 0, err
	}
	work, ok := regexpProgramWork(parsed, maxSchemaRegexpProgramWork)
	if !ok {
		return 0, fmt.Errorf("regular expression exceeds the %d-unit compiled-program safety limit", maxSchemaRegexpProgramWork)
	}
	return max(1, work), nil
}

const ecmaWhitespaceClass = `\x{0009}-\x{000D}\x{0020}\x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}`

func translateECMARegexp(expression string) (string, error) {
	if _, err := regexp2syntax.Parse(expression, regexp2syntax.ECMAScript|regexp2syntax.Unicode); err != nil {
		return "", err
	}
	var translated strings.Builder
	translated.Grow(len(expression) + 32)
	inClass := false
	classFirst := false
	classHasContent := false
	for offset := 0; offset < len(expression); {
		character, width := utf8.DecodeRuneInString(expression[offset:])
		if character == utf8.RuneError && width == 1 {
			return "", fmt.Errorf("regular expression contains invalid UTF-8")
		}
		if character == '\\' {
			if offset+width >= len(expression) {
				return "", fmt.Errorf("regular expression ends with an escape")
			}
			escaped := expression[offset+width]
			switch {
			case escaped >= '1' && escaped <= '9', escaped == 'k':
				return "", fmt.Errorf("regular expression backreferences are not supported by bounded artifact validation")
			case escaped == 'p' || escaped == 'P':
				return "", fmt.Errorf("regular expression Unicode property escapes are not supported by bounded artifact validation")
			case escaped == 'c':
				if offset+width+1 >= len(expression) {
					return "", fmt.Errorf("regular expression has an incomplete control escape")
				}
				letter := expression[offset+width+1]
				if !((letter >= 'A' && letter <= 'Z') || (letter >= 'a' && letter <= 'z')) {
					return "", fmt.Errorf("regular expression has an invalid control escape")
				}
				fmt.Fprintf(&translated, `\x{%X}`, letter&0x1f)
				offset += width + 2
				classFirst = false
				classHasContent = inClass
				continue
			case escaped == 'u':
				valueOffset := offset + width + 1
				valueEnd := valueOffset + 4
				braced := false
				if valueOffset < len(expression) && expression[valueOffset] == '{' {
					braced = true
					valueOffset++
					closing := strings.IndexByte(expression[valueOffset:], '}')
					if closing < 0 {
						return "", fmt.Errorf("regular expression has an incomplete Unicode escape")
					}
					valueEnd = valueOffset + closing
					offset = valueEnd + 1
				} else {
					if valueEnd > len(expression) {
						return "", fmt.Errorf("regular expression has an incomplete Unicode escape")
					}
					offset = valueEnd
				}
				codepoint, err := strconv.ParseUint(expression[valueOffset:valueEnd], 16, 32)
				if err != nil || codepoint > unicode.MaxRune {
					return "", fmt.Errorf("regular expression Unicode escape is outside the supported scalar range")
				}
				if !braced && codepoint >= 0xD800 && codepoint <= 0xDBFF {
					if offset+6 > len(expression) || !strings.HasPrefix(expression[offset:], `\u`) {
						return "", fmt.Errorf("regular expression Unicode escape has an unpaired lead surrogate")
					}
					trail, trailErr := strconv.ParseUint(expression[offset+2:offset+6], 16, 16)
					if trailErr != nil || trail < 0xDC00 || trail > 0xDFFF {
						return "", fmt.Errorf("regular expression Unicode escape has an unpaired lead surrogate")
					}
					codepoint = 0x10000 + (codepoint-0xD800)<<10 + trail - 0xDC00
					offset += 6
				} else if codepoint >= 0xD800 && codepoint <= 0xDFFF {
					return "", fmt.Errorf("regular expression Unicode escape has an unpaired surrogate")
				}
				fmt.Fprintf(&translated, `\x{%X}`, codepoint)
				classFirst = false
				classHasContent = inClass
				continue
			case escaped == 's':
				if inClass {
					translated.WriteString(ecmaWhitespaceClass)
				} else {
					translated.WriteByte('[')
					translated.WriteString(ecmaWhitespaceClass)
					translated.WriteByte(']')
				}
			case escaped == 'S':
				if inClass {
					return "", fmt.Errorf("regular expression \\S inside a character class is not supported by bounded artifact validation")
				}
				translated.WriteString(`[^` + ecmaWhitespaceClass + `]`)
			case escaped == 'b' && inClass:
				translated.WriteString(`\x{8}`)
			case escaped == '0':
				translated.WriteString(`\x{0}`)
			case escaped == '/':
				translated.WriteByte('/')
			case escaped == '-':
				translated.WriteString(`\x{2D}`)
			case strings.ContainsRune(`dDwWbBfnrtv\\.^$|?*+()[]{} `, rune(escaped)):
				translated.WriteByte('\\')
				translated.WriteByte(escaped)
			case escaped == 'x':
				if offset+width+3 > len(expression) {
					return "", fmt.Errorf("regular expression has an incomplete hexadecimal escape")
				}
				translated.WriteString(expression[offset : offset+width+3])
				offset += width + 3
				classFirst = false
				classHasContent = inClass
				continue
			default:
				return "", fmt.Errorf("regular expression escape \\%c is not supported by bounded artifact validation", escaped)
			}
			offset += width + 1
			classFirst = false
			if inClass {
				classHasContent = true
			}
			continue
		}
		if !inClass && character == '(' && strings.HasPrefix(expression[offset:], "(?:") {
			translated.WriteByte('(')
			offset += 3
			continue
		}
		if !inClass && character == '(' && strings.HasPrefix(expression[offset:], "(?") {
			return "", fmt.Errorf("regular expression lookarounds and inline options are not supported by bounded artifact validation")
		}
		if !inClass && character == '.' {
			translated.WriteString(`[^\n\r\x{2028}\x{2029}]`)
			offset += width
			continue
		}
		if !inClass && character == '$' {
			translated.WriteRune(character)
			offset += width
			continue
		}
		if character == '[' && !inClass {
			inClass = true
			classFirst = true
			classHasContent = false
			translated.WriteRune(character)
			offset += width
			continue
		}
		if character == ']' && inClass {
			if !classHasContent {
				return "", fmt.Errorf("empty regular expression character classes are not supported by bounded artifact validation")
			} else {
				translated.WriteRune(character)
				inClass = false
			}
			offset += width
			continue
		}
		translated.WriteRune(character)
		if inClass {
			if classFirst && character == '^' {
				classFirst = false
			} else {
				classFirst = false
				classHasContent = true
			}
		}
		offset += width
	}
	return translated.String(), nil
}

func regexpProgramWork(expression *regexpsyntax.Regexp, limit int) (int, bool) {
	add := func(total, delta int) (int, bool) {
		if delta > limit-total {
			return 0, false
		}
		return total + delta, true
	}
	walkChildren := func(children []*regexpsyntax.Regexp) (int, bool) {
		total := 0
		for _, child := range children {
			childWork, ok := regexpProgramWork(child, limit)
			if !ok {
				return 0, false
			}
			total, ok = add(total, childWork)
			if !ok {
				return 0, false
			}
		}
		return total, true
	}

	switch expression.Op {
	case regexpsyntax.OpLiteral:
		return min(limit, max(1, len(expression.Rune))), len(expression.Rune) <= limit
	case regexpsyntax.OpCharClass:
		return min(limit, 1+len(expression.Rune)), len(expression.Rune) < limit
	case regexpsyntax.OpCapture:
		childWork, ok := regexpProgramWork(expression.Sub[0], limit)
		if !ok {
			return 0, false
		}
		return add(childWork, 2)
	case regexpsyntax.OpConcat:
		return walkChildren(expression.Sub)
	case regexpsyntax.OpAlternate:
		total, ok := walkChildren(expression.Sub)
		if !ok {
			return 0, false
		}
		return add(total, max(0, len(expression.Sub)-1))
	case regexpsyntax.OpStar, regexpsyntax.OpPlus, regexpsyntax.OpQuest:
		childWork, ok := regexpProgramWork(expression.Sub[0], limit)
		if !ok {
			return 0, false
		}
		return add(childWork, 1)
	case regexpsyntax.OpRepeat:
		childWork, ok := regexpProgramWork(expression.Sub[0], limit)
		if !ok {
			return 0, false
		}
		copies := expression.Max
		if copies < 0 {
			copies = expression.Min + 1
		}
		if copies > limit || childWork+1 > limit/max(1, copies) {
			return 0, false
		}
		return add(copies*(childWork+1), 1)
	default:
		return 1, true
	}
}

var cardinalitySchemaKeywords = map[string]bool{
	"maxContains":   true,
	"maxItems":      true,
	"maxLength":     true,
	"maxProperties": true,
	"minContains":   true,
	"minItems":      true,
	"minLength":     true,
	"minProperties": true,
}

type artifactSchemaComplexity struct {
	subschemas       int
	validationWeight int
	resources        map[string]any
	resourceDrafts   map[string]int
	resourceURLBytes int
}

func chargeArtifactSchemaReferenceURL(complexity *artifactSchemaComplexity, baseURL, keyword, raw string) error {
	if len(raw) > maxSchemaResourceURLBytes {
		return fmt.Errorf("schema %s URL exceeds the %d-byte limit", keyword, maxSchemaResourceURLBytes)
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil
	}
	reference, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	resolved := base.ResolveReference(reference).String()
	if len(resolved) > maxSchemaResourceURLBytes {
		return fmt.Errorf("resolved schema %s URL exceeds the %d-byte limit", keyword, maxSchemaResourceURLBytes)
	}
	if len(resolved) > maxSchemaResourceURLTotal-complexity.resourceURLBytes {
		return fmt.Errorf("schema resource and reference URLs exceed the %d-byte aggregate limit", maxSchemaResourceURLTotal)
	}
	complexity.resourceURLBytes += len(resolved)
	return nil
}

func validateSchemaImplementationBounds(root any) (artifactSchemaComplexity, error) {
	complexity := artifactSchemaComplexity{
		resources:        map[string]any{artifactSchemaResourceURL: root},
		resourceDrafts:   map[string]int{artifactSchemaResourceURL: 2020},
		resourceURLBytes: len(artifactSchemaResourceURL),
	}
	visitedObjects := make(map[uintptr]bool)
	addValidationWeight := func(delta int) error {
		if delta > maxSchemaValidationWork-complexity.validationWeight {
			return fmt.Errorf("schema exceeds the %d-unit validation safety budget", maxSchemaValidationWork)
		}
		complexity.validationWeight += delta
		return nil
	}
	var walk func(any, int, bool, string) error
	walk = func(value any, draft int, isRoot bool, baseURL string) error {
		schema, ok := value.(map[string]any)
		if !ok {
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("schema subschema must be an object or boolean")
			}
			complexity.subschemas++
			if complexity.subschemas > maxSchemaSubschemas {
				return fmt.Errorf("schema exceeds the %d-subschema limit", maxSchemaSubschemas)
			}
			return addValidationWeight(1)
		}
		identity := reflect.ValueOf(schema).Pointer()
		if visitedObjects[identity] {
			return nil
		}
		visitedObjects[identity] = true
		draft = artifactSchemaObjectDraft(schema, draft, isRoot)
		resourceURL, isResource, err := artifactSchemaResourceBase(schema, draft, baseURL)
		if err != nil {
			return err
		}
		if isRoot {
			complexity.resourceDrafts[baseURL] = draft
		}
		if isResource {
			baseURL = resourceURL
			if _, exists := complexity.resources[resourceURL]; !exists {
				if len(resourceURL) > maxSchemaResourceURLBytes {
					return fmt.Errorf("schema resource URL exceeds the %d-byte limit", maxSchemaResourceURLBytes)
				}
				if len(resourceURL) > maxSchemaResourceURLTotal-complexity.resourceURLBytes {
					return fmt.Errorf("schema resource URLs exceed the %d-byte aggregate limit", maxSchemaResourceURLTotal)
				}
				complexity.resourceURLBytes += len(resourceURL)
				complexity.resources[resourceURL] = schema
				complexity.resourceDrafts[resourceURL] = draft
			}
		}
		referenceKeywords := []string{"$ref"}
		if isRoot || isResource {
			referenceKeywords = append(referenceKeywords, "$schema")
		}
		if draft == 2019 {
			referenceKeywords = append(referenceKeywords, "$recursiveRef")
		}
		if draft >= 2020 {
			referenceKeywords = append(referenceKeywords, "$dynamicRef")
		}
		for _, keyword := range referenceKeywords {
			if raw, ok := schema[keyword].(string); ok {
				if err := chargeArtifactSchemaReferenceURL(&complexity, baseURL, keyword, raw); err != nil {
					return err
				}
			}
		}

		complexity.subschemas++
		if complexity.subschemas > maxSchemaSubschemas {
			return fmt.Errorf("schema exceeds the %d-subschema limit", maxSchemaSubschemas)
		}
		if err := addValidationWeight(1); err != nil {
			return err
		}
		rawReference, hasReference := schema["$ref"].(string)
		if draft < 2019 && hasReference {
			if target, targetBase, targetDraft, ok := artifactSchemaReferenceTarget(
				complexity.resources, complexity.resourceDrafts, baseURL, rawReference,
			); ok {
				if err := walk(target, targetDraft, false, targetBase); err != nil {
					return err
				}
			}
			return nil
		}
		if draft < 2019 && schema["format"] == "regex" {
			return fmt.Errorf("schema format %q is not supported by bounded artifact validation", "regex")
		}
		for keyword := range cardinalitySchemaKeywords {
			if (keyword == "minContains" || keyword == "maxContains") && draft < 2019 {
				continue
			}
			if err := validateSchemaCardinality(keyword, schema[keyword]); err != nil {
				return err
			}
		}
		for _, keyword := range []string{"required", "type"} {
			if values, ok := schema[keyword].([]any); ok {
				if err := addValidationWeight(len(values)); err != nil {
					return err
				}
			}
		}
		for _, keyword := range []string{"enum"} {
			if value, ok := schema[keyword]; ok {
				if err := addValidationWeight(countJSONValues(value)); err != nil {
					return err
				}
			}
		}
		if draft >= 6 {
			if value, ok := schema["const"]; ok {
				if err := addValidationWeight(countJSONValues(value)); err != nil {
					return err
				}
			}
		}
		dependencyKeywords := []string{}
		if draft >= 2019 {
			dependencyKeywords = append(dependencyKeywords, "dependentRequired")
		} else {
			dependencyKeywords = append(dependencyKeywords, "dependencies")
		}
		for _, keyword := range dependencyKeywords {
			if dependencies, ok := schema[keyword].(map[string]any); ok {
				for _, dependency := range dependencies {
					if err := addValidationWeight(1); err != nil {
						return err
					}
					if names, ok := dependency.([]any); ok {
						if err := addValidationWeight(len(names)); err != nil {
							return err
						}
					}
				}
			}
		}
		for keyword, child := range schema {
			switch keyword {
			case "additionalProperties", "not":
				if err := walk(child, draft, false, baseURL); err != nil {
					return err
				}
			case "additionalItems":
				if draft < 2020 {
					if err := walk(child, draft, false, baseURL); err != nil {
						return err
					}
				}
			case "contains", "propertyNames":
				if draft >= 6 {
					if err := walk(child, draft, false, baseURL); err != nil {
						return err
					}
				}
			case "else", "if", "then":
				if draft >= 7 {
					if err := walk(child, draft, false, baseURL); err != nil {
						return err
					}
				}
			case "contentSchema", "unevaluatedItems", "unevaluatedProperties":
				if draft >= 2019 {
					if err := walk(child, draft, false, baseURL); err != nil {
						return err
					}
				}
			case "items":
				if children, ok := child.([]any); ok && draft < 2020 {
					for _, item := range children {
						if err := walk(item, draft, false, baseURL); err != nil {
							return err
						}
					}
				} else if err := walk(child, draft, false, baseURL); err != nil {
					return err
				}
			case "allOf", "anyOf", "oneOf":
				children, ok := child.([]any)
				if !ok {
					return fmt.Errorf("schema keyword %q must contain an array of schemas", keyword)
				}
				for _, item := range children {
					if err := walk(item, draft, false, baseURL); err != nil {
						return err
					}
				}
			case "prefixItems":
				if draft >= 2020 {
					children, ok := child.([]any)
					if !ok {
						return fmt.Errorf("schema keyword %q must contain an array of schemas", keyword)
					}
					for _, item := range children {
						if err := walk(item, draft, false, baseURL); err != nil {
							return err
						}
					}
				}
			case "patternProperties", "properties":
				children, ok := child.(map[string]any)
				if !ok {
					return fmt.Errorf("schema keyword %q must contain an object", keyword)
				}
				for _, item := range children {
					if err := walk(item, draft, false, baseURL); err != nil {
						return err
					}
				}
			case "definitions":
				if draft < 2019 {
					if err := walkSchemaMapKeyword(keyword, child, draft, baseURL, walk); err != nil {
						return err
					}
				}
			case "$defs", "dependentSchemas":
				if draft >= 2019 {
					if err := walkSchemaMapKeyword(keyword, child, draft, baseURL, walk); err != nil {
						return err
					}
				}
			case "dependencies":
				if draft < 2019 {
					children, ok := child.(map[string]any)
					if !ok {
						return fmt.Errorf("schema keyword %q must contain an object", keyword)
					}
					for _, item := range children {
						if _, ok := item.([]any); ok {
							continue
						}
						if err := walk(item, draft, false, baseURL); err != nil {
							return err
						}
					}
				}
			}
		}
		if hasReference {
			if target, targetBase, targetDraft, ok := artifactSchemaReferenceTarget(
				complexity.resources, complexity.resourceDrafts, baseURL, rawReference,
			); ok {
				if err := walk(target, targetDraft, false, targetBase); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, 2020, true, artifactSchemaResourceURL); err != nil {
		return artifactSchemaComplexity{}, err
	}
	return complexity, nil
}

func artifactSchemaReferenceTarget(
	resources map[string]any,
	resourceDrafts map[string]int,
	baseURL, rawReference string,
) (target any, targetBase string, targetDraft int, ok bool) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, "", 0, false
	}
	reference, err := url.Parse(rawReference)
	if err != nil {
		return nil, "", 0, false
	}
	resolved := base.ResolveReference(reference)
	pointer := resolved.Fragment
	resolved.Fragment = ""
	resolved.RawFragment = ""
	root, ok := resources[resolved.String()]
	if !ok || (pointer != "" && !strings.HasPrefix(pointer, "/")) {
		return nil, "", 0, false
	}
	target, err = artifactSchemaAtJSONPointer(root, pointer)
	if err != nil {
		return nil, "", 0, false
	}
	return target, resolved.String(), resourceDrafts[resolved.String()], true
}

func walkSchemaMapKeyword(keyword string, value any, draft int, baseURL string, walk func(any, int, bool, string) error) error {
	children, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("schema keyword %q must contain an object", keyword)
	}
	for _, child := range children {
		if err := walk(child, draft, false, baseURL); err != nil {
			return err
		}
	}
	return nil
}

func artifactSchemaObjectDraft(schema map[string]any, inherited int, isRoot bool) int {
	rawSchema, ok := schema["$schema"].(string)
	if !ok {
		return inherited
	}
	declared, ok := artifactSchemaDraft(rawSchema)
	if !ok {
		return inherited
	}
	if isRoot {
		return declared
	}
	idKeyword := "$id"
	if declared == 4 {
		idKeyword = "id"
	}
	id, _ := schema[idKeyword].(string)
	if id == "" {
		return inherited
	}
	return declared
}

func artifactSchemaResourceBase(schema map[string]any, draft int, current string) (string, bool, error) {
	keyword := "$id"
	if draft == 4 {
		keyword = "id"
	}
	raw, ok := schema[keyword].(string)
	if !ok || raw == "" {
		return "", false, nil
	}
	if len(raw) > maxSchemaResourceURLBytes {
		return "", false, fmt.Errorf("schema resource ID exceeds the %d-byte limit", maxSchemaResourceURLBytes)
	}
	base, err := url.Parse(current)
	if err != nil {
		return "", false, nil
	}
	reference, err := url.Parse(raw)
	if err != nil {
		return "", false, nil
	}
	resolved := base.ResolveReference(reference)
	resolved.Fragment = ""
	resolved.RawFragment = ""
	base.Fragment = ""
	base.RawFragment = ""
	if resolved.String() == base.String() {
		return "", false, nil
	}
	if len(resolved.String()) > maxSchemaResourceURLBytes {
		return "", false, fmt.Errorf("resolved schema resource URL exceeds the %d-byte limit", maxSchemaResourceURLBytes)
	}
	return resolved.String(), true, nil
}

func artifactSchemaDraft(raw string) (int, bool) {
	normalized := strings.TrimSuffix(raw, "#")
	normalized = strings.TrimPrefix(normalized, "http://")
	normalized = strings.TrimPrefix(normalized, "https://")
	switch normalized {
	case "json-schema.org/schema", "json-schema.org/draft/2020-12/schema":
		return 2020, true
	case "json-schema.org/draft/2019-09/schema":
		return 2019, true
	case "json-schema.org/draft-07/schema":
		return 7, true
	case "json-schema.org/draft-06/schema":
		return 6, true
	case "json-schema.org/draft-04/schema":
		return 4, true
	default:
		return 0, false
	}
}

func countJSONValues(value any) int {
	count := 1
	switch value := value.(type) {
	case []any:
		for _, child := range value {
			count += countJSONValues(child)
			if count > maxSchemaValidationWork {
				return maxSchemaValidationWork + 1
			}
		}
	case map[string]any:
		for _, child := range value {
			count += countJSONValues(child)
			if count > maxSchemaValidationWork {
				return maxSchemaValidationWork + 1
			}
		}
	}
	return count
}

func countJSONNumericWork(value any) int {
	work := 0
	switch value := value.(type) {
	case json.Number:
		work, _ = boundedJSONNumberWork(value)
	case []any:
		for _, child := range value {
			work += countJSONNumericWork(child)
			if work > maxSchemaValidationWork {
				return maxSchemaValidationWork + 1
			}
		}
	case map[string]any:
		for _, child := range value {
			work += countJSONNumericWork(child)
			if work > maxSchemaValidationWork {
				return maxSchemaValidationWork + 1
			}
		}
	}
	return work
}

func countJSONStringBytes(value any) int {
	bytes := 0
	switch value := value.(type) {
	case string:
		bytes = len(value)
	case []any:
		for _, child := range value {
			bytes += countJSONStringBytes(child)
			if bytes > maxSchemaValidationWork {
				return maxSchemaValidationWork + 1
			}
		}
	case map[string]any:
		for key, child := range value {
			bytes += len(key) + countJSONStringBytes(child)
			if bytes > maxSchemaValidationWork {
				return maxSchemaValidationWork + 1
			}
		}
	}
	return bytes
}

func schemaStringBytes(values []string) int {
	bytes := 0
	for _, value := range values {
		bytes += len(value)
		if bytes > maxSchemaValidationWork {
			return maxSchemaValidationWork + 1
		}
	}
	return bytes
}

func validateSchemaCardinality(keyword string, value any) error {
	number, ok := value.(json.Number)
	if !ok {
		return nil
	}
	cardinality, ok := new(big.Rat).SetString(number.String())
	if !ok || !cardinality.IsInt() || cardinality.Sign() < 0 {
		return nil
	}
	if cardinality.Num().Cmp(big.NewInt(maxSchemaCardinality)) > 0 {
		return fmt.Errorf("schema keyword %q exceeds the supported cardinality limit of %d", keyword, maxSchemaCardinality)
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

func validateJSONAgainstSchema(doc []byte, schema *artifactSchema) []schemaViolation {
	stats, err := validateArtifactJSONShape(doc)
	if err != nil {
		message := "artifact is not valid unambiguous JSON"
		if errors.Is(err, errJSONValueLimit) {
			message = fmt.Sprintf("artifact JSON exceeds the %d-value validation limit", maxSchemaArtifactValues)
		} else if errors.Is(err, errJSONNumericLimit) {
			message = "artifact JSON exceeds the numeric complexity limit"
		} else if errors.Is(err, errJSONDepthLimit) {
			message = fmt.Sprintf("artifact JSON exceeds the %d-level nesting-depth limit", maxSchemaJSONDepth)
		}
		return []schemaViolation{{Keyword: "json", Message: message}}
	}
	if stats.values > 0 && schema.validationWeight > maxSchemaValidationWork/stats.values {
		return []schemaViolation{{
			Keyword: "complexity",
			Message: fmt.Sprintf("artifact and schema exceed the %d-unit validation safety budget", maxSchemaValidationWork),
		}}
	}
	if stats.stringBytes > 0 && schema.validationWeight > maxSchemaValidationBytes/stats.stringBytes {
		return []schemaViolation{{
			Keyword: "complexity",
			Message: fmt.Sprintf("artifact strings and schema exceed the %d-byte validation safety budget", maxSchemaValidationBytes),
		}}
	}
	if stats.numberWork > 0 && schema.numericValidationWeight > maxSchemaNumericWork/stats.numberWork {
		return []schemaViolation{{
			Keyword: "complexity",
			Message: fmt.Sprintf("artifact numbers and schema exceed the %d-unit numeric validation safety budget", maxSchemaNumericWork),
		}}
	}
	if schema.uniqueItemsWeight > 0 && stats.maxArrayLen > 1 {
		if exceedsBoundedProduct(maxSchemaValidationWork, schema.uniqueItemsWeight, stats.maxArrayLen, stats.values) {
			return []schemaViolation{{
				Keyword: "complexity",
				Message: "artifact and schema exceed the uniqueItems structural comparison safety budget",
			}}
		}
		if exceedsBoundedProduct(maxSchemaValidationBytes, schema.uniqueItemsWeight, stats.maxArrayLen, stats.stringBytes) {
			return []schemaViolation{{
				Keyword: "complexity",
				Message: "artifact strings and schema exceed the uniqueItems byte comparison safety budget",
			}}
		}
		if exceedsBoundedProduct(maxSchemaNumericWork, schema.uniqueItemsWeight, stats.maxArrayLen, stats.numberWork) {
			return []schemaViolation{{
				Keyword: "complexity",
				Message: "artifact numbers and schema exceed the uniqueItems numeric comparison safety budget",
			}}
		}
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		return []schemaViolation{{Keyword: "json", Message: "artifact is not valid JSON"}}
	}
	validationErrValue := schema.compiled.Validate(value)
	if validationErrValue != nil {
		var validationErr *jsonschema.ValidationError
		if !errors.As(validationErrValue, &validationErr) {
			return []schemaViolation{{Message: "artifact did not satisfy its JSON Schema"}}
		}
		return boundedSchemaViolations(validationErr)
	}
	return nil
}

func exceedsBoundedProduct(limit int, factors ...int) bool {
	remaining := limit
	for _, factor := range factors {
		if factor <= 0 {
			return false
		}
		if factor > remaining {
			return true
		}
		remaining /= factor
	}
	return false
}

func boundedSchemaViolations(validationErr *jsonschema.ValidationError) []schemaViolation {
	violations := make([]schemaViolation, 0, maxSchemaViolations)
	seen := make(map[string]bool)
	stack := []*jsonschema.ValidationError{validationErr}
	totalBytes := 0
	truncation := schemaViolation{
		Keyword: "truncated",
		Message: "additional violations omitted after the bounded diagnostic budget",
	}
	truncationBytes := len(truncation.String())
	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		if len(current.Causes) > 0 {
			for i := len(current.Causes) - 1; i >= 0; i-- {
				stack = append(stack, current.Causes[i])
			}
			continue
		}
		path := boundedJSONPointer(current.InstanceLocation)
		keyword := boundedSchemaKeywordLocation(current)
		key := path + "\x00" + keyword
		if seen[key] {
			continue
		}
		seen[key] = true
		violation := schemaViolation{Path: path, Keyword: keyword}
		renderedBytes := len(violation.String())
		if len(violations) == maxSchemaViolations || renderedBytes > maxSchemaDiagnosticBytes-totalBytes-truncationBytes {
			return append(violations, truncation)
		}
		totalBytes += renderedBytes
		violations = append(violations, violation)
	}
	if len(violations) == 0 {
		violations = append(violations, schemaViolation{Message: "artifact did not satisfy its JSON Schema"})
	}
	return violations
}

func boundedJSONPointer(tokens []string) string {
	return boundedSchemaLocation("", tokens)
}

func boundedSchemaKeywordLocation(validationErr *jsonschema.ValidationError) string {
	fragment := ""
	if hash := strings.IndexByte(validationErr.SchemaURL, '#'); hash >= 0 {
		fragment = validationErr.SchemaURL[hash+1:]
	}
	return boundedSchemaLocation(fragment, validationErr.ErrorKind.KeywordPath())
}

func boundedSchemaLocation(prefix string, tokens []string) string {
	const marker = "..."
	contentLimit := maxSchemaLocationBytes - len(marker)
	var result strings.Builder
	result.Grow(min(maxSchemaLocationBytes, len(prefix)+32))
	truncated := false
	if len(prefix) > contentLimit {
		result.WriteString(prefix[:contentLimit])
		truncated = true
	} else {
		result.WriteString(prefix)
	}
	for _, token := range tokens {
		if truncated {
			break
		}
		if result.Len()+1 > contentLimit {
			truncated = true
			break
		}
		result.WriteByte('/')
		for _, character := range token {
			encoded := safeSchemaLocationRune(character)
			switch character {
			case '~':
				encoded = "~0"
			case '/':
				encoded = "~1"
			}
			if result.Len()+len(encoded) > contentLimit {
				truncated = true
				break
			}
			result.WriteString(encoded)
		}
	}
	if truncated {
		result.WriteString(marker)
	}
	return result.String()
}

func safeSchemaLocationRune(character rune) string {
	if unicode.IsControl(character) || character == '\u2028' || character == '\u2029' || isBidirectionalControl(character) {
		if character <= 0xffff {
			return fmt.Sprintf("\\u%04X", character)
		}
		return fmt.Sprintf("\\U%08X", character)
	}
	return string(character)
}

func isBidirectionalControl(character rune) bool {
	return character == '\u061c' || character == '\u200e' || character == '\u200f' ||
		(character >= '\u202a' && character <= '\u202e') ||
		(character >= '\u2066' && character <= '\u2069')
}

type loadedArtifactSchema struct {
	remote     string
	schemaPath string
	schema     *artifactSchema
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
		data, err := readBoundedSchemaDefinition(schemaPath)
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

func readBoundedSchemaDefinition(schemaPath string) ([]byte, error) {
	file, err := os.Open(schemaPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxSchemaDefinitionBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSchemaDefinitionBytes {
		return nil, fmt.Errorf("schema exceeds the %d-byte limit", maxSchemaDefinitionBytes)
	}
	return data, nil
}

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
