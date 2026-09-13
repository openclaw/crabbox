package cli

import "testing"

func TestConfigEnvironmentSplitIgnoresRuntimeFields(t *testing.T) {
	type config struct {
		State map[string]string `sources:"runtime"`
		First string            `sources:"env" env:"CRABBOX_TEST_CONFIG_FIRST" reportApplied:"true"`
		Other *int              `sources:"runtime"`
		Last  string            `sources:"env" env:"CRABBOX_TEST_CONFIG_LAST" reportApplied:"true"`
	}
	type report struct{ InputAccepted, First, Last bool }
	t.Setenv("CRABBOX_TEST_CONFIG_FIRST", "first")
	t.Setenv("CRABBOX_TEST_CONFIG_LAST", "last")
	value := 7
	cfg := config{State: map[string]string{"runtime": "retained"}, Other: &value}
	var prefix, suffix report
	if err := applyConfigEnvironment(&cfg, &prefix, 0, 1); err != nil {
		t.Fatal(err)
	}
	if cfg.First != "first" || cfg.Last != "" || prefix != (report{InputAccepted: true, First: true}) {
		t.Fatalf("prefix crossed a schema boundary: %+v %+v", cfg, prefix)
	}
	if err := applyConfigEnvironment(&cfg, &suffix, 1, 2); err != nil {
		t.Fatal(err)
	}
	if cfg.Last != "last" || suffix != (report{InputAccepted: true, Last: true}) {
		t.Fatalf("suffix missed a schema field: %+v %+v", cfg, suffix)
	}
	if cfg.State["runtime"] != "retained" || cfg.Other != &value {
		t.Fatal("environment application changed runtime state")
	}
}
