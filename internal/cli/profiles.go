package cli

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var presetPlaceholderPattern = regexp.MustCompile(`\{\{[A-Za-z_][A-Za-z0-9_.-]*\}\}`)

type runProfileExpansion struct {
	Profile       ProfileConfig
	Preset        PresetConfig
	PresetName    string
	Variables     map[string]string
	Env           map[string]string
	ArtifactGlobs []string
	ProofTemplate string
	Command       []string
	LiteralArgs   map[int]bool
	Shell         bool
	Preflight     bool
}

func applySelectedProfileConfig(cfg *Config) error {
	name := strings.TrimSpace(cfg.Profile)
	if name == "" {
		return nil
	}
	profile, ok := cfg.Profiles[name]
	if !ok {
		return nil
	}
	if err := validateProfileConfig(name, profile); err != nil {
		return err
	}
	cfg.EnvAllow = appendUniqueStrings(cfg.EnvAllow, profile.EnvAllow...)
	if len(profile.Presets) > 0 {
		if cfg.Presets == nil {
			cfg.Presets = map[string]PresetConfig{}
		}
		for presetName, preset := range profile.Presets {
			cfg.Presets[presetName] = preset
		}
	}
	if len(profile.ProofTemplates) > 0 {
		if cfg.ProofTemplates == nil {
			cfg.ProofTemplates = map[string]ProofTemplateConfig{}
		}
		for templateName, tmpl := range profile.ProofTemplates {
			cfg.ProofTemplates[templateName] = tmpl
		}
	}
	return nil
}

func validateProfileConfig(name string, profile ProfileConfig) error {
	if err := validateProfileEnvNames("profile "+name, profile.Env); err != nil {
		return err
	}
	for presetName, preset := range profile.Presets {
		if err := validateProfileEnvNames("profile "+name+" preset "+presetName, preset.Env); err != nil {
			return err
		}
	}
	if profile.Doctor.Enabled {
		if err := validateProfileDoctorTools(profile.Doctor.Tools); err != nil {
			return exit(2, "profile %q doctor: %v", name, err)
		}
	}
	return nil
}

func validateProfileDoctorTools(tools []string) error {
	for _, tool := range normalizePreflightToolNames(tools) {
		if tool == "none" {
			continue
		}
		spec, ok := preflightToolRegistry[tool]
		if !ok {
			return exit(2, "unknown preflight tool %q", tool)
		}
		if tool == "sudo" {
			continue
		}
		if len(spec.Posix) == 0 {
			return exit(2, "profile doctor tool %q is not supported for POSIX profile doctor", tool)
		}
	}
	return nil
}

func expandRunProfile(cfg Config, presetName, scenario string, vars []string, command []string, shellMode bool, preflight bool, artifactGlobs []string, proofTemplate string) (runProfileExpansion, error) {
	variables, err := parsePresetVariables(vars)
	if err != nil {
		return runProfileExpansion{}, err
	}
	if strings.TrimSpace(scenario) != "" {
		variables["scenario"] = strings.TrimSpace(scenario)
	}
	expansion := runProfileExpansion{
		Profile:       cfg.Profiles[strings.TrimSpace(cfg.Profile)],
		PresetName:    strings.TrimSpace(presetName),
		Variables:     variables,
		Env:           map[string]string{},
		ArtifactGlobs: append([]string{}, artifactGlobs...),
		ProofTemplate: strings.TrimSpace(proofTemplate),
		Command:       append([]string{}, command...),
		Shell:         shellMode,
		Preflight:     preflight,
	}
	for key, value := range expansion.Profile.Env {
		expanded, err := expandPresetTemplateValue("profile "+cfg.Profile+" env "+key, value, variables)
		if err != nil {
			return runProfileExpansion{}, err
		}
		expansion.Env[key] = expanded
	}
	expansion.ArtifactGlobs = appendUniqueStrings(expansion.ArtifactGlobs, expansion.Profile.ArtifactGlobs...)
	if expansion.PresetName == "" {
		return expansion, nil
	}
	preset, ok := cfg.Presets[expansion.PresetName]
	if !ok {
		return runProfileExpansion{}, exit(2, "preset %q is not configured for profile %q", expansion.PresetName, cfg.Profile)
	}
	if err := validateProfileEnvNames("preset "+expansion.PresetName, preset.Env); err != nil {
		return runProfileExpansion{}, err
	}
	expansion.Preset = preset
	for key, value := range preset.Env {
		expanded, err := expandPresetTemplateValue("preset "+expansion.PresetName+" env "+key, value, variables)
		if err != nil {
			return runProfileExpansion{}, err
		}
		expansion.Env[key] = expanded
	}
	expansion.ArtifactGlobs = appendUniqueStrings(expansion.ArtifactGlobs, preset.ArtifactGlobs...)
	if preset.ProofTemplate != "" && expansion.ProofTemplate == "" {
		expansion.ProofTemplate = preset.ProofTemplate
	}
	if preset.Preflight {
		expansion.Preflight = true
	}
	if preset.Shell {
		expansion.Shell = true
	}
	if strings.TrimSpace(preset.Command) != "" {
		if err := validatePresetTemplatePlaceholders("preset "+expansion.PresetName+" command", preset.Command, variables); err != nil {
			return runProfileExpansion{}, err
		}
		if expansion.Shell {
			expanded := expandPresetValue(preset.Command, variables)
			expansion.Command = []string{expanded}
		} else {
			words, wordLiterals, err := splitShellWordsWithLiterals(preset.Command)
			if err != nil {
				return runProfileExpansion{}, exit(2, "preset %q command: %v", expansion.PresetName, err)
			}
			expansion.Command, expansion.LiteralArgs = expandPresetWords(words, wordLiterals, variables)
		}
	}
	if len(expansion.Command) == 0 {
		return runProfileExpansion{}, exit(2, "preset %q expanded to an empty command", expansion.PresetName)
	}
	return expansion, nil
}

