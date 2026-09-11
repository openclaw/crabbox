// Command configgen derives mechanical config wiring from a concrete Go struct.
// It emits explicit source bindings; source trust is supplied by the loader.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
)

type field struct {
	envSplitBefore                                                                                                                                                               bool
	flagDurationRawPositive                                                                                                                                                      bool
	fileListNonemptyNormalized, envListTrimmedNonempty, flagListCSV                                                                                                              bool
	flagDurationRawZeroReset, flagListAppendTrimmed                                                                                                                              bool
	flagDurationError                                                                                                                                                            string
	envListCSV, flagListScalarEmptyNil                                                                                                                                           bool
	fileStorageValue                                                                                                                                                             bool
	fileListNonemptyRaw, flagListEmptyScalar                                                                                                                                     bool
	fileIntNonzero                                                                                                                                                               bool
	fileListRaw, envListPresence, flagListReplaceAppend                                                                                                                          bool
	name, kind, key, configAlias, env, envAlias, envAlias2, flag, help, defaultExpr, flagFallbackExpr                                                                            string
	nonnegative, trustedFileOnly, noFile, noEnv, noFlag, fileIgnoreEmpty, reportApplied, envIntFallback, fileIntPositive, fileIntPresent, fileFloatPositive, envAliasAfterConfig bool
}

func (f field) stringDurationFlag() bool {
	return f.flagDurationError != "" || f.flagDurationRawZeroReset || f.flagDurationRawPositive
}

type fileBinding struct {
	member, key string
}

func (f field) fileBindings() []fileBinding {
	if f.noFile {
		return nil
	}
	bindings := []fileBinding{{f.name, f.key}}
	if f.configAlias != "" {
		bindings = append(bindings, fileBinding{f.name + "ConfigAlias", f.configAlias})
	}
	return bindings
}

type schema struct {
	pkg, name, provider string
	fields              []field
}

