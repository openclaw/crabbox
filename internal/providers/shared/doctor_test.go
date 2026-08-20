package shared

import (
	"context"
	"errors"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestConfigureDoctor(t *testing.T) {
	t.Run("returns doctor backend", func(t *testing.T) {
		want := &doctorBackendStub{}
		got, err := ConfigureDoctor("example", func() (core.Backend, error) { return want, nil })
		if err != nil || got != want {
			t.Fatalf("ConfigureDoctor() = (%T, %v), want (%T, nil)", got, err, want)
		}
	})

	t.Run("propagates configure error", func(t *testing.T) {
		want := errors.New("configure failed")
		got, err := ConfigureDoctor("example", func() (core.Backend, error) { return nil, want })
		if got != nil || !errors.Is(err, want) {
			t.Fatalf("ConfigureDoctor() = (%T, %v), want (nil, %v)", got, err, want)
		}
	})

	t.Run("rejects backend without doctor capability", func(t *testing.T) {
		got, err := ConfigureDoctor("example-provider", func() (core.Backend, error) { return backendStub{}, nil })
		var exitErr core.ExitError
		if got != nil || !errors.As(err, &exitErr) {
			t.Fatalf("ConfigureDoctor() = (%T, %v), want (nil, ExitError)", got, err)
		}
		if exitErr.Code != 2 || exitErr.Message != "example-provider doctor backend unavailable" {
			t.Fatalf("ConfigureDoctor() error = %#v", exitErr)
		}
	})
}

type backendStub struct{}

func (backendStub) Spec() core.ProviderSpec { return core.ProviderSpec{} }

type doctorBackendStub struct {
	backendStub
}

func (*doctorBackendStub) Doctor(context.Context, core.DoctorRequest) (core.DoctorResult, error) {
	return core.DoctorResult{}, nil
}
