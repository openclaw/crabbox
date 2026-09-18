package cli

import "maps"

// These sources describe accepted input in this load, not the winning source of
// each value. Defaults, selected providers and observed runtime state are absent.
type configInputSource uint8

const (
	configInputUser configInputSource = iota + 1
	configInputRepo
	configInputEnvironment
	configInputFlag
)

type configInputEffect uint8

const (
	configInputValue configInputEffect = 1 << iota
	configInputIntent
)

type configInputOwner string

const configInputGeneric configInputOwner = "$generic"

type configInputFacts struct {
	values   uint8
	intents  uint8
	complete bool
}

// configInputLedger is copy-on-write because Config is often copied by value.
// Application owners record only finalized effects, after any layer rollback.
type configInputLedger map[configInputOwner]configInputFacts

func recordConfigInput(cfg *Config, owner configInputOwner, source configInputSource, accepted bool) {
	if accepted {
		if source == configInputFlag && cfg.synthesizedFlagInputs {
			return
		}
		cfg.inputProvenance = cfg.inputProvenance.withInput(owner, source, configInputValue)
	}
}

// Stored job arguments are derived inputs, not fresh external flags. Preserve
// known source facts while leaving these execution snapshots unqualified.
func markSynthesizedFlagInputs(cfg *Config, synthesized bool) {
	if !synthesized {
		return
	}
	cfg.synthesizedFlagInputs = true
	for owner := range cfg.inputProvenance {
		cfg.inputProvenance = cfg.inputProvenance.withCoverage(owner, false)
	}
}

func configInputSourceForFile(source providerSelectionSource) configInputSource {
	switch source {
	case providerSelectionUserConfig:
		return configInputUser
	case providerSelectionRepoConfig:
		return configInputRepo
	default:
		return 0
	}
}

// RecordProviderFlagInputs records accepted settings for the named canonical
// providers. Shared configuration owners name each affected provider explicitly.
// Calling this for a selected provider or an ignored flag is not acceptance.
func RecordProviderFlagInputs(cfg *Config, accepted bool, providers ...string) {
	for _, provider := range providers {
		recordConfigInput(cfg, configInputOwner(provider), configInputFlag, accepted)
	}
}

func (ledger configInputLedger) withInput(owner configInputOwner, source configInputSource, effect configInputEffect) configInputLedger {
	if owner == "" || source < configInputUser || source > configInputFlag {
		return ledger
	}
	facts := ledger[owner]
	bit := uint8(1 << (source - 1))
	if effect&configInputValue != 0 {
		facts.values |= bit
	}
	if effect&configInputIntent != 0 {
		facts.intents |= bit
	}
	return ledger.withFacts(owner, facts)
}

// Completeness certifies the owner's entire input history, not its latest layer.
// Uninstrumented applications invalidate it; only complete history accounting
// may restore it. A value or event alone never establishes coverage.
func (ledger configInputLedger) withCoverage(owner configInputOwner, complete bool) configInputLedger {
	if owner == "" {
		return ledger
	}
	facts := ledger[owner]
	facts.complete = complete
	return ledger.withFacts(owner, facts)
}

func (ledger configInputLedger) withFacts(owner configInputOwner, facts configInputFacts) configInputLedger {
	if ledger[owner] == facts {
		return ledger
	}
	next := maps.Clone(ledger)
	if next == nil {
		next = make(configInputLedger)
	}
	next[owner] = facts
	return next
}

type configInputSummary struct {
	state    string
	sources  []string
	effects  configInputEffect
	complete bool
}

func (ledger configInputLedger) summary(owner configInputOwner) configInputSummary {
	facts := ledger[owner]
	result := configInputSummary{state: "unknown", sources: []string{}, complete: facts.complete}
	if facts.values != 0 {
		result.effects |= configInputValue
	}
	if facts.intents != 0 {
		result.effects |= configInputIntent
	}
	if result.effects != 0 {
		result.state = "present"
	} else if facts.complete {
		result.state = "none"
	}
	for i, name := range [...]string{"user_config", "repo_config", "environment", "flag"} {
		if (facts.values|facts.intents)&(1<<i) != 0 {
			result.sources = append(result.sources, name)
		}
	}
	return result
}
