package cli

import (
	"io"
	"testing"
)

func TestCapacityMarketSelectionProvenance(t *testing.T) {
	for _, tc := range []struct {
		name, file, env, flag, want string
		explicit                    bool
	}{
		{name: "default", want: "spot"},
		{name: "file spot", file: "spot", want: "spot", explicit: true},
		{name: "file hourly", file: "on-demand", want: "on-demand", explicit: true},
		{name: "environment spot", env: "spot", want: "spot", explicit: true},
		{name: "environment overrides file", file: "spot", env: "on-demand", want: "on-demand", explicit: true},
		{name: "flag overrides environment", file: "on-demand", env: "on-demand", flag: "spot", want: "spot", explicit: true},
		{name: "flag hourly", flag: "on-demand", want: "on-demand", explicit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("CRABBOX_CAPACITY_MARKET", tc.env)
			cfg := baseConfig()
			if err := applyFileConfig(&cfg, fileConfig{Capacity: &fileCapacityConfig{Market: tc.file}}); err != nil {
				t.Fatal(err)
			}
			if err := applyEnv(&cfg); err != nil {
				t.Fatal(err)
			}
			fs := newFlagSet("market", io.Discard)
			market := fs.String("market", cfg.Capacity.Market, "")
			var args []string
			if tc.flag != "" {
				args = []string{"--market", tc.flag}
			}
			if err := parseFlags(fs, args); err != nil {
				t.Fatal(err)
			}
			if err := applyCapacityMarketFlag(&cfg, fs, *market); err != nil {
				t.Fatal(err)
			}
			if cfg.Capacity.Market != tc.want || CapacityMarketExplicit(cfg) != tc.explicit {
				t.Fatalf("market=%q explicit=%t, want %q/%t", cfg.Capacity.Market, CapacityMarketExplicit(cfg), tc.want, tc.explicit)
			}
		})
	}
}

func TestJobCapacityMarketPreservesExplicitSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		file fileJobConfig
		want string
	}{
		{name: "capacity", file: fileJobConfig{Capacity: &fileCapacityConfig{Market: "spot"}}, want: "spot"},
		{name: "job override", file: fileJobConfig{Capacity: &fileCapacityConfig{Market: "spot"}, Market: "on-demand"}, want: "on-demand"},
		{name: "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			job := applyFileJobConfig(JobConfig{}, tc.file)
			args := jobLeaseCreateArgsFor(cfg, job, false)
			fs := newFlagSet("job-market", io.Discard)
			market := fs.String("market", cfg.Capacity.Market, "")
			if err := parseFlags(fs, args); err != nil {
				t.Fatal(err)
			}
			if err := applyCapacityMarketFlag(&cfg, fs, *market); err != nil {
				t.Fatal(err)
			}
			if CapacityMarketExplicit(cfg) != (tc.want != "") || tc.want != "" && cfg.Capacity.Market != tc.want {
				t.Fatalf("job args=%q market=%q explicit=%t", args, cfg.Capacity.Market, CapacityMarketExplicit(cfg))
			}
		})
	}
}
