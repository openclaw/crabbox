package cli

// ConfigureProviderDoctor configures only the selected provider. An override
// may omit acquisition-only requirements; otherwise its ordinary backend owns
// diagnostics. A nil backend and nil error mean direct doctor is unsupported.
func ConfigureProviderDoctor(provider Provider, cfg Config, rt Runtime) (DoctorBackend, error) {
	if override, ok := provider.(DoctorProvider); ok {
		return override.ConfigureDoctor(cfg, rt)
	}
	backend, err := provider.Configure(cfg, rt)
	if err != nil {
		return nil, err
	}
	doctor, _ := backend.(DoctorBackend)
	return doctor, nil
}
