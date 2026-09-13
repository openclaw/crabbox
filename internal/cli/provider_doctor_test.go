package cli

import (
	"context"
	"errors"
	"io"
	"testing"
)

type doctorConfigurationProvider struct {
	Provider
	configure func(Config, Runtime) (Backend, error)
}

func (p doctorConfigurationProvider) Configure(cfg Config, rt Runtime) (Backend, error) {
	return p.configure(cfg, rt)
}

type doctorOverrideProvider struct {
	doctorConfigurationProvider
	doctor func(Config, Runtime) (DoctorBackend, error)
}

func (p doctorOverrideProvider) ConfigureDoctor(cfg Config, rt Runtime) (DoctorBackend, error) {
	return p.doctor(cfg, rt)
}

type configuredDoctor struct{ testSSHBackend }

func (*configuredDoctor) Doctor(context.Context, DoctorRequest) (DoctorResult, error) {
	return DoctorResult{}, nil
}

func TestConfigureProviderDoctorUsesSelectedBackend(t *testing.T) {
	want := &configuredDoctor{}
	calls := 0
	provider := doctorConfigurationProvider{configure: func(cfg Config, rt Runtime) (Backend, error) {
		calls++
		if cfg.Provider != "example" || rt.Stdout == nil {
			t.Fatal("selected configuration/runtime were not forwarded")
		}
		return want, nil
	}}
	got, err := ConfigureProviderDoctor(provider, Config{Provider: "example"}, Runtime{Stdout: io.Discard})
	if got != want || err != nil || calls != 1 {
		t.Fatalf("doctor=%T err=%v configure calls=%d", got, err, calls)
	}
}

func TestConfigureProviderDoctorOverrideDoesNotConfigureAcquisition(t *testing.T) {
	failure := errors.New("diagnostic configuration failed")
	for _, outcome := range []error{nil, failure} {
		want := &configuredDoctor{}
		calls := 0
		provider := doctorOverrideProvider{
			doctorConfigurationProvider: doctorConfigurationProvider{configure: func(Config, Runtime) (Backend, error) {
				t.Fatal("diagnostic override must not configure acquisition")
				return nil, nil
			}},
			doctor: func(Config, Runtime) (DoctorBackend, error) {
				calls++
				if outcome != nil {
					return nil, outcome
				}
				return want, nil
			},
		}
		got, err := ConfigureProviderDoctor(provider, Config{}, Runtime{})
		if err != outcome || calls != 1 || outcome == nil && got != want || outcome != nil && got != nil {
			t.Fatalf("doctor=%T err=%v override calls=%d", got, err, calls)
		}
	}
}

func TestConfigureProviderDoctorAbsentAndFailed(t *testing.T) {
	failure := errors.New("configure failed")
	for _, outcome := range []error{nil, failure} {
		provider := doctorConfigurationProvider{configure: func(Config, Runtime) (Backend, error) {
			return testSSHBackend{}, outcome
		}}
		got, err := ConfigureProviderDoctor(provider, Config{}, Runtime{})
		if got != nil || err != outcome {
			t.Fatalf("doctor=%T err=%v want nil/%v", got, err, outcome)
		}
	}
}
