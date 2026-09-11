package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// ProviderConfigShowProjector supplies passive, already-redacted display data.
// Implementations must not resolve defaults, credentials, files or native state.
type ProviderConfigShowProjector interface {
	ConfigShowSection(Config) ProviderConfigShowSection
}

type ProviderConfigShowSection struct {
	JSONKey   string
	TextLabel string
	Providers []string
	Fields    []ProviderConfigShowField
}

// A field may belong to only one format by leaving the other name empty.
type ProviderConfigShowField struct {
	JSONName  string
	JSONValue any
	TextName  string
	TextValue string
}

// ConfigShowURL uses the existing configuration-display URL redactor.
func ConfigShowURL(value string) string { return redactedConfigURL(value) }

// ConfigShowSecretState reports presence only, using the existing display owner.
func ConfigShowSecretState(value string) string { return tokenState(value) }

// Names only: no legacy value projection or environment reads are needed to
// protect existing text slots. Retire a name only when its legacy row migrates.
const legacyConfigShowTextLabels = "config provider lease broker access_auth ssh sync env run capacity actions blacksmith agent_sandbox phala namespace namespace_instance morph e2b cubesandbox upstash_box smolvm blaxel nomad ascii_box superserve local_container apple_container mxc docker_sandbox multipass machine0 tart lume cloudflare fastapi_cloud cloudflare_dynamic_workers cloudflare_sandbox static results cache jobs aws aws_lambda_microvm azure digitalocean vultr linode github_codespaces lambda vast nvidia_brev nebius hostinger ovh scaleway tencentcloud azure_dynamic_sessions gcp proxmox firecracker xcp_ng parallels inspection provider_status"

func collectProviderConfigShowSections(cfg Config) ([]ProviderConfigShowSection, error) {
	return collectProviderConfigShowSectionsFrom(cfg, registeredProviders())
}

func configShowNameValid(name string) bool {
	return name != "" && !strings.ContainsAny(name, " \t\r\n=")
}

func collectProviderConfigShowSectionsFrom(cfg Config, providers []Provider) ([]ProviderConfigShowSection, error) {
	known := make(map[string]bool, len(providers))
	for _, provider := range providers {
		known[provider.Name()] = true
	}
	keys, labels, covered := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, label := range strings.Fields(legacyConfigShowTextLabels) {
		labels[label] = true
	}
	var sections []ProviderConfigShowSection
	for _, provider := range providers {
		projector, ok := provider.(ProviderConfigShowProjector)
		if !ok {
			continue
		}
		section := projector.ConfigShowSection(cfg)
		if !configShowNameValid(section.JSONKey) || !configShowNameValid(section.TextLabel) || len(section.Providers) == 0 || len(section.Fields) == 0 {
			return nil, fmt.Errorf("provider %s: incomplete config-show section", provider.Name())
		}
		if keys[section.JSONKey] || labels[section.TextLabel] {
			return nil, fmt.Errorf("provider %s: duplicate config-show section key or text label", provider.Name())
		}
		keys[section.JSONKey], labels[section.TextLabel] = true, true
		ownerIncluded := false
		for _, name := range section.Providers {
			if !known[name] || covered[name] {
				return nil, fmt.Errorf("provider %s: unknown or duplicate config-show coverage %q", provider.Name(), name)
			}
			covered[name] = true
			ownerIncluded = ownerIncluded || name == provider.Name()
		}
		if !ownerIncluded {
			return nil, fmt.Errorf("provider %s: config-show coverage omits its owner", provider.Name())
		}
		jsonNames, textNames := map[string]bool{}, map[string]bool{}
		for _, field := range section.Fields {
			if field.JSONName == "" && field.TextName == "" {
				return nil, fmt.Errorf("provider %s: unnamed config-show field", provider.Name())
			}
			if field.JSONName != "" {
				if !configShowNameValid(field.JSONName) || jsonNames[field.JSONName] {
					return nil, fmt.Errorf("provider %s: invalid or duplicate config-show JSON field %q", provider.Name(), field.JSONName)
				}
				jsonNames[field.JSONName] = true
			}
			if field.TextName != "" {
				if !configShowNameValid(field.TextName) || textNames[field.TextName] {
					return nil, fmt.Errorf("provider %s: invalid or duplicate config-show text field %q", provider.Name(), field.TextName)
				}
				textNames[field.TextName] = true
			}
		}
		if len(jsonNames) == 0 || len(textNames) == 0 {
			return nil, fmt.Errorf("provider %s: config-show section requires both formats", provider.Name())
		}
		sections = append(sections, section)
	}
	sort.SliceStable(sections, func(i, j int) bool { return sections[i].TextLabel < sections[j].TextLabel })
	return sections, nil
}

func addProviderConfigShowSections(view map[string]any, sections []ProviderConfigShowSection) error {
	seen := map[string]bool{}
	for _, section := range sections {
		if _, exists := view[section.JSONKey]; exists || seen[section.JSONKey] {
			return fmt.Errorf("config-show JSON key %q already exists", section.JSONKey)
		}
		seen[section.JSONKey] = true
	}
	for _, section := range sections {
		fields := make(map[string]any)
		for _, field := range section.Fields {
			if field.JSONName != "" {
				fields[field.JSONName] = field.JSONValue
			}
		}
		view[section.JSONKey] = fields
	}
	return nil
}

func writeProviderConfigShowSections(w io.Writer, sections []ProviderConfigShowSection) error {
	for _, section := range sections {
		var line strings.Builder
		line.WriteString(section.TextLabel)
		for _, field := range section.Fields {
			if field.TextName == "" {
				continue
			}
			line.WriteByte(' ')
			line.WriteString(field.TextName)
			line.WriteByte('=')
			line.WriteString(field.TextValue)
		}
		line.WriteByte('\n')
		n, err := io.WriteString(w, line.String())
		if err != nil {
			return err
		}
		if n != line.Len() {
			return io.ErrShortWrite
		}
	}
	return nil
}

// Capture legacy fmt write failures without changing its published line assembly.
type configShowWriter struct {
	io.Writer
	err error
}

func (w *configShowWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.Writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	w.err = err
	return n, err
}