func main() {
	source := flag.String("source", "", "Go source containing the config struct")
	output := flag.String("output", "", "generated Go output")
	name := flag.String("type", "", "config type name")
	provider := flag.String("provider", "", "provider name for diagnostics")
	check := flag.Bool("check", false, "fail if output is missing or stale; do not write")
	flag.Parse()
	if err := run(*source, *output, *name, *provider, *check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(source, output, name, provider string, check bool) error {
	if source == "" || output == "" || name == "" || provider == "" {
		return fmt.Errorf("configgen requires -source, -output, -type, and -provider")
	}
	input, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	s, err := parseSchema(input, name, provider)
	if err != nil {
		return err
	}
	generated, err := generate(s, filepath.Base(source))
	if err != nil {
		return err
	}
	if check {
		current, err := os.ReadFile(output)
		if err != nil || !bytes.Equal(current, generated) {
			return fmt.Errorf("%s is stale; run go generate ./internal/cli", output)
		}
		return nil
	}
	return os.WriteFile(output, generated, 0644)
}

func parseSchema(source []byte, name, provider string) (schema, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "config.go", source, 0)
	if err != nil {
		return schema{}, err
	}
	s := schema{pkg: file.Name.Name, name: name, provider: provider}
	var fields *ast.FieldList
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			typ, ok := spec.(*ast.TypeSpec)
			if !ok || typ.Name.Name != name {
				continue
			}
			st, ok := typ.Type.(*ast.StructType)
			if !ok {
				return s, fmt.Errorf("%s must be a struct", name)
			}
			fields = st.Fields
		}
	}
	if fields == nil {
		return s, fmt.Errorf("config type %s not found", name)
	}
	seen := map[string]bool{}
	seenFileMembers := map[string]bool{}
	for _, node := range fields.List {
		if len(node.Names) != 1 || !node.Names[0].IsExported() || node.Tag == nil {
			return s, fmt.Errorf("each config field must be singly named, exported, and tagged")
		}
		raw, err := strconv.Unquote(node.Tag.Value)
		if err != nil {
			return s, err
		}
		tags := reflect.StructTag(raw)
		if tags.Get("sources") == "runtime" {
			if raw != `sources:"runtime"` {
				return s, fmt.Errorf("%s: runtime fields require only sources:\"runtime\"", node.Names[0].Name)
			}
			continue
		}
		f := field{name: node.Names[0].Name, key: tags.Get("config"), env: tags.Get("env"), flag: tags.Get("flag"), help: tags.Get("help")}
		// These exact grants preserve existing loader policy; they are not a
		// general source-policy language or a default permission.
		switch tags.Get("sources") {
		case "user,repo,env,flag":
		case "user,env,flag":
			f.trustedFileOnly = true
		case "env,flag":
			f.noFile = true
			if _, ok := tags.Lookup("config"); ok {
				return s, fmt.Errorf("%s: env,flag sources require an absent config tag", f.name)
			}
		case "user,repo,env", "user,env":
			f.noFlag = true
			f.trustedFileOnly = tags.Get("sources") == "user,env"
			for _, tag := range []string{"flag", "help", "default"} {
				if _, ok := tags.Lookup(tag); ok {
					return s, fmt.Errorf("%s: %s sources require an absent %s tag", f.name, tags.Get("sources"), tag)
				}
			}
		case "env":
			f.noFile, f.noFlag = true, true
			for _, tag := range []string{"config", "flag", "help", "default"} {
				if _, ok := tags.Lookup(tag); ok {
					return s, fmt.Errorf("%s: env sources require an absent %s tag", f.name, tag)
				}
			}
		case "flag":
			f.noFile, f.noEnv = true, true
			for _, tag := range []string{"config", "env", "envAlias", "envAlias2"} {
				if _, ok := tags.Lookup(tag); ok {
					return s, fmt.Errorf("%s: flag sources require an absent %s tag", f.name, tag)
				}
			}
		default:
			return s, fmt.Errorf("%s requires explicit sources user,repo,env,flag, user,env,flag, env,flag, flag, env, user,repo,env, or user,env", f.name)
		}
		var bindings []struct{ label, value string }
		if !f.noFlag {
			bindings = append(bindings, struct{ label, value string }{"flag", f.flag})
		}
		if !f.noEnv {
			bindings = append([]struct{ label, value string }{{"env", f.env}}, bindings...)
		}
		if !f.noFile {
			bindings = append([]struct{ label, value string }{{"config", f.key}}, bindings...)
		}
		alias, hasAlias := tags.Lookup("envAlias")
		if hasAlias {
			f.envAlias = alias
			bindings = append(bindings, struct{ label, value string }{"env", alias})
		}
		alias2, hasAlias2 := tags.Lookup("envAlias2")
		if hasAlias2 {
			if !hasAlias {
				return s, fmt.Errorf("%s: envAlias2 requires envAlias", f.name)
			}
			f.envAlias2 = alias2
			bindings = append(bindings, struct{ label, value string }{"env", alias2})
		}
		configAlias, hasConfigAlias := tags.Lookup("configAlias")
		if hasConfigAlias {
			f.configAlias = configAlias
			bindings = append(bindings, struct{ label, value string }{"config", configAlias})
		}
		for _, binding := range bindings {
			if binding.value == "" || strings.ContainsAny(binding.value, " \t\n,\"`") {
				return s, fmt.Errorf("%s: invalid %s binding", f.name, binding.label)
			}
			key := binding.label + ":" + binding.value
			if seen[key] {
				return s, fmt.Errorf("duplicate %s binding %s", binding.label, binding.value)
			}
			seen[key] = true
		}
		if !f.noFlag && f.help == "" {
			return s, fmt.Errorf("%s needs flag help", f.name)
		}
		var typeText bytes.Buffer
		if err := format.Node(&typeText, token.NewFileSet(), node.Type); err != nil {
			return s, err
		}
		f.kind = typeText.String()
		switch f.kind {
		case "string", "int", "int64", "float64", "bool", "[]string":
		case "time.Duration":
			standardTime := false
			for _, spec := range file.Imports {
				path, err := strconv.Unquote(spec.Path.Value)
				if err == nil && path == "time" && (spec.Name == nil || spec.Name.Name == "time") {
					standardTime = true
				}
			}
			if !standardTime {
				return s, fmt.Errorf("%s: time.Duration requires the standard time import named time", f.name)
			}
		default:
			if _, ok := tags.Lookup("fileStorage"); ok {
				return s, fmt.Errorf("%s: fileStorage does not support config type %s", f.name, f.kind)
			}
			return s, fmt.Errorf("%s: unsupported config type %s", f.name, f.kind)
		}
		if mode, present := tags.Lookup("duration"); f.kind == "time.Duration" {
			if !present || mode != "positive-overlay" {
				return s, fmt.Errorf("%s: time.Duration requires duration positive-overlay", f.name)
			}
			if !f.noFile && tags.Get("fileStorage") != "value" {
				return s, fmt.Errorf("%s: duration file input requires fileStorage value", f.name)
			}
		} else if present {
			return s, fmt.Errorf("%s: duration is supported only for time.Duration", f.name)
		}
		flagDuration, hasFlagDuration := tags.Lookup("flagDuration")
		flagDurationError, hasFlagDurationError := tags.Lookup("flagDurationError")
		if hasFlagDuration {
			if f.kind != "time.Duration" || f.noFlag || (flagDuration != "trim-positive" && flagDuration != "raw-zero-reset" && flagDuration != "raw-positive") {
				return s, fmt.Errorf("%s: flagDuration requires trim-positive, raw-zero-reset, or raw-positive on a flag-admitted time.Duration", f.name)
			}
			if flagDuration == "raw-zero-reset" || flagDuration == "raw-positive" {
				if hasFlagDurationError {
					return s, fmt.Errorf("%s: %s forbids flagDurationError", f.name, flagDuration)
				}
				f.flagDurationRawZeroReset = flagDuration == "raw-zero-reset"
				f.flagDurationRawPositive = flagDuration == "raw-positive"
			} else if !hasFlagDurationError || strings.TrimSpace(flagDurationError) == "" {
				return s, fmt.Errorf("%s: flagDuration requires a nonempty flagDurationError", f.name)
			} else {
				f.flagDurationError = flagDurationError
			}
		} else if hasFlagDurationError {
			return s, fmt.Errorf("%s: flagDurationError requires flagDuration", f.name)
		}
		if hasConfigAlias && (f.kind != "string" || f.noFile) {
			return s, fmt.Errorf("%s: configAlias requires a string field with a file source", f.name)
		}
		for _, binding := range f.fileBindings() {
			if seenFileMembers[binding.member] {
				return s, fmt.Errorf("duplicate generated file member %s", binding.member)
			}
			seenFileMembers[binding.member] = true
		}
		if f.noFlag && f.kind != "string" && !(tags.Get("sources") == "user,repo,env" && (f.kind == "[]string" || f.kind == "bool" || f.kind == "time.Duration")) {
			allowed := "string fields"
			if tags.Get("sources") == "user,repo,env" {
				allowed += ", []string, bool, or time.Duration fields"
			}
			return s, fmt.Errorf("%s: %s sources support only %s", f.name, tags.Get("sources"), allowed)
		}
		for _, mode := range []struct {
			tag      string
			admitted bool
			values   map[string]*bool
		}{
			{"fileList", !f.noFile, map[string]*bool{"raw": &f.fileListRaw, "nonempty-raw": &f.fileListNonemptyRaw, "nonempty-normalized": &f.fileListNonemptyNormalized}},
			{"envList", !f.noEnv, map[string]*bool{"presence": &f.envListPresence, "csv": &f.envListCSV, "trimmed-nonempty": &f.envListTrimmedNonempty}},
			{"flagList", !f.noFlag, map[string]*bool{"replace-append": &f.flagListReplaceAppend, "append-trimmed": &f.flagListAppendTrimmed, "empty-scalar": &f.flagListEmptyScalar, "scalar-empty-nil": &f.flagListScalarEmptyNil, "csv": &f.flagListCSV}},
		} {
			if value, ok := tags.Lookup(mode.tag); ok {
				enabled := mode.values[value]
				if f.kind != "[]string" || !mode.admitted || enabled == nil {
					return s, fmt.Errorf("%s: %s requires a supported mode on a []string field with that source", f.name, mode.tag)
				}
				*enabled = true
			}
		}
		if value, ok := tags.Lookup("reportApplied"); ok {
			if value != "true" || (f.kind != "string" && f.kind != "bool") {
				return s, fmt.Errorf("%s: reportApplied is supported only as true for string or bool fields", f.name)
			}
			f.reportApplied = true
			if f.name == "InputAccepted" {
				return s, fmt.Errorf("InputAccepted is reserved for the generated acceptance report")
			}
		}
		if value, ok := tags.Lookup("fileIgnoreEmpty"); ok {
			if value != "true" || f.kind != "string" || f.noFile {
				return s, fmt.Errorf("%s: fileIgnoreEmpty is supported only as true for string fields with a file source", f.name)
			}
			f.fileIgnoreEmpty = true
		}
		if hasAlias2 && (f.kind != "string" || f.noEnv) {
			return s, fmt.Errorf("%s: envAlias2 requires an environment-admitted string field", f.name)
		}
		if value, ok := tags.Lookup("envAliasAfterConfig"); ok {
			if value != "true" || f.kind != "string" || f.noEnv || !hasAlias || hasAlias2 {
				return s, fmt.Errorf("%s: envAliasAfterConfig requires true on an environment-admitted string with exactly one envAlias", f.name)
			}
			f.envAliasAfterConfig = true
		}
		if value, ok := tags.Lookup("nonnegative"); ok {
			if (f.kind != "int" && f.kind != "int64") || value != "true" {
				return s, fmt.Errorf("%s: nonnegative is supported only as true for int fields or int64 fields", f.name)
			}
			f.nonnegative = true
		}
		if (f.kind == "int" || f.kind == "int64") && !f.nonnegative {
			return s, fmt.Errorf("%s: pilot int fields require nonnegative policy", f.name)
		}
		if value, ok := tags.Lookup("fileInt"); ok {
			if (value != "positive" && value != "present" && value != "nonzero") || (f.kind != "int" && f.kind != "int64") || f.noFile || !f.nonnegative {
				return s, fmt.Errorf("%s: fileInt is supported only as positive, present, or nonzero for file-admitted nonnegative int fields", f.name)
			}
			f.fileIntPositive = value == "positive"
			f.fileIntPresent = value == "present"
			f.fileIntNonzero = value == "nonzero"
		}
		if value, ok := tags.Lookup("fileFloat"); ok {
			if value != "positive" || f.kind != "float64" || f.noFile {
				return s, fmt.Errorf("%s: fileFloat is supported only as positive for file-admitted float64 fields", f.name)
			}
			f.fileFloatPositive = true
		}
		if value, ok := tags.Lookup("envInt"); ok {
			if value != "fallback" || (f.kind != "int" && f.kind != "int64") || f.noEnv || !f.nonnegative {
				return s, fmt.Errorf("%s: envInt is supported only as fallback for environment-admitted nonnegative int fields", f.name)
			}
			f.envIntFallback = true
		}
		if f.kind == "int64" && !f.noEnv && !f.envIntFallback {
			return s, fmt.Errorf("%s: int64 environment fields require envInt fallback", f.name)
		}
		if hasAlias && f.kind != "string" && !(f.kind == "int" && !f.noEnv && f.envIntFallback) {
			return s, fmt.Errorf("%s: envAlias is supported only for string fields or environment-admitted int fields with envInt fallback", f.name)
		}
		if value, ok := tags.Lookup("flagFallback"); ok {
			if value == "" || f.kind != "string" || f.noFlag {
				return s, fmt.Errorf("%s: flagFallback requires a nonempty value on a flag-admitted string field", f.name)
			}
			if _, hasDefault := tags.Lookup("default"); hasDefault {
				return s, fmt.Errorf("%s: flagFallback and default are mutually exclusive", f.name)
			}
			f.flagFallbackExpr = strconv.Quote(value)
		}
		if value, ok := tags.Lookup("default"); ok {
			f.defaultExpr, err = defaultExpression(f.kind, value)
			if err != nil {
				return s, fmt.Errorf("%s default: %w", f.name, err)
			}
			if f.nonnegative && strings.HasPrefix(f.defaultExpr, "-") {
				return s, fmt.Errorf("%s default must be non-negative", f.name)
			}
		}
		if value, ok := tags.Lookup("fileStorage"); ok {
			eligible := false
			switch f.kind {
			case "string":
				eligible = f.fileIgnoreEmpty
			case "[]string":
				eligible = f.fileListNonemptyRaw || f.fileListRaw || f.fileListNonemptyNormalized
			case "int", "int64":
				eligible = f.fileIntPositive || f.fileIntNonzero
			case "float64":
				eligible = f.fileFloatPositive
			case "time.Duration":
				eligible = true
			}
			if value != "value" || f.noFile || !eligible {
				return s, fmt.Errorf("%s: fileStorage requires value on a file-admitted field with a zero-ignoring file rule", f.name)
			}
			f.fileStorageValue = true
		}
		if value, ok := tags.Lookup("envSplitBefore"); ok {
			if value != "true" || f.noEnv {
				return s, fmt.Errorf("%s: envSplitBefore requires true on an environment-admitted field", f.name)
			}
			f.envSplitBefore = true
		}
		s.fields = append(s.fields, f)
	}
	if len(s.fields) == 0 {
		return s, fmt.Errorf("%s has no fields", name)
	}
	seenSplit, prefixEnv := false, false
	for _, f := range s.fields {
		if f.envSplitBefore {
			if seenSplit || !prefixEnv {
				return s, fmt.Errorf("%s: envSplitBefore requires one split with a nonempty environment prefix and suffix", f.name)
			}
			seenSplit = true
		}
		prefixEnv = prefixEnv || !f.noEnv
	}
	return s, nil
}

