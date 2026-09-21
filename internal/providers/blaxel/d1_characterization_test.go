package blaxel

import (
	"context"
	"errors"
	"reflect"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestD1NoWaitReadFailures(t *testing.T) {
	for _, mode := range []string{"raw error", "canceled fetch", "absent labels", "empty labels", "foreign labels"} {
		t.Run(mode, func(t *testing.T) {
			b, fake, sb := newStatusBackend(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			nativeErr := errors.New("D1 raw fetch failure")
			fake.getSandbox = func(context.Context, string) (Sandbox, error) {
				switch mode {
				case "raw error":
					return Sandbox{}, nativeErr
				case "canceled fetch":
					cancel()
					return Sandbox{}, nativeErr
				case "absent labels":
					sb.Labels = nil
				case "empty labels":
					sb.Labels = map[string]string{}
				case "foreign labels":
					sb.Labels[blaxelClaimKey] = "foreign"
				}
				return sb, nil
			}
			view, err := b.Status(ctx, core.StatusRequest{ID: "status-one"})
			if err == nil || !reflect.DeepEqual(view, core.StatusView{}) {
				t.Fatalf("view=%#v err=%v", view, err)
			}
			switch mode {
			case "raw error":
				if !errors.Is(err, nativeErr) {
					t.Fatal(err)
				}
			case "canceled fetch":
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			default:
				if core.ExitCodeForError(err, 1) != 4 {
					t.Fatal(err)
				}
			}
		})
	}
}