func parsePresetVariables(values []string) (map[string]string, error) {
	out := map[string]string{}
	for _, value := range values {
		for _, part := range splitCommaList(value) {
			key, val, ok := strings.Cut(part, "=")
			key = strings.TrimSpace(key)
			if !ok || key == "" {
				return nil, exit(2, "--preset-var expects name=value")
			}
			out[key] = strings.TrimSpace(val)
		}
	}
	return out, nil
}

func expandPresetTemplateValue(label, value string, vars map[string]string) (string, error) {
	if err := validatePresetTemplatePlaceholders(label, value, vars); err != nil {
		return "", err
	}
	return expandPresetValue(value, vars), nil
}

func expandPresetWords(words []string, wordLiterals map[int]bool, vars map[string]string) ([]string, map[int]bool) {
	out := make([]string, 0, len(words))
	literals := map[int]bool{}
	for idx, word := range words {
		if wordLiterals[idx] || isPresetPlaceholderWord(word) {
			literals[idx] = true
		}
		out = append(out, expandPresetValue(word, vars))
	}
	if len(literals) == 0 {
		literals = nil
	}
	return out, literals
}

func isPresetPlaceholderWord(word string) bool {
	matches := presetPlaceholderPattern.FindAllString(word, -1)
	return len(matches) == 1 && matches[0] == word
}

func validateProfileEnvNames(label string, env map[string]string) error {
	for key := range env {
		if !validEnvName(strings.TrimSpace(key)) {
			return exit(2, "%s env key %q is not a valid shell environment name", label, key)
		}
	}
	return nil
}

func expandPresetValue(value string, vars map[string]string) string {
	out := value
	keys := make([]string, 0, len(vars))
	for key := range vars {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = strings.ReplaceAll(out, "{{"+key+"}}", vars[key])
	}
	return out
}

func validatePresetTemplatePlaceholders(label, value string, vars map[string]string) error {
	matches := presetPlaceholderPattern.FindAllString(value, -1)
	if len(matches) == 0 {
		return nil
	}
	var missing []string
	for _, match := range appendUniqueStrings(nil, matches...) {
		key := strings.TrimSuffix(strings.TrimPrefix(match, "{{"), "}}")
		if _, ok := vars[key]; !ok {
			missing = append(missing, match)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return exit(2, "%s has unresolved preset variable(s): %s", label, strings.Join(missing, ", "))
}

func formatExpandedPresetCommand(name string, command []string, shell bool, env map[string]string, literalArgs map[int]bool) string {
	parts := []string{}
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for key := range env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			parts = append(parts, envMetadata(key, env[key]))
		}
	}
	parts = append(parts, runCommandDisplayWithLiteralArgs(command, shell, literalArgs))
	if name == "" {
		return "expanded command: " + strings.Join(parts, " ")
	}
	return "expanded preset " + name + ": " + strings.Join(parts, " ")
}

func splitShellWords(value string) ([]string, error) {
	words, _, err := splitShellWordsWithLiterals(value)
	return words, err
}

func splitShellWordsWithLiterals(value string) ([]string, map[int]bool, error) {
	words := []string{}
	literals := map[int]bool{}
	var b strings.Builder
	inSingle := false
	inDouble := false
	haveWord := false
	literalWord := false
	appendWord := func() {
		if !haveWord {
			return
		}
		if literalWord {
			literals[len(words)] = true
		}
		words = append(words, b.String())
		b.Reset()
		haveWord = false
		literalWord = false
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		switch {
		case inSingle:
			if ch == '\'' {
				inSingle = false
			} else {
				b.WriteByte(ch)
				haveWord = true
				literalWord = true
			}
		case inDouble:
			switch ch {
			case '"':
				inDouble = false
			case '\\':
				if i+1 >= len(value) {
					b.WriteByte(ch)
					haveWord = true
					literalWord = true
					continue
				}
				i++
				next := value[i]
				if next == '$' || next == '`' || next == '"' || next == '\\' || next == '\n' {
					b.WriteByte(next)
				} else {
					b.WriteByte(ch)
					b.WriteByte(next)
				}
				haveWord = true
				literalWord = true
			default:
				b.WriteByte(ch)
				haveWord = true
				literalWord = true
			}
		default:
			switch ch {
			case ' ', '\t', '\n', '\r':
				appendWord()
			case '\'':
				inSingle = true
				haveWord = true
				literalWord = true
			case '"':
				inDouble = true
				haveWord = true
				literalWord = true
			case '\\':
				if i+1 >= len(value) {
					return nil, nil, fmt.Errorf("trailing escape")
				}
				i++
				b.WriteByte(value[i])
				haveWord = true
				literalWord = true
			default:
				b.WriteByte(ch)
				haveWord = true
			}
		}
	}
	if inSingle {
		return nil, nil, fmt.Errorf("unterminated single quote")
	}
	if inDouble {
		return nil, nil, fmt.Errorf("unterminated double quote")
	}
	appendWord()
	if len(literals) == 0 {
		literals = nil
	}
	return words, literals, nil
}
