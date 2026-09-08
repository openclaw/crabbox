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
)

type field struct {
	name, kind, key, env, envAlias, flag, help, defaultExpr                             string
	nonnegative, trustedFileOnly, noFile, noEnv, noFlag, fileIgnoreEmpty, reportApplied bool
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
	for _, node := range fields.List {
		if len(node.Names) != 1 || !node.Names[0].IsExported() || node.Tag == nil {
			return s, fmt.Errorf("each config field must be singly named, exported, and tagged")
		}
		raw, err := strconv.Unquote(node.Tag.Value)
		if err != nil {
			return s, err
		}
		tags := reflect.StructTag(raw)
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
		case "user,repo,env":
			f.noFlag = true
			for _, tag := range []string{"flag", "help", "default"} {
				if _, ok := tags.Lookup(tag); ok {
					return s, fmt.Errorf("%s: user,repo,env sources require an absent %s tag", f.name, tag)
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
			for _, tag := range []string{"config", "env", "envAlias"} {
				if _, ok := tags.Lookup(tag); ok {
					return s, fmt.Errorf("%s: flag sources require an absent %s tag", f.name, tag)
				}
			}
		default:
			return s, fmt.Errorf("%s requires explicit sources user,repo,env,flag, user,env,flag, env,flag, flag, env, or user,repo,env", f.name)
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
		case "string", "int", "float64", "bool", "[]string":
		default:
			return s, fmt.Errorf("%s: unsupported config type %s", f.name, f.kind)
		}
		if f.noFlag && f.kind != "string" {
			return s, fmt.Errorf("%s: %s sources support only string fields", f.name, tags.Get("sources"))
		}
		if value, ok := tags.Lookup("reportApplied"); ok {
			if value != "true" || (f.kind != "string" && f.kind != "bool") {
				return s, fmt.Errorf("%s: reportApplied is supported only as true for string or bool fields", f.name)
			}
			f.reportApplied = true
		}
		if value, ok := tags.Lookup("fileIgnoreEmpty"); ok {
			if value != "true" || f.kind != "string" || f.noFile {
				return s, fmt.Errorf("%s: fileIgnoreEmpty is supported only as true for string fields with a file source", f.name)
			}
			f.fileIgnoreEmpty = true
		}
		if hasAlias && f.kind != "string" {
			return s, fmt.Errorf("%s: envAlias is supported only for string fields", f.name)
		}
		if value, ok := tags.Lookup("nonnegative"); ok {
			if f.kind != "int" || value != "true" {
				return s, fmt.Errorf("%s: nonnegative is supported only as true for int fields", f.name)
			}
			f.nonnegative = true
		}
		if f.kind == "int" && !f.nonnegative {
			return s, fmt.Errorf("%s: pilot int fields require nonnegative policy", f.name)
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
		s.fields = append(s.fields, f)
	}
	if len(s.fields) == 0 {
		return s, fmt.Errorf("%s has no fields", name)
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
	case "float64":
		v, err := strconv.ParseFloat(value, 64)
		if err == nil && (math.IsNaN(v) || math.IsInf(v, 0)) {
			err = fmt.Errorf("default must be finite")
		}
		return strconv.FormatFloat(v, 'g', -1, 64), err
	case "bool":
		v, err := strconv.ParseBool(value)
		return strconv.FormatBool(v), err
	default:
		return "", fmt.Errorf("defaults for %s are not supported", kind)
	}
}

func generate(s schema, source string) ([]byte, error) {
	var out bytes.Buffer
	p := func(pattern string, args ...any) { fmt.Fprintf(&out, pattern, args...) }
	p("// Code generated by scripts/configgen from %s; DO NOT EDIT.\n\npackage %s\n\nimport (\"flag\"", source, s.pkg)
	needsOS, needsStrings := false, false
	for _, f := range s.fields {
		if f.kind == "[]string" {
			needsStrings = true
			needsOS = needsOS || !f.noEnv
		}
	}
	if needsOS {
		p("; \"os\"")
	}
	if needsStrings {
		p("; \"strings\"")
	}
	p(")\n\n")
	p("type file%s struct {\n", s.name)
	for _, f := range s.fields {
		if f.noFile {
			continue
		}
		p("%s *%s `yaml:%q`\n", f.name, f.kind, f.key+",omitempty")
	}
	p("}\n\n")
	for _, f := range s.fields {
		if f.defaultExpr != "" {
			p("const %sDefault%s %s = %s\n", s.name, f.name, f.kind, f.defaultExpr)
		}
	}
	p("\nfunc default%s() %s { return %s{\n", s.name, s.name, s.name)
	for _, f := range s.fields {
		if f.defaultExpr != "" {
			p("%s: %sDefault%s,\n", f.name, s.name, f.name)
		}
	}
	p("} }\n\n")
	reportsApplied, trackedFlags := false, false
	for _, f := range s.fields {
		reportsApplied = reportsApplied || f.reportApplied
		trackedFlags = trackedFlags || (f.reportApplied && !f.noFlag)
	}
	resultType, resultPrefix, reportInit := "error", "", ""
	if reportsApplied {
		p("// %sApplied records accepted assignments during one application.\ntype %sApplied struct {\n", s.name, s.name)
		for _, f := range s.fields {
			if f.reportApplied {
				p("%s bool\n", f.name)
			}
		}
		p("}\n\n")
		resultType = "(" + s.name + "Applied, error)"
		resultPrefix = "applied, "
		reportInit = "var applied " + s.name + "Applied\n"
	}
	trustedParameter := ""
	for _, f := range s.fields {
		if f.trustedFileOnly {
			trustedParameter = ", trusted bool"
			break
		}
	}
	p("func (cfg *%s) applyFile(file *file%s%s) %s {\n%sif file == nil { return %snil }\n", s.name, s.name, trustedParameter, resultType, reportInit, resultPrefix)
	for _, f := range s.fields {
		if f.noFile {
			continue
		}
		condition := ""
		if f.trustedFileOnly {
			condition = "trusted && "
		}
		fileCondition := fmt.Sprintf("%sfile.%s != nil", condition, f.name)
		if f.fileIgnoreEmpty {
			fileCondition += fmt.Sprintf(" && *file.%s != \"\"", f.name)
		}
		p("if %s {\n", fileCondition)
		if f.nonnegative {
			p("if *file.%s < 0 { return %sexit(2, %q) }\n", f.name, resultPrefix, s.provider+" "+f.key+" must be non-negative")
		}
		value := "*file." + f.name
		if f.kind == "[]string" {
			value = "normalizeList(" + value + ")"
		}
		p("cfg.%s = %s\n", f.name, value)
		if f.reportApplied {
			p("applied.%s = true\n", f.name)
		}
		p("}\n")
	}
	p("return %snil\n}\n\n", resultPrefix)
	p("func (cfg *%s) applyEnv() %s {\n%s", s.name, resultType, reportInit)
	for _, f := range s.fields {
		if f.noEnv {
			continue
		}
		switch f.kind {
		case "string":
			if f.reportApplied {
				names := strconv.Quote(f.env)
				if f.envAlias != "" {
					names += ", " + strconv.Quote(f.envAlias)
				}
				p("if value, ok := firstNonEmptyEnv(%s); ok { cfg.%s = value; applied.%s = true }\n", names, f.name, f.name)
				continue
			}
			fallback := "cfg." + f.name
			if f.envAlias != "" {
				fallback = fmt.Sprintf("getenv(%q, %s)", f.envAlias, fallback)
			}
			p("cfg.%s = getenv(%q, %s)\n", f.name, f.env, fallback)
		case "float64":
			p("cfg.%s = getenvFloat(%q, cfg.%s)\n", f.name, f.env, f.name)
		case "int":
			p("{ var err error; cfg.%s, err = getenvNonNegativeInt(%q, cfg.%s); if err != nil { return %serr } }\n", f.name, f.env, f.name, resultPrefix)
		case "bool":
			if f.reportApplied {
				p("if value, ok := getenvBool(%q); ok { cfg.%s = value; applied.%s = true }\n", f.env, f.name, f.name)
			} else {
				p("if value, ok := getenvBool(%q); ok { cfg.%s = value }\n", f.env, f.name)
			}
		case "[]string":
			p("if value := os.Getenv(%q); value != \"\" { cfg.%s = splitCommaList(value) }\n", f.env, f.name)
		}
	}
	p("return %snil\n}\n\n", resultPrefix)
	p("// %sFlagValues holds parsed values; only visited flags are applied.\ntype %sFlagValues struct {\n", s.name, s.name)
	for _, f := range s.fields {
		if f.noFlag {
			continue
		}
		kind := f.kind
		if kind == "[]string" {
			kind = "string"
		}
		p("%s *%s\n", f.name, kind)
	}
	p("}\n\n")
	p("// Register%sFlags registers mechanical bindings without selecting a provider.\nfunc Register%sFlags(fs *flag.FlagSet, defaults %s) %sFlagValues {\nreturn %sFlagValues{\n", s.name, s.name, s.name, s.name, s.name)
	for _, f := range s.fields {
		if f.noFlag {
			continue
		}
		method := map[string]string{"string": "String", "int": "Int", "float64": "Float64", "bool": "Bool", "[]string": "String"}[f.kind]
		value := "defaults." + f.name
		if f.kind == "[]string" {
			value = "strings.Join(" + value + ", \",\")"
		}
		p("%s: fs.%s(%q, %s, %q),\n", f.name, method, f.flag, value, f.help)
	}
	p("}\n}\n\n")
	applyResult := ""
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
	if reportsApplied {
		applyResult = " " + s.name + "Applied"
	}
	p("// Apply copies explicit flag values. Provider validation must run afterward.\nfunc (values %sFlagValues) Apply(cfg *%s, fs *flag.FlagSet)%s {\n%s", s.name, s.name, applyResult, reportInit)
	if trackedFlags {
		p("visited := %sFlagPresence(fs)\n", s.name)
	}
	for _, f := range s.fields {
		if f.noFlag {
			continue
		}
		value := "*values." + f.name
		if f.kind == "[]string" {
			value = "splitCommaList(" + value + ")"
		}
		if f.reportApplied {
			p("if visited.%s { cfg.%s = %s; applied.%s = true }\n", f.name, f.name, value, f.name)
		} else {
			p("if flagWasSet(fs, %q) { cfg.%s = %s }\n", f.flag, f.name, value)
		}
	}
	if reportsApplied {
		p("return applied\n")
	}
	p("}\n")
	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated config: %w", err)
	}
	return formatted, nil
}
