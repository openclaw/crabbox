package cli

import (
	"flag"
	"fmt"
	"slices"
	"strings"
	"time"
)

// LeaseLifetimePolicy preserves the coordinator's recorded policy and observations.
type LeaseLifetimePolicy struct {
	Mode             string                  `json:"mode"`
	Source           string                  `json:"source,omitempty"`
	Workload         string                  `json:"workload"`
	DeadlineAt       string                  `json:"deadlineAt"`
	EffectiveStopAt  string                  `json:"effectiveStopAt,omitempty"`
	RetainSeconds    int                     `json:"retainSeconds"`
	Extension        *LeaseLifetimeExtension `json:"extension,omitempty"`
	WarnedAt         string                  `json:"warnedAt,omitempty"`
	DeletionWarnedAt string                  `json:"deletionWarnedAt,omitempty"`
	CheckedAt        string                  `json:"checkedAt,omitempty"`
	NextCheckAt      string                  `json:"nextCheckAt,omitempty"`
	Status           string                  `json:"status,omitempty"`
	Error            string                  `json:"error,omitempty"`
	StoppedAt        string                  `json:"stoppedAt,omitempty"`
	StopStartedAt    string                  `json:"stopStartedAt,omitempty"`
	DeleteStartedAt  string                  `json:"deleteStartedAt,omitempty"`
	DeleteAfterAt    string                  `json:"deleteAfterAt,omitempty"`
	RetainedIdentity string                  `json:"retainedIdentity,omitempty"`
	Warnings         []string                `json:"warnings,omitempty"`
	Readiness        *LeaseLifetimeReadiness `json:"readiness,omitempty"`
	Compute          *LeaseLifetimeCompute   `json:"compute,omitempty"`
}

type LeaseLifetimeExtension struct {
	Owner     string `json:"owner"`
	GrantedBy string `json:"grantedBy,omitempty"`
	Reason    string `json:"reason"`
	GrantedAt string `json:"grantedAt"`
	ExpiresAt string `json:"expiresAt"`
}

type LeaseLifetimeReadiness struct {
	Status    string `json:"status"`
	CheckedAt string `json:"checkedAt"`
	Error     string `json:"error,omitempty"`
}

type LeaseLifetimeCompute struct {
	State         string `json:"state"`
	RunningSince  string `json:"runningSince,omitempty"`
	AccumulatedMs int64  `json:"accumulatedMs"`
}

type LeaseLifetimeRequest struct {
	Mode          string `json:"mode"`
	MaxAgeSeconds int    `json:"maxAgeSeconds"`
	RetainSeconds int    `json:"retainSeconds"`
}

func applyLifetimeCanaryFlags(cfg *Config, fs *flag.FlagSet, values leaseCreateFlagValues, target leaseFlagTarget) error {
	if !flagWasSet(fs, "lifetime-canary") && !flagWasSet(fs, "lifetime-retention") {
		return nil
	}
	if !flagWasSet(fs, "lifetime-canary") || !flagWasSet(fs, "lifetime-retention") {
		return Exit(2, "--lifetime-canary and --lifetime-retention must be supplied together")
	}
	if target.Reuse || (fs.Name() != "warmup" && fs.Name() != "run") {
		return Exit(2, "lifetime canary flags require a new warmup or run lease; existing lease reuse is unsupported")
	}
	if pool := fs.Lookup("pool"); pool != nil && strings.TrimSpace(pool.Value.String()) != "" {
		return Exit(2, "lifetime canary flags are unsupported with ready pools")
	}
	if cfg.Provider != "aws" || strings.TrimSpace(cfg.Coordinator) == "" || cfg.BrokerMode == BrokerModeRegistered {
		return Exit(2, "lifetime canaries require a coordinator-managed AWS lease")
	}
	if cfg.Capacity.Market != "on-demand" || cfg.TargetOS == targetMacOS || strings.TrimSpace(cfg.AWSMacHostID) != "" {
		return Exit(2, "lifetime canaries require an AWS on-demand lease without a dedicated Mac host")
	}
	for _, value := range []struct {
		name     string
		duration time.Duration
	}{{"lifetime-canary", *values.LifetimeCanary}, {"lifetime-retention", *values.LifetimeRetention}} {
		if value.duration < time.Minute || value.duration > 31*24*time.Hour || value.duration%time.Second != 0 {
			return Exit(2, "--%s must be a whole number of seconds between 1m and 744h", value.name)
		}
	}
	cfg.LifetimePolicy = &LeaseLifetimeRequest{Mode: "enforce", MaxAgeSeconds: int(values.LifetimeCanary.Seconds()), RetainSeconds: int(values.LifetimeRetention.Seconds())}
	// A canary explicitly requests administrator authorization; use its configured
	// credential without retrying a denied administrator as an ordinary caller.
	if cfg.CoordAdminToken != "" {
		cfg.CoordToken, cfg.CoordTokenCommand = cfg.CoordAdminToken, nil
	}
	return nil
}

func lifetimePolicySummary(policy *LeaseLifetimePolicy) string {
	if policy == nil {
		return ""
	}
	stopAt := blank(policy.EffectiveStopAt, policy.DeadlineAt)
	if policy.EffectiveStopAt == "" && policy.Extension != nil {
		base, baseErr := time.Parse(time.RFC3339Nano, stopAt)
		extended, extErr := time.Parse(time.RFC3339Nano, policy.Extension.ExpiresAt)
		if baseErr == nil && extErr == nil && extended.After(base) {
			stopAt = policy.Extension.ExpiresAt
		}
	}
	summary := fmt.Sprintf(" lifetime_mode=%s lifetime_source=%s lifetime_status=%s lifetime_stop=%s lifetime_delete_after=%s lifetime_retention=%s",
		policy.Mode, blank(policy.Source, "default"), blank(policy.Status, "pending"), blank(stopAt, "-"), blank(policy.DeleteAfterAt, "-"), formatSecondsDuration(policy.RetainSeconds))
	if policy.Readiness != nil {
		summary += fmt.Sprintf(" lifetime_readiness=%s", policy.Readiness.Status)
	}
	for _, warning := range policy.Warnings {
		summary += fmt.Sprintf(" lifetime_warning=%q", warning)
	}
	if policy.Error != "" && !slices.Contains(policy.Warnings, policy.Error) {
		summary += fmt.Sprintf(" lifetime_error=%q", policy.Error)
	}
	return summary
}
