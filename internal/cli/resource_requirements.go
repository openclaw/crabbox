package cli

import "gopkg.in/yaml.v3"

func hasCapacityMinimums(cfg Config) bool {
	return cfg.Capacity.MinVCPUs > 0 || cfg.Capacity.MinMemoryMiB > 0
}

func validateCapacityMinimumValues(capacity CapacityConfig) error {
	for _, field := range []struct {
		name  string
		value int
	}{
		{"capacity.minVCPUs", capacity.MinVCPUs},
		{"capacity.minMemoryMiB", capacity.MinMemoryMiB},
	} {
		if field.value < 0 || int64(field.value) > 2147483647 {
			return Exit(2, "%s must be an integer from 0 through 2147483647", field.name)
		}
	}
	return nil
}

func validateResourceRequirements(cfg Config) error {
	if err := validateCapacityMinimumValues(cfg.Capacity); err != nil {
		return err
	}
	if !hasCapacityMinimums(cfg) {
		return nil
	}
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return err
	}
	capable, ok := provider.(ProviderResourceRequirementsCapability)
	if !ok || !capable.SupportsResourceRequirements(cfg) {
		return Exit(2, "resource requirements are unsupported for provider=%s target=%s architecture=%s", cfg.Provider, cfg.TargetOS, cfg.Architecture)
	}
	return nil
}

// yaml.v3 otherwise converts fractional numeric scalars into Go integers.
func (capacity *fileCapacityConfig) UnmarshalYAML(node *yaml.Node) error {
	// Let yaml.v3 select effective values, including merge precedence, while
	// retaining scalar tags that decoding directly into int would discard.
	var minimums struct {
		MinVCPUs     yaml.Node `yaml:"minVCPUs"`
		MinMemoryMiB yaml.Node `yaml:"minMemoryMiB"`
	}
	if err := node.Decode(&minimums); err != nil {
		return err
	}
	for _, field := range []struct {
		name string
		node *yaml.Node
	}{
		{"minVCPUs", &minimums.MinVCPUs},
		{"minMemoryMiB", &minimums.MinMemoryMiB},
	} {
		// ShortTag follows scalar aliases; a zero node means the field is absent.
		if field.node.Kind != 0 && field.node.ShortTag() != "!!int" {
			return Exit(2, "capacity.%s must be an integer from 0 through 2147483647", field.name)
		}
	}
	type plainCapacity fileCapacityConfig
	var decoded plainCapacity
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*capacity = fileCapacityConfig(decoded)
	values := CapacityConfig{}
	if capacity.MinVCPUs != nil {
		values.MinVCPUs = *capacity.MinVCPUs
	}
	if capacity.MinMemoryMiB != nil {
		values.MinMemoryMiB = *capacity.MinMemoryMiB
	}
	return validateCapacityMinimumValues(values)
}
