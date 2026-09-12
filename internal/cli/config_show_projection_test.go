package cli

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestProviderConfigShowLegacyLabelInventoryMatchesSource(t *testing.T) {
	actual := map[string]bool{}
	for _, source := range []struct{ file, function string }{
		{"config_cmd.go", "writeConfigShowText"},
		{"config_provider_status.go", "writeProviderConfigStatus"},
	} {
		parsed, err := parser.ParseFile(token.NewFileSet(), source.file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var body *ast.BlockStmt
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == source.function {
				body = fn.Body
			}
		}
		if body == nil {
			t.Fatalf("missing formatter %s", source.function)
		}
		ast.Inspect(body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if !ok || pkg.Name != "fmt" || (selector.Sel.Name != "Fprintf" && selector.Sel.Name != "Fprintln") {
				return true
			}
			if len(call.Args) < 2 {
				t.Fatalf("incomplete fmt call in %s", source.function)
			}
			literal, ok := call.Args[1].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Fatalf("nonliteral legacy format in %s needs an explicit inventory rule", source.function)
			}
			format, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			end := strings.IndexAny(format, " =\t\r\n")
			if end < 0 {
				end = len(format)
			}
			label := format[:end]
			if label == "" || strings.Contains(label, "%") {
				t.Fatalf("nonstatic legacy label %q in %s", label, source.function)
			}
			actual[label] = true
			return true
		})
	}
	reserved := map[string]bool{}
	for _, label := range strings.Fields(legacyConfigShowTextLabels) {
		reserved[label] = true
	}
	if !reflect.DeepEqual(actual, reserved) {
		t.Fatalf("legacy label inventory drift: source=%v reserved=%v", actual, reserved)
	}
}

// Nil embedded capabilities catch any unintended provider operation by collection.
type configShowProjectionTestProvider struct {
	Provider
	name    string
	section ProviderConfigShowSection
}

func (p configShowProjectionTestProvider) Name() string { return p.name }
func (p configShowProjectionTestProvider) ConfigShowSection(Config) ProviderConfigShowSection {
	return p.section
}

func projectionTestSection(name string) ProviderConfigShowSection {
	return ProviderConfigShowSection{JSONKey: name, TextLabel: name, Providers: []string{name}, Fields: []ProviderConfigShowField{
		{JSONName: "zero", JSONValue: 0, TextName: "zero", TextValue: "0"},
		{JSONName: "nil", JSONValue: nil},
		{TextName: "state", TextValue: "unchecked"},
		{JSONName: "false", JSONValue: false, TextName: "false", TextValue: "false"},
	}}
}

func TestProviderConfigShowProjectionOrderedAndPassive(t *testing.T) {
	a, b := projectionTestSection("alpha_projection"), projectionTestSection("beta_projection")
	providers := []Provider{configShowProjectionTestProvider{name: b.Providers[0], section: b}, configShowProjectionTestProvider{name: a.Providers[0], section: a}}
	sections, err := collectProviderConfigShowSectionsFrom(Config{Provider: "unselected"}, providers)
	if err != nil {
		t.Fatal(err)
	}
	var text bytes.Buffer
	if err := writeProviderConfigShowSections(&text, sections); err != nil {
		t.Fatal(err)
	}
	want := "alpha_projection zero=0 state=unchecked false=false\nbeta_projection zero=0 state=unchecked false=false\n"
	if text.String() != want {
		t.Fatalf("text=%q", text.String())
	}
	view := map[string]any{"legacy": "unchanged"}
	if err := addProviderConfigShowSections(view, sections); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(view["alpha_projection"], map[string]any{"zero": 0, "nil": nil, "false": false}) || view["legacy"] != "unchanged" {
		t.Fatalf("view=%#v", view)
	}
	if !reflect.DeepEqual(providers[0].(configShowProjectionTestProvider).section, b) {
		t.Fatal("collection mutated provider data")
	}
}

