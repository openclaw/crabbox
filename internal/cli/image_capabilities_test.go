package cli

import (
	"io"
	"reflect"
	"testing"
)

func TestLeaseImageCapabilityFlags(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.Coordinator = "https://crabbox.example.test"
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, cfg)
	if err := parseFlags(fs, []string{
		"--image-min-os", "15.4",
		"--image-sdk", "Xcode=16.3",
		"--image-runtime", "node=24.2",
		"--image-runtime", "go=1.25",
		"--image-require-browser",
		"--image-require-webview2",
		"--image-require-desktop",
	}); err != nil {
		t.Fatal(err)
	}
	if err := applyLeaseCreateFlags(&cfg, fs, values); err != nil {
		t.Fatal(err)
	}
	want := imageRequirements{
		MinOS:    "15.4",
		SDKs:     map[string]string{"xcode": "16.3"},
		Runtimes: map[string]string{"node": "24.2", "go": "1.25"},
		Browser:  true,
		WebView2: true,
		Desktop:  true,
	}
	if !reflect.DeepEqual(cfg.imageRequirements, want) {
		t.Fatalf("image requirements=%#v want %#v", cfg.imageRequirements, want)
	}
}

func TestImageCapabilityFlagsRejectMalformedVersions(t *testing.T) {
	cfg := baseConfig()
	fs := newFlagSet("test", io.Discard)
	values := registerLeaseCreateFlags(fs, cfg)
	if err := parseFlags(fs, []string{"--image-runtime", "node=current"}); err != nil {
		t.Fatal(err)
	}
	if err := applyLeaseCreateFlags(&cfg, fs, values); err == nil {
		t.Fatal("expected malformed image runtime version to fail")
	}
}

func TestLeaseImageCapabilityFlagsRejectDirectProviders(t *testing.T) {
	for name, configure := range map[string]func(*Config){
		"direct": func(*Config) {},
		"registered": func(cfg *Config) {
			cfg.Provider = "aws"
			cfg.Coordinator = "https://crabbox.example.test"
			cfg.BrokerMode = BrokerModeRegistered
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := baseConfig()
			configure(&cfg)
			fs := newFlagSet("test", io.Discard)
			values := registerLeaseCreateFlags(fs, cfg)
			if err := parseFlags(fs, []string{"--image-min-os", "26.04"}); err != nil {
				t.Fatal(err)
			}
			if err := applyLeaseCreateFlags(&cfg, fs, values); err == nil {
				t.Fatal("expected unmanaged image requirements to fail")
			}
		})
	}
}

func TestReadyPoolRejectsImageRequirements(t *testing.T) {
	err := validateReadyPoolImageRequirements(imageRequirements{Browser: true}, "browser-pool")
	if err == nil {
		t.Fatal("expected ready-pool image requirements to fail")
	}
	if err := validateReadyPoolImageRequirements(imageRequirements{}, "browser-pool"); err != nil {
		t.Fatalf("empty image requirements: %v", err)
	}
}
