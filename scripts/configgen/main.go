// Command configgen derives mechanical config wiring from a concrete Go struct.
// It deliberately supports only repo-safe fields; security policy stays in code.
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
	name, kind, key, env, flag, help, defaultExpr string
	nonnegative                                   bool
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
		// This is an explicit source grant, not a default. Do not extend it to
		// credentials or destinations without a separate provenance-aware design.
		if tags.Get("sources") != "user,repo,env,flag" {
			return s, fmt.Errorf("%s requires explicit repo-safe sources user,repo,env,flag", f.name)
		}
		for _, binding := range []struct{ label, value string }{{"config", f.key}, {"env", f.env}, {"flag", f.flag}} {
			if binding.value == "" || strings.ContainsAny(binding.value, " \t\n,\"`") {
				return s, fmt.Errorf("%s: invalid %s binding", f.name, binding.label)
			}
			key := binding.label + ":" + binding.value
			if seen[key] {
				return s, fmt.Errorf("duplicate %s binding %s", binding.label, binding.value)
			}
			seen[key] = true
		}
		if f.help == "" {
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
	for _, f := range s.fields {
		if f.kind == "[]string" {
			p("; \"os\"; \"strings\"")
			break
		}
	}
	p(")\n\n")
	p("type file%s struct {\n", s.name)
	for _, f := range s.fields {
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
	p("func (cfg *%s) applyFile(file *file%s) error {\nif file == nil { return nil }\n", s.name, s.name)
	for _, f := range s.fields {
		p("if file.%s != nil {\n", f.name)
		if f.nonnegative {
			p("if *file.%s < 0 { return exit(2, %q) }\n", f.name, s.provider+" "+f.key+" must be non-negative")
		}
		value := "*file." + f.name
		if f.kind == "[]string" {
			value = "normalizeList(" + value + ")"
		}
		p("cfg.%s = %s\n}\n", f.name, value)
	}
	p("return nil\n}\n\n")
	p("func (cfg *%s) applyEnv() error {\n", s.name)
	for _, f := range s.fields {
		switch f.kind {
		case "string":
			p("cfg.%s = getenv(%q, cfg.%s)\n", f.name, f.env, f.name)
		case "float64":
			p("cfg.%s = getenvFloat(%q, cfg.%s)\n", f.name, f.env, f.name)
		case "int":
			p("{ var err error; cfg.%s, err = getenvNonNegativeInt(%q, cfg.%s); if err != nil { return err } }\n", f.name, f.env, f.name)
		case "bool":
			p("if value, ok := getenvBool(%q); ok { cfg.%s = value }\n", f.env, f.name)
		case "[]string":
			p("if value := os.Getenv(%q); value != \"\" { cfg.%s = splitCommaList(value) }\n", f.env, f.name)
		}
	}
	p("return nil\n}\n\n")
	p("// %sFlagValues holds parsed values; only visited flags are applied.\ntype %sFlagValues struct {\n", s.name, s.name)
	for _, f := range s.fields {
		kind := f.kind
		if kind == "[]string" {
			kind = "string"
		}
		p("%s *%s\n", f.name, kind)
	}
	p("}\n\n")
	p("// Register%sFlags registers mechanical bindings without selecting a provider.\nfunc Register%sFlags(fs *flag.FlagSet, defaults %s) %sFlagValues {\nreturn %sFlagValues{\n", s.name, s.name, s.name, s.name, s.name)
	for _, f := range s.fields {
		method := map[string]string{"string": "String", "int": "Int", "float64": "Float64", "bool": "Bool", "[]string": "String"}[f.kind]
		value := "defaults." + f.name
		if f.kind == "[]string" {
			value = "strings.Join(" + value + ", \",\")"
		}
		p("%s: fs.%s(%q, %s, %q),\n", f.name, method, f.flag, value, f.help)
	}
	p("}\n}\n\n")
	p("// Apply copies explicit flag values. Provider validation must run afterward.\nfunc (values %sFlagValues) Apply(cfg *%s, fs *flag.FlagSet) {\n", s.name, s.name)
	for _, f := range s.fields {
		value := "*values." + f.name
		if f.kind == "[]string" {
			value = "splitCommaList(" + value + ")"
		}
		p("if flagWasSet(fs, %q) { cfg.%s = %s }\n", f.flag, f.name, value)
	}
	p("}\n")
	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated config: %w", err)
	}
	return formatted, nil
}
