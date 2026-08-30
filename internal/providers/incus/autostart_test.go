package incus

import (
	"context"
	"testing"
)

func TestIncusOrdinaryLeasePreservesAutostartPolicy(t *testing.T) {
	for _, instanceType := range []string{"container", "vm"} {
		for _, policy := range []string{"", "true"} {
			t.Run(instanceType+"/autostart="+policy, func(t *testing.T) {
				backend, fake, req := lifecycleFixture(t)
				backend.cfg.Incus.InstanceType = instanceType
				backend.cfg.ServerType = ""
				if policy != "" {
					fake.profileConfig = map[string]string{"boot.autostart": policy}
				}
				if _, err := backend.Acquire(context.Background(), req); err != nil {
					t.Fatal(err)
				}
				if override, exists := fake.created[0].Config["boot.autostart"]; exists {
					t.Fatalf("ordinary lease replaced inherited autostart with %q", override)
				}
			})
		}
	}
}
