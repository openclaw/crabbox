package shared

import core "github.com/openclaw/crabbox/internal/cli"

// ConfigureDoctor configures the provider backend and requires doctor capability.
func ConfigureDoctor(providerName string, configure func() (core.Backend, error)) (core.DoctorBackend, error) {
	backend, err := configure()
	if err != nil {
		return nil, err
	}
	doctor, ok := backend.(core.DoctorBackend)
	if !ok {
		return nil, core.Exit(2, "%s doctor backend unavailable", providerName)
	}
	return doctor, nil
}
