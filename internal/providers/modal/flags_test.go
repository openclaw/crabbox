package modal

import (
	"flag"
	"reflect"
	"testing"
)

func TestModalSecretFlagsReplaceConfiguredDefaults(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "replace and repeat", args: []string{"--modal-secret", "example,sample", "--modal-secret", "dummy"}, want: []string{"example", "sample", "dummy"}},
		{name: "clear", args: []string{"--modal-secret="}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newTestConfig()
			cfg.Provider = providerName
			cfg.Modal.Secrets = []string{"sample"}
			fs := flag.NewFlagSet("modal", flag.ContinueOnError)
			values := RegisterModalProviderFlags(fs, cfg)
			if err := fs.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			if err := ApplyModalProviderFlags(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cfg.Modal.Secrets, tt.want) {
				t.Fatalf("Modal secrets=%v want %v", cfg.Modal.Secrets, tt.want)
			}
		})
	}
}