func TestProviderConfigShowProjectionRejectsInvalidMetadata(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ProviderConfigShowSection)
	}{
		{"empty key", func(s *ProviderConfigShowSection) { s.JSONKey = "" }},
		{"empty label", func(s *ProviderConfigShowSection) { s.TextLabel = "" }},
		{"legacy label", func(s *ProviderConfigShowSection) { s.TextLabel = "phala" }},
		{"generic label", func(s *ProviderConfigShowSection) { s.TextLabel = "provider" }},
		{"missing coverage", func(s *ProviderConfigShowSection) { s.Providers = nil }},
		{"unknown identity", func(s *ProviderConfigShowSection) { s.Providers = []string{"unknown"} }},
		{"duplicate identity", func(s *ProviderConfigShowSection) { s.Providers = append(s.Providers, s.Providers[0]) }},
		{"missing fields", func(s *ProviderConfigShowSection) { s.Fields = nil }},
		{"unnamed field", func(s *ProviderConfigShowSection) { s.Fields = append(s.Fields, ProviderConfigShowField{}) }},
		{"duplicate json", func(s *ProviderConfigShowSection) {
			s.Fields = append(s.Fields, ProviderConfigShowField{JSONName: "zero"})
		}},
		{"duplicate text", func(s *ProviderConfigShowSection) {
			s.Fields = append(s.Fields, ProviderConfigShowField{TextName: "zero"})
		}},
		{"bad text name", func(s *ProviderConfigShowSection) { s.Fields[0].TextName = "two fields" }},
		{"missing text format", func(s *ProviderConfigShowSection) { s.Fields = []ProviderConfigShowField{{JSONName: "one"}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := projectionTestSection("test_projection")
			tc.change(&s)
			if _, err := collectProviderConfigShowSectionsFrom(Config{}, []Provider{configShowProjectionTestProvider{name: "test_projection", section: s}}); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
	for _, duplicate := range []string{"key", "label", "coverage", "owner"} {
		a, b := projectionTestSection("a_projection"), projectionTestSection("b_projection")
		switch duplicate {
		case "key":
			b.JSONKey = a.JSONKey
		case "label":
			b.TextLabel = a.TextLabel
		case "coverage":
			b.Providers = append(b.Providers, a.Providers...)
		case "owner":
			a.Providers = b.Providers
		}
		if _, err := collectProviderConfigShowSectionsFrom(Config{}, []Provider{configShowProjectionTestProvider{name: "a_projection", section: a}, configShowProjectionTestProvider{name: "b_projection", section: b}}); err == nil {
			t.Fatalf("duplicate %s accepted", duplicate)
		}
	}
}

func TestProviderConfigShowProjectionJSONCollisionIsAtomic(t *testing.T) {
	a, b := projectionTestSection("a_projection"), projectionTestSection("b_projection")
	for _, sections := range [][]ProviderConfigShowSection{{a, b}, {a, a}} {
		view := map[string]any{"b_projection": "legacy"}
		if err := addProviderConfigShowSections(view, sections); err == nil {
			t.Fatal("collision accepted")
		}
		if !reflect.DeepEqual(view, map[string]any{"b_projection": "legacy"}) {
			t.Fatal("collision partially mutated output")
		}
	}
}

type configShowFailureWriter struct {
	err   error
	short bool
}

func (w configShowFailureWriter) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	return 0, w.err
}

func TestConfigShowLegacySlotPositions(t *testing.T) {
	parsed, err := parser.ParseFile(token.NewFileSet(), "config_cmd.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "writeConfigShowText" {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("missing text formatter")
	}
	var order []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		literal, ok := call.Args[1].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Fatal(err)
		}
		if sel.Sel.Name == "writeSlot" {
			order = append(order, value)
		} else if sel.Sel.Name == "Fprintf" {
			name := strings.SplitN(value, " ", 2)[0]
			if strings.HasPrefix(value, "jobs=") {
				name = "jobs"
			}
			switch name {
			case "actions", "phala", "superserve", "local_container", "apple_container", "mxc", "docker_sandbox", "machine0", "cloudflare", "jobs", "aws", "aws_lambda_microvm", "azure", "digitalocean", "vultr", "linode", "github_codespaces", "azure_dynamic_sessions", "gcp", "proxmox", "xcp_ng":
				order = append(order, name)
			}
		}
		return true
	})
	if got := strings.Join(order, ","); got != "actions,blacksmith,agent_sandbox,phala,superserve,local_container,apple_container,mxc,docker_sandbox,multipass,machine0,tart,lume,cloudflare,jobs,aws,aws_lambda_microvm,azure,digitalocean,vultr,linode,github_codespaces,azure_dynamic_sessions,gcp,proxmox,firecracker,xcp_ng,parallels" {
		t.Fatalf("legacy text slot positions: %s", got)
	}
}

