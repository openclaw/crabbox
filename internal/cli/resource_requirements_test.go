package cli

import (
	"fmt"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCapacityMinimumYAMLMergesRejectInvalidEffectiveScalars(t *testing.T) {
	for _, name := range []string{"minVCPUs", "minMemoryMiB"} {
		for _, value := range []string{"1.5", "1.0", "-1", "2147483648", "null", "'8'", "true", "[8]", "{value: 8}"} {
			for _, document := range []string{
				fmt.Sprintf("capacity: {<<: {%s: %s}}", name, value),
				fmt.Sprintf("base: &base {%s: %s}\ncapacity: {<<: *base}", name, value),
				fmt.Sprintf("base: &base {%s: %s}\nother: &other {<<: *base}\ncapacity: {<<: *other}", name, value),
				fmt.Sprintf("value: &value %s\ncapacity: {%s: *value}", value, name),
			} {
				var config fileConfig
				if err := yaml.Unmarshal([]byte(document), &config); err == nil {
					t.Errorf("accepted invalid effective minimum: %s", document)
				}
			}
		}
	}
}

func TestCapacityMinimumYAMLAliasesPreserveMergePrecedence(t *testing.T) {
	for _, document := range []string{
		"cpu: &cpu 8\nram: &ram 24576\ncapacity: {minVCPUs: *cpu, minMemoryMiB: *ram}",
		"base: &base {minVCPUs: 8, minMemoryMiB: 24576}\ncapacity: {<<: *base}",
		"base: &base {minVCPUs: 8, minMemoryMiB: 24576}\ncapacity: *base",
		"base: &base {minVCPUs: 1.5, minMemoryMiB: 1.5}\ncapacity: {<<: *base, minVCPUs: 8, minMemoryMiB: 24576}",
		"first: &first {minVCPUs: 8, minMemoryMiB: 24576}\nsecond: &second {minVCPUs: 1.5, minMemoryMiB: 1.5}\ncapacity: {<<: [*first, *second]}",
	} {
		var config fileConfig
		if err := yaml.Unmarshal([]byte(document), &config); err != nil {
			t.Fatalf("decode %s: %v", document, err)
		}
		if config.Capacity == nil || config.Capacity.MinVCPUs == nil || *config.Capacity.MinVCPUs != 8 || config.Capacity.MinMemoryMiB == nil || *config.Capacity.MinMemoryMiB != 24576 {
			t.Fatalf("wrong effective minimums for %s: %+v", document, config.Capacity)
		}
	}
}

func TestCapacityMinimumYAMLRetainsLegacyCapacityFields(t *testing.T) {
	var config fileConfig
	document := "base: &base {market: spot, strategy: most-available, fallback: on-demand, regions: [eu-west-1], availabilityZones: [eu-west-1a], hints: false}\ncapacity: {<<: *base, market: on-demand}"
	if err := yaml.Unmarshal([]byte(document), &config); err != nil {
		t.Fatal(err)
	}
	falseValue := false
	want := &fileCapacityConfig{
		Market: "on-demand", Strategy: "most-available", Fallback: "on-demand",
		Regions: []string{"eu-west-1"}, AvailabilityZones: []string{"eu-west-1a"}, Hints: &falseValue,
	}
	if !reflect.DeepEqual(config.Capacity, want) {
		t.Fatalf("capacity=%+v want=%+v", config.Capacity, want)
	}
}
