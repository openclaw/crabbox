package serve

import (
	"context"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestBackendConstructsWithoutCoordinatorButRejectsDirectUse(t *testing.T) {
	configured, err := (Provider{}).Configure(core.Config{}, core.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if spec := configured.Spec(); spec.Name != "serve" || !spec.BrokeredOnly {
		t.Fatalf("backend spec=%+v", spec)
	}
	b := configured.(core.SSHLeaseBackend)
	ctx := t.Context()
	for _, tc := range []struct {
		name string
		use  func(context.Context) error
	}{
		{"acquire", func(ctx context.Context) error {
			_, err := b.Acquire(ctx, core.AcquireRequest{})
			return err
		}},
		{"resolve", func(ctx context.Context) error {
			_, err := b.Resolve(ctx, core.ResolveRequest{})
			return err
		}},
		{"list", func(ctx context.Context) error {
			_, err := b.List(ctx, core.ListRequest{})
			return err
		}},
		{"release", func(ctx context.Context) error {
			return b.ReleaseLease(ctx, core.ReleaseLeaseRequest{})
		}},
		{"touch", func(ctx context.Context) error {
			_, err := b.Touch(ctx, core.TouchRequest{})
			return err
		}},
		{"doctor", func(ctx context.Context) error {
			_, err := b.(core.DoctorBackend).Doctor(ctx, core.DoctorRequest{})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.use(ctx)
			const want = "provider=serve requires a coordinator in brokered mode; configure broker.url and log in"
			if err == nil || err.Error() != want || core.ExitCodeForError(err, 1) != 2 {
				t.Fatalf("direct use error=%v, want exit 2: %s", err, want)
			}
		})
	}
}