func TestConfigShowTextLayoutConsumesSlotsOnce(t *testing.T) {
	section := func(label string) ProviderConfigShowSection {
		return ProviderConfigShowSection{TextLabel: label, Fields: []ProviderConfigShowField{{TextName: "value", TextValue: label}}}
	}
	layout := newConfigShowTextLayout([]ProviderConfigShowSection{section("alpha"), section("lume"), section("multipass"), section("tart"), section("zeta"), section("docker_sandbox"), section("mxc"), section("apple_container"), section("local_container"), section("aws"), section("azure"), section("gcp"), section("digitalocean"), section("vultr"), section("linode")})
	var out bytes.Buffer
	for _, label := range []string{"absent", "local_container", "apple_container", "mxc", "docker_sandbox", "local_container", "apple_container", "mxc", "docker_sandbox", "multipass", "multipass", "tart", "lume", "aws", "azure", "gcp", "digitalocean", "vultr", "linode", "aws", "azure", "gcp", "digitalocean", "vultr", "linode"} {
		if err := layout.writeSlot(&out, label); err != nil {
			t.Fatal(err)
		}
	}
	if err := layout.writeRemaining(&out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "local_container value=local_container\napple_container value=apple_container\nmxc value=mxc\ndocker_sandbox value=docker_sandbox\nmultipass value=multipass\ntart value=tart\nlume value=lume\naws value=aws\nazure value=azure\ngcp value=gcp\ndigitalocean value=digitalocean\nvultr value=vultr\nlinode value=linode\nalpha value=alpha\nzeta value=zeta\n"; got != want {
		t.Fatalf("slot order/duplication got %q want %q", got, want)
	}
	out.Reset()
	if err := layout.writeSlot(&out, "lume"); err != nil {
		t.Fatal(err)
	}
	if err := layout.writeRemaining(&out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatal("consumed section emitted again")
	}
	empty := newConfigShowTextLayout(nil)
	if err := empty.writeSlot(configShowFailureWriter{err: errors.New("must not write")}, "multipass"); err != nil {
		t.Fatalf("absent optional section: %v", err)
	}
}

func TestConfigShowTextLayoutWriteErrors(t *testing.T) {
	failure := errors.New("slot write failed")
	for _, writer := range []configShowFailureWriter{{err: failure}, {short: true}} {
		section := projectionTestSection("slot_fixture")
		layout := newConfigShowTextLayout([]ProviderConfigShowSection{section})
		err := layout.writeSlot(writer, "slot_fixture")
		want := failure
		if writer.short {
			want = io.ErrShortWrite
		}
		if !errors.Is(err, want) {
			t.Fatalf("slot write error %v", err)
		}
		var got, wantText bytes.Buffer
		if err := layout.writeRemaining(&got); err != nil {
			t.Fatal(err)
		}
		if err := writeProviderConfigShowSections(&wantText, []ProviderConfigShowSection{section}); err != nil {
			t.Fatal(err)
		}
		if got.String() != wantText.String() {
			t.Fatal("failed slot was consumed")
		}
		layout = newConfigShowTextLayout([]ProviderConfigShowSection{section})
		if err := layout.writeRemaining(writer); !errors.Is(err, want) {
			t.Fatalf("remaining write error %v", err)
		}
	}
}

func TestProviderConfigShowProjectionWriteErrors(t *testing.T) {
	failure := errors.New("display write failed")
	section := projectionTestSection("test_projection")
	if err := writeProviderConfigShowSections(configShowFailureWriter{err: failure}, []ProviderConfigShowSection{section}); !errors.Is(err, failure) {
		t.Fatalf("error=%v", err)
	}
	if err := writeProviderConfigShowSections(configShowFailureWriter{short: true}, []ProviderConfigShowSection{section}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short error=%v", err)
	}
	w := &configShowWriter{Writer: configShowFailureWriter{err: failure}}
	_, _ = w.Write([]byte("first"))
	_, err := w.Write([]byte("second"))
	if !errors.Is(err, failure) || !errors.Is(w.err, failure) {
		t.Fatal("legacy error lost")
	}
}

func TestProviderConfigShowProjectionCollisionBeforeTextOutput(t *testing.T) {
	s := projectionTestSection("collision_projection")
	s.TextLabel = "config"
	p := configShowProjectionTestProvider{name: "collision_projection", section: s}
	providerRegistry[p.Name()] = p
	t.Cleanup(func() { delete(providerRegistry, p.Name()) })
	var out bytes.Buffer
	if err := writeConfigShowText(&out, Config{}); err == nil || out.Len() != 0 {
		t.Fatalf("error=%v output=%q", err, out.String())
	}
}

func TestProviderConfigShowFormattingBridges(t *testing.T) {
	if ConfigShowSecretState("") != tokenState("") || ConfigShowSecretState("synthetic") != tokenState("synthetic") {
		t.Fatal("secret-state owner changed")
	}
	value := "https://example.invalid/path?sample=value"
	if got := ConfigShowURL(value); got != redactedConfigURL(value) || strings.Contains(got, "sample=value") {
		t.Fatal("URL bridge changed")
	}
}

func TestConfigShowMigratedLegacyOwnershipRetired(t *testing.T) {
	view := configShowView(Config{})
	for _, tc := range []struct{ key, label string }{
		{"localContainer", "local_container"}, {"appleContainer", "apple_container"}, {"mxc", "mxc"}, {"dockerSandbox", "docker_sandbox"},
		{"aws", "aws"}, {"azure", "azure"}, {"gcp", "gcp"},
		{"digitalocean", "digitalocean"}, {"vultr", "vultr"}, {"linode", "linode"},
		{"blacksmith", "blacksmith"}, {"agentSandbox", "agent_sandbox"}, {"firecracker", "firecracker"},
		{"parallels", "parallels"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			if _, exists := view[tc.key]; exists {
				t.Errorf("core still owns %s values", tc.key)
			}
			for _, reserved := range strings.Fields(legacyConfigShowTextLabels) {
				if reserved == tc.label {
					t.Errorf("migrated label %s still reserved as legacy", tc.label)
				}
			}
		})
	}
}
