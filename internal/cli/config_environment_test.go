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

func TestConfigEnvironmentThirdAliasOrdering(t *testing.T) {
	type config struct {
		Value string `sources:"env" env:"CRABBOX_TEST_ALIAS_PRIMARY" envAlias:"CRABBOX_TEST_ALIAS_FIRST" envAlias2:"CRABBOX_TEST_ALIAS_SECOND" envAlias3:"CRABBOX_TEST_ALIAS_THIRD" reportApplied:"true"`
	}
	type report struct{ InputAccepted, Value bool }
	names := []string{"CRABBOX_TEST_ALIAS_PRIMARY", "CRABBOX_TEST_ALIAS_FIRST", "CRABBOX_TEST_ALIAS_SECOND", "CRABBOX_TEST_ALIAS_THIRD"}
	for winner := -1; winner < len(names); winner++ {
		for _, raw := range []string{"fixture-value", "  "} {
			for i, name := range names {
				value := ""
				if winner >= 0 && i >= winner {
					value = "fixture-later"
				}
				if i == winner {
					value = raw
				}
				t.Setenv(name, value)
			}
			cfg := config{Value: "prior"}
			var got report
			if err := applyConfigEnvironment(&cfg, &got, 0, 1); err != nil {
				t.Fatal(err)
			}
			want := "prior"
			if winner >= 0 {
				want = raw
			}
			if cfg.Value != want || got != (report{InputAccepted: winner >= 0, Value: winner >= 0}) {
				t.Fatalf("winner=%d raw=%q cfg=%+v report=%+v", winner, raw, cfg, got)
			}
		}
	}
}
