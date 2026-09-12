package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

type providerAuthenticationRouteView struct {
	Route       string                         `json:"route"`
	Methods     []ProviderAuthenticationMethod `json:"methods"`
	Description string                         `json:"description"`
}

type providerAuthenticationView struct {
	Scope   string                            `json:"scope"`
	Methods []ProviderAuthenticationMethod    `json:"methods"`
	Routes  []providerAuthenticationRouteView `json:"routes"`
	Status  string                            `json:"status"`
}

type providerStaticStatus struct {
	MetadataKind   string                     `json:"metadataKind"`
	Authentication providerAuthenticationView `json:"authentication"`
	Readiness      string                     `json:"readiness"`
}

func providerStaticStatusFor(spec ProviderSpec) providerStaticStatus {
	auth := providerAuthenticationView{
		Scope: "provider_access", Status: "unchecked",
		Methods: []ProviderAuthenticationMethod{}, Routes: []providerAuthenticationRouteView{},
	}
	seen := map[ProviderAuthenticationMethod]bool{}
	for _, route := range spec.Authentication {
		methods := append([]ProviderAuthenticationMethod{}, route.Methods...)
		auth.Routes = append(auth.Routes, providerAuthenticationRouteView{route.Route, methods, route.Description})
		for _, method := range methods {
			if !seen[method] {
				seen[method] = true
				auth.Methods = append(auth.Methods, method)
			}
		}
	}
	sort.Slice(auth.Methods, func(i, j int) bool { return auth.Methods[i] < auth.Methods[j] })
	return providerStaticStatus{MetadataKind: "static", Authentication: auth, Readiness: "unchecked"}
}

func (status providerStaticStatus) clone() providerStaticStatus {
	status.Authentication.Methods = append([]ProviderAuthenticationMethod{}, status.Authentication.Methods...)
	status.Authentication.Routes = append([]providerAuthenticationRouteView{}, status.Authentication.Routes...)
	for i := range status.Authentication.Routes {
		status.Authentication.Routes[i].Methods = append([]ProviderAuthenticationMethod{}, status.Authentication.Routes[i].Methods...)
	}
	return status
}

type configInputView struct {
	State    string   `json:"state"`
	Sources  []string `json:"sources"`
	Complete bool     `json:"complete"`
}

func configInputViewFor(ledger configInputLedger, owner configInputOwner) configInputView {
	input := ledger.summary(owner)
	return configInputView{input.state, input.sources, input.complete}
}

type providerConfigurationView struct {
	State         string          `json:"state"`
	ProviderInput configInputView `json:"providerInput"`
	GenericInput  configInputView `json:"genericInput"`
}

func providerConfigurationFor(cfg Config, owner configInputOwner) providerConfigurationView {
	view := providerConfigurationView{
		State: "unknown", ProviderInput: configInputViewFor(cfg.inputProvenance, owner),
		GenericInput: configInputViewFor(cfg.inputProvenance, configInputGeneric),
	}
	switch {
	case view.ProviderInput.State == "present":
		view.State = "explicit"
	case view.ProviderInput.State == "none" && view.GenericInput.State == "present":
		view.State = "generic_inputs_present"
	case view.ProviderInput.State == "none" && view.GenericInput.State == "none":
		view.State = "defaults_only"
	}
	return view
}

type providerSelectionView struct {
	Selected bool                     `json:"selected"`
	Source   *providerSelectionSource `json:"source"`
}

type providerConfigStatusEntry struct {
	Supported      bool                       `json:"supported"`
	Selection      providerSelectionView      `json:"selection"`
	Configuration  providerConfigurationView  `json:"configuration"`
	Authentication providerAuthenticationView `json:"authentication"`
	Readiness      string                     `json:"readiness"`
}

type providerConfigStatusView struct {
	SchemaVersion int                                  `json:"schemaVersion"`
	Kind          string                               `json:"kind"`
	Providers     map[string]providerConfigStatusEntry `json:"providers"`
}

// providerConfigStatus reads only accepted-input facts and declared metadata.
// Config values, credential presence and live provider state are not evidence.
func providerConfigStatus(cfg Config) providerConfigStatusView {
	view := providerConfigStatusView{SchemaVersion: 1, Kind: "offline", Providers: map[string]providerConfigStatusEntry{}}
	selected := ""
	if providerSelectionIsActionable(cfg) {
		if provider, err := ProviderFor(cfg.Provider); err == nil {
			selected = provider.Name()
		}
	}
	for _, provider := range registeredProviders() {
		name := provider.Name()
		static := providerStaticStatusFor(provider.Spec())
		selection := providerSelectionView{Selected: name == selected}
		if selection.Selected {
			source := cfg.providerSelectionSource
			selection.Source = &source
		}
		view.Providers[name] = providerConfigStatusEntry{
			Supported: true, Selection: selection, Configuration: providerConfigurationFor(cfg, configInputOwner(name)),
			Authentication: static.Authentication, Readiness: static.Readiness,
		}
	}
	return view
}

func authenticationInterfaces(auth providerAuthenticationView) string {
	interfaces := make([]string, 0, len(auth.Routes))
	for _, route := range auth.Routes {
		methods := make([]string, len(route.Methods))
		for i, method := range route.Methods {
			methods[i] = string(method)
		}
		interfaces = append(interfaces, route.Route+":"+strings.Join(methods, ","))
	}
	return blank(strings.Join(interfaces, ";"), "-")
}

func writeProviderStaticStatus(out io.Writer, status providerStaticStatus) {
	fmt.Fprintf(out, "  metadata: %s; authentication: %s; readiness: %s\n", status.MetadataKind, status.Authentication.Status, status.Readiness)
	fmt.Fprintf(out, "  auth interfaces (possible, not requirements): %s\n", authenticationInterfaces(status.Authentication))
}

func writeProviderConfigStatus(out io.Writer, view providerConfigStatusView) {
	fmt.Fprintln(out, "inspection=offline; visible values may be defaults; authentication and readiness are unchecked")
	names := make([]string, 0, len(view.Providers))
	for name := range view.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry := view.Providers[name]
		source := "-"
		if entry.Selection.Source != nil {
			source = string(*entry.Selection.Source)
		}
		input := entry.Configuration
		fmt.Fprintf(out, "provider_status name=%s supported=%t selected=%t selection_source=%s configuration=%s input_coverage_complete=%t provider_sources=%s generic_sources=%s auth_status=%s readiness=%s auth_interfaces=%s\n",
			name, entry.Supported, entry.Selection.Selected, source, input.State,
			input.ProviderInput.Complete && input.GenericInput.Complete,
			commaOrDash(input.ProviderInput.Sources), commaOrDash(input.GenericInput.Sources),
			entry.Authentication.Status, entry.Readiness, authenticationInterfaces(entry.Authentication))
	}
}
