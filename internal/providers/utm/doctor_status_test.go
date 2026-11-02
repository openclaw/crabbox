package utm

import (
	"context"
	"os"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestUTMDoctorFailuresUseCoreFailureStatus(t *testing.T) {
	for _, scenario := range []string{"template-missing", "template-running", "application-control", "storage", "ipsw-missing", "native-preflight"} {
		t.Run(scenario, func(t *testing.T) {
			var b *backend
			var check string
			if scenario == "ipsw-missing" || scenario == "native-preflight" {
				b, _ = setupMacOS(t)
				if scenario == "ipsw-missing" {
					if err := os.Remove(b.cfg.UTM.IPSW); err != nil {
						t.Fatal(err)
					}
					check = "ipsw-input"
				} else {
					inspectMacOSImage = func(*backend, context.Context, string) (macOSImage, error) {
						return macOSImage{}, context.DeadlineExceeded
					}
					check = "native-image-account"
				}
			} else {
				var f *controlFixture
				b, f = setup(t)
				switch scenario {
				case "template-missing":
					b.cfg.UTM.Template = "55555555-5555-4555-8555-555555555555"
					check = "template"
				case "template-running":
					f.vms[0].Status = "started"
					check = "template"
				case "application-control":
					f.failInventory = true
					check = "application-control"
				case "storage":
					b.cfg.UTM.StorageRoot = b.cfg.UTM.StorageRoot + "/absent"
					check = "storage"
				}
			}
			result, err := b.Doctor(t.Context(), core.DoctorRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "failed" || core.DoctorChecksStatus(result.Checks) != "failed" {
				t.Errorf("provider failure looks successful to core: status=%q aggregate=%q checks=%+v", result.Status, core.DoctorChecksStatus(result.Checks), result.Checks)
			}
			found := false
			for _, c := range result.Checks {
				if c.Check == check {
					found = true
					if c.Status != "failed" || c.Message == "" {
						t.Errorf("failed check lost canonical status or diagnostic: %+v", c)
					}
				}
			}
			if !found {
				t.Fatalf("missing failure check %s", check)
			}
			if records, err := b.records(); err != nil || len(records) != 0 {
				t.Fatal("doctor changed ownership state")
			}
		})
	}
}