func defaultExpression(kind, value string) (string, error) {
	switch kind {
	case "string":
		return strconv.Quote(value), nil
	case "int":
		v, err := strconv.ParseInt(value, 10, 32)
		return strconv.FormatInt(v, 10), err
	case "int64":
		v, err := strconv.ParseInt(value, 10, 64)
		return strconv.FormatInt(v, 10), err
	case "float64":
		v, err := strconv.ParseFloat(value, 64)
		if err == nil && (math.IsNaN(v) || math.IsInf(v, 0)) {
			err = fmt.Errorf("default must be finite")
		}
		return strconv.FormatFloat(v, 'g', -1, 64), err
	case "bool":
		v, err := strconv.ParseBool(value)
		return strconv.FormatBool(v), err
	case "time.Duration":
		v, err := time.ParseDuration(value)
		if err != nil {
			return "", err
		}
		if v <= 0 {
			return "", fmt.Errorf("duration default must be positive")
		}
		for _, unit := range []struct {
			value time.Duration
			name  string
		}{{time.Second, "Second"}, {time.Millisecond, "Millisecond"}, {time.Microsecond, "Microsecond"}} {
			if v%unit.value == 0 {
				return fmt.Sprintf("%d * time.%s", v/unit.value, unit.name), nil
			}
		}
		return fmt.Sprintf("%d * time.Nanosecond", v), nil
	default:
		return "", fmt.Errorf("defaults for %s are not supported", kind)
	}
}

