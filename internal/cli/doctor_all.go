package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

type doctorAllOptions struct {
	Providers    []string
	JSON         bool
	PrepareCheck bool
}

type doctorAllJSONOutput struct {
	OK        bool               `json:"ok"`
	Providers []doctorJSONOutput `json:"providers"`
}

func (a App) doctorAll(ctx context.Context, opts doctorAllOptions) error {
	providers := normalizeDoctorAllProviders(opts.Providers)
	if len(providers) == 0 {
		return exit(2, "doctor --all requires at least one provider")
	}
	results := make([]doctorJSONOutput, 0, len(providers))
	ok := true
	for _, provider := range providers {
		var stdout bytes.Buffer
		err := (App{Stdout: &stdout, Stderr: a.Stderr, Stdin: a.Stdin}).doctor(ctx, []string{"--provider", provider, "--json"})
		var result doctorJSONOutput
		if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
			result = doctorJSONOutput{
				OK:       false,
				Provider: provider,
				Checks: []doctorJSONCheck{{
					Status:  "failed",
					Check:   "doctor",
					Message: fmt.Sprintf("provider=%s json_decode_failed=%v", provider, decodeErr),
					Details: map[string]string{"provider": provider, "error": decodeErr.Error()},
				}},
			}
		}
		if opts.PrepareCheck {
			result.Checks = append(result.Checks, doctorPrepareCheck(result.Provider)...)
			result.OK = result.OK && !doctorPrepareChecksFail(result.Checks)
		}
		if err != nil {
			result.OK = false
		}
		if !result.OK {
			ok = false
		}
		results = append(results, result)
	}
	if opts.JSON {
		if err := json.NewEncoder(a.Stdout).Encode(doctorAllJSONOutput{OK: ok, Providers: results}); err != nil {
			return err
		}
	} else {
		for _, result := range results {
			status := "ok"
			if !result.OK {
				status = "failed"
			}
			fmt.Fprintf(a.Stdout, "%s provider=%s\n", status, result.Provider)
			for _, check := range result.Checks {
				if check.Message != "" {
					fmt.Fprintf(a.Stdout, "  %-7s %-16s %s\n", check.Status, check.Check, check.Message)
				} else {
					fmt.Fprintf(a.Stdout, "  %-7s %s\n", check.Status, check.Check)
				}
			}
		}
	}
	if !ok {
		return exit(1, "doctor found problems")
	}
	return nil
}

func normalizeDoctorAllProviders(values []string) []string {
	seen := map[string]struct{}{}
	var providers []string
	for _, raw := range values {
		for _, item := range splitCommaList(raw) {
			name := strings.TrimSpace(item)
			if name == "" {
				continue
			}
			if provider, err := ProviderFor(name); err == nil {
				name = provider.Name()
			}
			key := normalizeProviderName(name)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			providers = append(providers, name)
		}
	}
	return providers
}

func doctorPrepareCheck(provider string) []doctorJSONCheck {
	cfg, err := loadConfig()
	if err != nil {
		return []doctorJSONCheck{{Status: "failed", Check: "prepare", Provider: provider, Message: err.Error(), Details: map[string]string{"provider": provider, "error": err.Error()}}}
	}
	configuredProvider := cfg.Provider
	cfg.Provider = provider
	if resolved, err := ProviderFor(provider); err == nil {
		cfg.Provider = resolved.Name()
	}
	if !cfg.ServerTypeExplicit || normalizeProviderName(configuredProvider) != normalizeProviderName(cfg.Provider) {
		cfg.ServerType = ""
		cfg.ServerTypeExplicit = false
	}
	if err := routeConfiguredProvider(&cfg); err != nil {
		return []doctorJSONCheck{{Status: "failed", Check: "prepare", Provider: provider, Message: err.Error(), Details: map[string]string{"provider": provider, "error": err.Error()}}}
	}
	if err := applyProviderConfigDefaults(&cfg); err != nil {
		return []doctorJSONCheck{{Status: "failed", Check: "prepare", Provider: provider, Message: err.Error(), Details: map[string]string{"provider": provider, "error": err.Error()}}}
	}
	if cfg.ServerType == "" {
		cfg.ServerType = serverTypeForConfig(cfg)
	}
	details := map[string]string{
		"provider": cfg.Provider,
		"type":     cfg.ServerType,
		"class":    cfg.Class,
		"workflow": blank(cfg.Actions.Workflow, "-"),
		"job":      blank(cfg.Actions.Job, "-"),
	}
	workflow := cfg.Actions.Workflow
	job := cfg.Actions.Job
	if isBlacksmithProvider(cfg.Provider) {
		workflow = doctorBlacksmithWorkflow(cfg)
		job = doctorBlacksmithJob(cfg)
		details["workflow"] = blank(workflow, "-")
		details["job"] = blank(job, "-")
	}
	status := "ok"
	message := fmt.Sprintf("provider=%s class=%s type=%s actions_workflow=%s actions_job=%s", cfg.Provider, cfg.Class, cfg.ServerType, blank(workflow, "-"), blank(job, "-"))
	if workflow == "" {
		if isBlacksmithProvider(cfg.Provider) {
			status = "missing"
		} else {
			status = "warning"
		}
		message += " hydrate=missing"
		details["hydrate"] = "missing"
	}
	if largeDefaultServerType(cfg) {
		if status == "ok" {
			status = "warning"
		}
		message += " large_default=true"
		details["large_default"] = "true"
	}
	return []doctorJSONCheck{{Status: status, Check: "prepare", Provider: cfg.Provider, Message: message, Details: details}}
}

func doctorBlacksmithWorkflow(cfg Config) string {
	if cfg.Blacksmith.Workflow != "" {
		return cfg.Blacksmith.Workflow
	}
	if doctorBlacksmithCanFallbackToActionsWorkflow(cfg) {
		return cfg.Actions.Workflow
	}
	return ""
}

func doctorBlacksmithJob(cfg Config) string {
	if cfg.Blacksmith.Job != "" {
		return cfg.Blacksmith.Job
	}
	if cfg.Blacksmith.Workflow != "" || doctorBlacksmithCanFallbackToActionsWorkflow(cfg) {
		return cfg.Actions.Job
	}
	return ""
}

func doctorBlacksmithCanFallbackToActionsWorkflow(cfg Config) bool {
	workflow := strings.TrimSpace(cfg.Actions.Workflow)
	if workflow == "" {
		return false
	}
	name := strings.TrimSpace(filepath.Base(workflow))
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	normalized := strings.NewReplacer(" ", "", "-", "", "_", "").Replace(strings.ToLower(stem))
	switch normalized {
	case "hydrate", "crabbox", "crabboxhydrate":
		return false
	default:
		return true
	}
}

func largeDefaultServerType(cfg Config) bool {
	serverType := strings.ToLower(strings.TrimSpace(cfg.ServerType))
	if serverType == "" || cfg.ServerTypeExplicit {
		return false
	}
	return strings.Contains(serverType, "192") || strings.Contains(serverType, "48xlarge")
}

func doctorPrepareChecksFail(checks []doctorJSONCheck) bool {
	for _, check := range checks {
		if check.Check == "prepare" && doctorStatusFails(check.Status) {
			return true
		}
	}
	return false
}