func generate(s schema, source string) ([]byte, error) {
	var out bytes.Buffer
	p := func(pattern string, args ...any) { fmt.Fprintf(&out, pattern, args...) }
	p("// Code generated by scripts/configgen from %s; DO NOT EDIT.\n\npackage %s\n\n", source, s.pkg)
	hasFlags, needsOS, needsStrings, needsTime, needsStrconv := false, false, false, false, false
	for _, f := range s.fields {
		hasFlags = hasFlags || !f.noFlag
		needsStrconv = needsStrconv || (f.kind == "int" && !f.noEnv && f.envIntFallback)
		needsStrings = needsStrings || f.flagDurationError != "" || f.flagDurationRawZeroReset
		if f.kind == "time.Duration" {
			needsTime = needsTime || (!f.noFlag && !f.stringDurationFlag()) || f.flagDurationError != "" || f.defaultExpr != ""
			needsOS = needsOS || !f.noEnv
		}
		if f.kind == "[]string" {
			needsStrings = needsStrings || f.envListTrimmedNonempty
			needsStrings = needsStrings || (!f.noFlag && !f.flagListReplaceAppend && !f.flagListAppendTrimmed && !f.flagListEmptyScalar)
			needsOS = needsOS || (!f.noEnv && !f.envListPresence)
		}
	}
	if hasFlags || needsOS || needsStrings || needsTime || needsStrconv {
		p("import (")
		if hasFlags {
			p("\"flag\";")
		}
		if needsOS {
			p("\"os\";")
		}
		if needsStrings {
			p("\"strings\";")
		}
		if needsTime {
			p("\"time\";")
		}
		if needsStrconv {
			p("\"strconv\";")
		}
		p(")\n\n")
	}
	p("type file%s struct {\n", s.name)
	for _, f := range s.fields {
		for _, binding := range f.fileBindings() {
			kind := "*" + f.kind
			if f.fileStorageValue {
				kind = f.kind
			}
			if f.kind == "time.Duration" {
				kind = "string"
			}
			p("%s %s `yaml:%q`\n", binding.member, kind, binding.key+",omitempty")
		}
	}
	p("}\n\n")
	for _, f := range s.fields {
		if f.defaultExpr != "" {
			p("const %sDefault%s %s = %s\n", s.name, f.name, f.kind, f.defaultExpr)
		}
		if f.flagFallbackExpr != "" {
			p("const %sFlagFallback%s string = %s\n", s.name, f.name, f.flagFallbackExpr)
		}
	}
	p("\nfunc default%s() %s { return %s{\n", s.name, s.name, s.name)
	for _, f := range s.fields {
		if f.defaultExpr != "" {
			p("%s: %sDefault%s,\n", f.name, s.name, f.name)
		}
	}
	p("} }\n\n")
	trackedFlags := false
	for _, f := range s.fields {
		trackedFlags = trackedFlags || (f.reportApplied && !f.noFlag)
	}
	p("// %sApplied records accepted assignments during one application.\ntype %sApplied struct {\nInputAccepted bool\n", s.name, s.name)
	for _, f := range s.fields {
		if f.reportApplied {
			p("%s bool\n", f.name)
		}
	}
	p("}\n\n")
	resultType := "(" + s.name + "Applied, error)"
	resultPrefix := "applied, "
	reportInit := "var applied " + s.name + "Applied\n"
	trustedParameter := ""
	for _, f := range s.fields {
		if f.trustedFileOnly {
			trustedParameter = ", trusted bool"
			break
		}
	}
	p("func (cfg *%s) applyFile(file *file%s%s) %s {\n%sif file == nil { return %snil }\n", s.name, s.name, trustedParameter, resultType, reportInit, resultPrefix)
	for _, f := range s.fields {
		for _, binding := range f.fileBindings() {
			member := "file." + binding.member
			value := member
			var conditions []string
			if f.trustedFileOnly {
				conditions = append(conditions, "trusted")
			}
			if !f.fileStorageValue {
				conditions = append(conditions, member+" != nil")
				value = "*" + member
			}
			if f.fileIgnoreEmpty || f.kind == "time.Duration" {
				conditions = append(conditions, value+" != \"\"")
			}
			if f.fileIntPositive || f.fileFloatPositive {
				conditions = append(conditions, value+" > 0")
			}
			if f.fileIntNonzero {
				conditions = append(conditions, value+" != 0")
			}
			if f.fileListNonemptyRaw || f.fileListNonemptyNormalized {
				conditions = append(conditions, "len("+value+") > 0")
			}
			if f.fileListRaw && f.fileStorageValue {
				conditions = append(conditions, value+" != nil")
			}
			p("if %s {\n", strings.Join(conditions, " && "))
			if f.nonnegative && !f.fileIntPositive && !f.fileIntPresent && !f.fileIntNonzero {
				p("if %s < 0 { return %sexit(2, %q) }\n", value, resultPrefix, s.provider+" "+f.key+" must be non-negative")
			}
			if f.kind == "[]string" {
				if f.fileListRaw {
					value = "append([]string(nil), (" + value + ")...)"
				} else if !f.fileListNonemptyRaw {
					value = "normalizeList(" + value + ")"
				}
			}
			if f.kind == "time.Duration" {
				p("if applyLeaseDuration(&cfg.%s, %s) { applied.InputAccepted = true }\n", f.name, value)
			} else {
				p("cfg.%s = %s\napplied.InputAccepted = true\n", f.name, value)
			}
			if f.reportApplied {
				p("applied.%s = true\n", f.name)
			}
			p("}\n")
		}
	}
	p("return %snil\n}\n\n", resultPrefix)
	emitEnv := func(name string, fields []field) {
		p("func (cfg *%s) %s() %s {\n%s", s.name, name, resultType, reportInit)
		for _, f := range fields {
			if f.noEnv {
				continue
			}
			switch f.kind {
			case "time.Duration":
				p("if value := os.Getenv(%q); value != \"\" { if applyLeaseDuration(&cfg.%s, value) { applied.InputAccepted = true } }\n", f.env, f.name)
			case "string":
				if f.envAliasAfterConfig {
					p("if value, ok := firstNonEmptyEnv(%q); ok {\ncfg.%s = value\napplied.InputAccepted = true\n", f.env, f.name)
					if f.reportApplied {
						p("applied.%s = true\n", f.name)
					}
					p("} else if cfg.%s == \"\" {\nif value, ok := firstNonEmptyEnv(%q); ok {\ncfg.%s = value\napplied.InputAccepted = true\n", f.name, f.envAlias, f.name)
					if f.reportApplied {
						p("applied.%s = true\n", f.name)
					}
					p("}\n}\n")
					continue
				}
				names := strconv.Quote(f.env)
				if f.envAlias != "" {
					names += ", " + strconv.Quote(f.envAlias)
				}
				if f.envAlias2 != "" {
					names += ", " + strconv.Quote(f.envAlias2)
				}
				p("if value, ok := firstNonEmptyEnv(%s); ok { cfg.%s = value; applied.InputAccepted = true\n", names, f.name)
				if f.reportApplied {
					p("applied.%s = true\n", f.name)
				}
				p("}\n")
			case "float64":
				p("if value, ok := lookupEnvFloat(%q); ok { cfg.%s = value; applied.InputAccepted = true }\n", f.env, f.name)
			case "int":
				if f.envIntFallback {
					if f.envAlias != "" {
						p("{ value, accepted := lookupEnvInteger(%q, strconv.IntSize); if primary, ok := lookupEnvInteger(%q, strconv.IntSize); ok { value, accepted = primary, true }; if accepted { cfg.%s = int(value); applied.InputAccepted = true } }\n", f.envAlias, f.env, f.name)
					} else {
						p("if value, ok := lookupEnvInteger(%q, strconv.IntSize); ok { cfg.%s = int(value); applied.InputAccepted = true }\n", f.env, f.name)
					}
					continue
				}
				p("{ var accepted bool; var err error; cfg.%s, accepted, err = getenvNonNegativeIntAccepted(%q, cfg.%s); if err != nil { return %serr }; if accepted { applied.InputAccepted = true } }\n", f.name, f.env, f.name, resultPrefix)
			case "int64":
				p("if value, ok := lookupEnvInteger(%q, 64); ok { cfg.%s = value; applied.InputAccepted = true }\n", f.env, f.name)
			case "bool":
				if f.reportApplied {
					p("if value, ok := getenvBool(%q); ok { cfg.%s = value; applied.InputAccepted = true; applied.%s = true }\n", f.env, f.name, f.name)
				} else {
					p("if value, ok := getenvBool(%q); ok { cfg.%s = value; applied.InputAccepted = true }\n", f.env, f.name)
				}
			case "[]string":
				if f.envListTrimmedNonempty {
					p("if value := os.Getenv(%q); strings.TrimSpace(value) != \"\" { cfg.%s = parseEnvListValue(value); applied.InputAccepted = true }\n", f.env, f.name)
					continue
				}
				if f.envListPresence {
					p("if value, ok := getenvList(%q); ok { cfg.%s = value; applied.InputAccepted = true }\n", f.env, f.name)
					continue
				}
				parser := "splitCommaList"
				if f.envListCSV {
					parser = "splitCSV"
				}
				p("if value := os.Getenv(%q); value != \"\" { cfg.%s = %s(value); applied.InputAccepted = true }\n", f.env, f.name, parser)
			}
		}
		p("return %snil\n}\n\n", resultPrefix)
	}
	split := -1
	for i, f := range s.fields {
		if f.envSplitBefore {
			split = i
		}
	}
	if split < 0 {
		emitEnv("applyEnv", s.fields)
	} else {
		emitEnv("applyEnvPrefix", s.fields[:split])
		emitEnv("applyEnvSuffix", s.fields[split:])
	}
	if hasFlags {
		p("// %sFlagValues holds parsed values; only visited flags are applied.\ntype %sFlagValues struct {\n", s.name, s.name)
		for _, f := range s.fields {
			if f.noFlag {
				continue
			}
			kind := f.kind
			if f.stringDurationFlag() {
				kind = "string"
			}
			if kind == "[]string" {
				kind = "string"
				if f.flagListReplaceAppend {
					kind = "replaceAppendListFlag"
				}
				if f.flagListAppendTrimmed {
					kind = "appendTrimmedListFlag"
				}
			}
			p("%s *%s\n", f.name, kind)
		}
		p("}\n\n")
		p("// Register%sFlags registers mechanical bindings without selecting a provider.\nfunc Register%sFlags(fs *flag.FlagSet, defaults %s) %sFlagValues {\n", s.name, s.name, s.name, s.name)
		hasAppendTrimmed := false
		for _, f := range s.fields {
			if f.flagListReplaceAppend {
				p("list%s := newReplaceAppendListFlag(defaults.%s)\nfs.Var(list%s, %q, %q)\n", f.name, f.name, f.name, f.flag, f.help)
			}
			if f.flagListAppendTrimmed {
				hasAppendTrimmed = true
				p("list%s := newAppendTrimmedListFlag(defaults.%s)\n", f.name, f.name)
			}
		}
		if hasAppendTrimmed {
			p("values := %sFlagValues{\n", s.name)
		} else {
			p("return %sFlagValues{\n", s.name)
		}
		for _, f := range s.fields {
			if f.noFlag {
				continue
			}
			if f.flagListReplaceAppend || f.flagListAppendTrimmed {
				p("%s: list%s,\n", f.name, f.name)
				continue
			}
			method := map[string]string{"string": "String", "int": "Int", "int64": "Int64", "float64": "Float64", "bool": "Bool", "[]string": "String", "time.Duration": "Duration"}[f.kind]
			value := "defaults." + f.name
			if f.stringDurationFlag() {
				method = "String"
				value += ".String()"
			}
			if f.flagFallbackExpr != "" {
				value = fmt.Sprintf("blank(%s, %sFlagFallback%s)", value, s.name, f.name)
			}
			if f.kind == "[]string" {
				if f.flagListEmptyScalar {
					value = "\"\""
				} else {
					value = "strings.Join(" + value + ", \",\")"
				}
			}
			p("%s: fs.%s(%q, %s, %q),\n", f.name, method, f.flag, value, f.help)
		}
		p("}\n")
		if hasAppendTrimmed {
			for _, f := range s.fields {
				if f.flagListAppendTrimmed {
					p("fs.Var(list%s, %q, %q)\n", f.name, f.flag, f.help)
				}
			}
			p("return values\n")
		}
		p("}\n\n")
		if trackedFlags {
			p("// %sVisitedFlags records raw flag visits, independently of application.\ntype %sVisitedFlags struct {\n", s.name, s.name)
			for _, f := range s.fields {
				if f.reportApplied && !f.noFlag {
					p("%s bool\n", f.name)
				}
			}
			p("}\n\n")
			p("// %sFlagPresence reports visits for tracked flag bindings.\nfunc %sFlagPresence(fs *flag.FlagSet) %sVisitedFlags {\nreturn %sVisitedFlags{\n", s.name, s.name, s.name, s.name)
			for _, f := range s.fields {
				if f.reportApplied && !f.noFlag {
					p("%s: flagWasSet(fs, %q),\n", f.name, f.flag)
				}
			}
			p("}\n}\n\n")
		}
		p("// Apply copies explicit flag values. Provider validation must run afterward.\nfunc (values %sFlagValues) Apply(cfg *%s, fs *flag.FlagSet) %s {\n%s", s.name, s.name, resultType, reportInit)
		if trackedFlags {
			p("visited := %sFlagPresence(fs)\n", s.name)
		}
		for _, f := range s.fields {
			if f.noFlag {
				continue
			}
			if f.flagDurationRawZeroReset {
				p("if flagWasSet(fs, %q) { if strings.TrimSpace(*values.%s) == \"0s\" { cfg.%s = 0; applied.InputAccepted = true } else if err := ApplyLeaseDuration(&cfg.%s, *values.%s); err != nil { return %serr } else if *values.%s != \"\" { applied.InputAccepted = true } }\n", f.flag, f.name, f.name, f.name, f.name, resultPrefix, f.name)
				continue
			}
			if f.flagDurationRawPositive {
				p("if flagWasSet(fs, %q) { if err := ApplyLeaseDuration(&cfg.%s, *values.%s); err != nil { return %serr } else if *values.%s != \"\" { applied.InputAccepted = true } }\n", f.flag, f.name, f.name, resultPrefix, f.name)
				continue
			}
			if f.flagDurationError != "" {
				p("if flagWasSet(fs, %q) { parsed, err := time.ParseDuration(strings.TrimSpace(*values.%s)); if err != nil || parsed <= 0 { return %sexit(2, \"%%s\", %q) }; cfg.%s = parsed; applied.InputAccepted = true }\n", f.flag, f.name, resultPrefix, f.flagDurationError, f.name)
				continue
			}
			if f.flagListEmptyScalar || f.flagListScalarEmptyNil {
				p("if flagWasSet(fs, %q) { cfg.%s = splitCommaList(*values.%s); if len(cfg.%s) == 0 { cfg.%s = nil }; applied.InputAccepted = true }\n", f.flag, f.name, f.name, f.name, f.name)
				continue
			}
			if f.flagListCSV {
				p("if flagWasSet(fs, %q) { cfg.%s = splitCSV(*values.%s); applied.InputAccepted = true }\n", f.flag, f.name, f.name)
				continue
			}
			value := "*values." + f.name
			if f.kind == "[]string" {
				if f.flagListReplaceAppend {
					value = "append([]string(nil), values." + f.name + ".values...)"
				} else if f.flagListAppendTrimmed {
					value = "append([]string(nil), values." + f.name + ".stringListFlag...)"
				} else {
					value = "splitCommaList(" + value + ")"
				}
			}
			if f.reportApplied {
				p("if visited.%s { cfg.%s = %s; applied.InputAccepted = true; applied.%s = true }\n", f.name, f.name, value, f.name)
			} else {
				p("if flagWasSet(fs, %q) { cfg.%s = %s; applied.InputAccepted = true }\n", f.flag, f.name, value)
			}
		}
		p("return %snil\n", resultPrefix)
		p("}\n")
	}
	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated config: %w", err)
	}
	return formatted, nil
}
