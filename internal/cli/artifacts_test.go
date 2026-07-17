package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseArtifactPublishOptionsNormalizesStorage(t *testing.T) {
	opts, err := parseArtifactPublishOptions([]string{
		"--dir", "bundle",
		"--storage", "r2",
		"--bucket", "qa",
		"--base-url", "https://artifacts.example.com/root/",
		"--endpoint-url", "https://account.r2.cloudflarestorage.com",
		"--prefix", "/runs/123/",
		"--pr", "42",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Storage != "r2" {
		t.Fatalf("storage=%q", opts.Storage)
	}
	if opts.BaseURL != "https://artifacts.example.com/root" {
		t.Fatalf("baseURL=%q", opts.BaseURL)
	}
	if opts.Prefix != "runs/123" {
		t.Fatalf("prefix=%q", opts.Prefix)
	}
	if opts.PR != 42 {
		t.Fatalf("pr=%d", opts.PR)
	}
}

func TestParseArtifactPublishOptionsRequiresExplicitDir(t *testing.T) {
	t.Setenv("CRABBOX_ARTIFACTS_DIR", "")
	_, err := parseArtifactPublishOptions([]string{
		"--storage", "local",
	}, io.Discard)
	if err == nil {
		t.Fatal("expected missing dir error")
	}
	if !strings.Contains(err.Error(), "requires --dir") {
		t.Fatalf("err=%v", err)
	}
}

func TestParseArtifactPublishOptionsAllowsDirFromEnv(t *testing.T) {
	t.Setenv("CRABBOX_ARTIFACTS_DIR", "bundle")
	opts, err := parseArtifactPublishOptions([]string{
		"--storage", "local",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Directory != "bundle" {
		t.Fatalf("dir=%q", opts.Directory)
	}
}

func TestParseArtifactPublishOptionsSupportsSkipManifest(t *testing.T) {
	opts, err := parseArtifactPublishOptions([]string{
		"--dir", "bundle",
		"--storage", "local",
		"--skip-manifest",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.NoManifest {
		t.Fatal("skip-manifest should disable manifest creation")
	}

	opts, err = parseArtifactPublishOptions([]string{
		"--dir", "bundle",
		"--storage", "local",
		"--no-manifest",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !opts.NoManifest {
		t.Fatal("no-manifest should disable manifest creation")
	}
}

func TestDefaultArtifactPublishPrefixIsUniqueAndScoped(t *testing.T) {
	when := time.Date(2026, 5, 8, 3, 40, 41, 123456789, time.UTC)
	got := defaultArtifactPublishPrefix(artifactPublishOptions{
		Directory: "/tmp/artifacts/Blue Lobster",
		PR:        42,
	}, when)
	if got != "pr-42/blue-lobster/20260508-034041-123456789" {
		t.Fatalf("prefix=%q", got)
	}
}

func TestEnsureArtifactPublishPrefixOnlyForHostedStorage(t *testing.T) {
	hosted := artifactPublishOptions{Storage: "s3", Directory: "/tmp/artifacts/Blue Lobster", PR: 42}
	ensureArtifactPublishPrefix(&hosted)
	if !strings.HasPrefix(hosted.Prefix, "pr-42/blue-lobster/") {
		t.Fatalf("hosted prefix=%q", hosted.Prefix)
	}

	local := artifactPublishOptions{Storage: "local", Directory: "/tmp/artifacts/Blue Lobster", PR: 42}
	ensureArtifactPublishPrefix(&local)
	if local.Prefix != "" {
		t.Fatalf("local prefix=%q", local.Prefix)
	}
}

func TestParseArtifactPublishOptionsR2UsesR2Defaults(t *testing.T) {
	t.Setenv("AWS_PROFILE", "default")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("CRABBOX_ARTIFACTS_R2_AWS_PROFILE", "qa-r2")
	t.Setenv("CRABBOX_ARTIFACTS_R2_ENDPOINT_URL", "https://account.r2.cloudflarestorage.com")

	opts, err := parseArtifactPublishOptions([]string{
		"--dir", "bundle",
		"--storage", "r2",
		"--bucket", "qa",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Profile != "qa-r2" {
		t.Fatalf("profile=%q", opts.Profile)
	}
	if opts.Region != "auto" {
		t.Fatalf("region=%q", opts.Region)
	}
	if opts.EndpointURL != "https://account.r2.cloudflarestorage.com" {
		t.Fatalf("endpointURL=%q", opts.EndpointURL)
	}
}

func TestParseArtifactPublishOptionsRequiresCloudflareBaseURLForPR(t *testing.T) {
	_, err := parseArtifactPublishOptions([]string{
		"--dir", "bundle",
		"--storage", "cloudflare",
		"--bucket", "qa",
		"--pr", "42",
	}, io.Discard)
	if err == nil {
		t.Fatal("expected base-url validation error")
	}
	if !strings.Contains(err.Error(), "requires --base-url") {
		t.Fatalf("err=%v", err)
	}
}

func TestArtifactStorageURLs(t *testing.T) {
	s3 := artifactS3URL(artifactPublishOptions{Bucket: "qa", Region: "eu-west-1"}, "runs/1/screen shot.png")
	if s3 != "https://qa.s3.eu-west-1.amazonaws.com/runs/1/screen%20shot.png" {
		t.Fatalf("s3 url=%s", s3)
	}
	custom := artifactS3URL(artifactPublishOptions{Bucket: "qa", EndpointURL: "https://s3.example.com/root"}, "runs/1/screen shot.png")
	if custom != "https://s3.example.com/root/qa/runs/1/screen%20shot.png" {
		t.Fatalf("custom s3 url=%s", custom)
	}
	r2 := artifactCloudflareURL(artifactPublishOptions{Bucket: "qa", BaseURL: "https://assets.example.com/base"}, "runs/1/after.gif")
	if r2 != "https://assets.example.com/base/runs/1/after.gif" {
		t.Fatalf("r2 url=%s", r2)
	}
}

func TestArtifactCollectNeedsDesktop(t *testing.T) {
	if artifactCollectNeedsDesktop(false, false, false, false) {
		t.Fatal("log-only artifact collection should not require desktop")
	}
	for name, args := range map[string][4]bool{
		"screenshot":    {true, false, false, false},
		"video":         {false, true, false, false},
		"doctor":        {false, false, true, false},
		"webvnc status": {false, false, false, true},
	} {
		if !artifactCollectNeedsDesktop(args[0], args[1], args[2], args[3]) {
			t.Fatalf("%s artifact collection should require desktop", name)
		}
	}
}

func TestWriteArtifactRunLogsUsesRoutedCoordinatorAndScrubsDesktopCredentials(t *testing.T) {
	t.Setenv("CRABBOX_ARTIFACT_RUN_TOKEN_HELPER", "1")
	t.Setenv("ARTIFACT_CURRENT_DESKTOP_PASSWORD", "current-secret")
	t.Setenv("ARTIFACT_ROUTED_DESKTOP_PASSWORD", "routed-secret")
	t.Setenv("CRABBOX_TEST_KEEP", "preserved")

	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if got := r.Header.Get("Authorization"); got != "Bearer artifact-token" {
			t.Errorf("Authorization=%q", got)
		}
		switch r.URL.Path {
		case "/v1/runs/run_routed/logs":
			_, _ = io.WriteString(w, "routed logs")
		case "/v1/runs/run_routed":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"run":{"id":"run_routed","state":"passed"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := Config{
		Coordinator:       server.URL,
		CoordTokenCommand: []string{os.Args[0], "-test.run=^TestArtifactRunLogCoordinatorTokenHelper$"},
		Provider:          "external",
		TargetOS:          targetMacOS,
	}
	cfg.External.Connection.Desktop.PasswordEnv = "ARTIFACT_ROUTED_DESKTOP_PASSWORD"
	// A base-config reload would ignore the routed coordinator above.
	t.Setenv("CRABBOX_COORDINATOR", "http://127.0.0.1:1")

	dir := t.TempDir()
	logPath, runPath, err := writeArtifactRunLogs(
		context.Background(),
		cfg,
		[]string{"ARTIFACT_CURRENT_DESKTOP_PASSWORD"},
		"run_routed",
		dir,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&requests); got != 2 {
		t.Fatalf("requests=%d", got)
	}
	if data, err := os.ReadFile(logPath); err != nil || string(data) != "routed logs" {
		t.Fatalf("logs=%q err=%v", data, err)
	}
	var run CoordinatorRun
	if data, err := os.ReadFile(runPath); err != nil {
		t.Fatal(err)
	} else if err := json.Unmarshal(data, &run); err != nil || run.ID != "run_routed" {
		t.Fatalf("run=%#v err=%v", run, err)
	}
}

func TestArtifactRunLogCoordinatorTokenHelper(t *testing.T) {
	if os.Getenv("CRABBOX_ARTIFACT_RUN_TOKEN_HELPER") != "1" {
		return
	}
	for _, name := range []string{"ARTIFACT_CURRENT_DESKTOP_PASSWORD", "ARTIFACT_ROUTED_DESKTOP_PASSWORD"} {
		if _, ok := os.LookupEnv(name); ok {
			os.Exit(89)
		}
	}
	if os.Getenv("CRABBOX_TEST_KEEP") != "preserved" {
		os.Exit(90)
	}
	_, _ = fmt.Fprintln(os.Stdout, "artifact-token")
	os.Exit(0)
}

func TestArtifactCloudflareEnvUsesGenericCredentials(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", "generic-token")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "generic-account")

	env := strings.Join(artifactCloudflareEnv(), "\n")
	for _, want := range []string{
		"CLOUDFLARE_API_TOKEN=generic-token",
		"CLOUDFLARE_ACCOUNT_ID=generic-account",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("env missing %q in %s", want, env)
		}
	}
}

func TestArtifactCloudflareEnvArtifactCredentialsWin(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", "generic-token")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "generic-account")
	t.Setenv("CRABBOX_ARTIFACTS_CLOUDFLARE_API_TOKEN", "artifact-token")
	t.Setenv("CRABBOX_ARTIFACTS_CLOUDFLARE_ACCOUNT_ID", "artifact-account")

	env := strings.Join(artifactCloudflareEnv(), "\n")
	for _, want := range []string{
		"CLOUDFLARE_API_TOKEN=artifact-token",
		"CLOUDFLARE_ACCOUNT_ID=artifact-account",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("env missing %q in %s", want, env)
		}
	}
}

func TestArtifactPublisherCommandsScrubTargetEnvironmentAndPreserveToolAuth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell publisher fixtures")
	}
	for _, tc := range []struct {
		name      string
		command   string
		authSetup func(*testing.T)
		authCheck string
		publish   func(*testing.T, context.Context, artifactPublishOptions, string) error
	}{
		{
			name:    "aws",
			command: "aws",
			authSetup: func(t *testing.T) {
				t.Setenv("AWS_ACCESS_KEY_ID", "publisher-access")
				t.Setenv("AWS_SECRET_ACCESS_KEY", "publisher-secret")
			},
			authCheck: "[ \"$AWS_ACCESS_KEY_ID\" = publisher-access ] && [ \"$AWS_SECRET_ACCESS_KEY\" = publisher-secret ] || exit 91\n",
			publish: func(t *testing.T, ctx context.Context, opts artifactPublishOptions, dir string) error {
				opts.Bucket = "qa"
				file, cleanup := artifactPublisherTestFile(t, dir)
				defer cleanup()
				_, err := uploadArtifactS3(ctx, opts, file, "proof.png", "image/png")
				return err
			},
		},
		{
			name:    "wrangler",
			command: "wrangler",
			authSetup: func(t *testing.T) {
				t.Setenv("CRABBOX_ARTIFACTS_CLOUDFLARE_API_TOKEN", "")
				t.Setenv("CRABBOX_ARTIFACTS_CLOUDFLARE_ACCOUNT_ID", "")
				t.Setenv("CLOUDFLARE_API_TOKEN", "publisher-token")
				t.Setenv("CLOUDFLARE_ACCOUNT_ID", "publisher-account")
			},
			authCheck: "[ \"$CLOUDFLARE_API_TOKEN\" = publisher-token ] && [ \"$CLOUDFLARE_ACCOUNT_ID\" = publisher-account ] || exit 92\n",
			publish: func(t *testing.T, ctx context.Context, opts artifactPublishOptions, dir string) error {
				opts.Bucket = "qa"
				opts.BaseURL = "https://assets.example.test"
				file, cleanup := artifactPublisherTestFile(t, dir)
				defer cleanup()
				_, err := uploadArtifactCloudflare(ctx, opts, file, "proof.png", "image/png")
				return err
			},
		},
		{
			name:    "gh",
			command: "gh",
			authSetup: func(t *testing.T) {
				t.Setenv("GH_TOKEN", "publisher-token")
				t.Setenv("GH_CONFIG_DIR", t.TempDir())
			},
			authCheck: "[ \"$GH_TOKEN\" = publisher-token ] && [ -n \"$GH_CONFIG_DIR\" ] || exit 93\n",
			publish: func(_ *testing.T, ctx context.Context, opts artifactPublishOptions, _ string) error {
				opts.PR = 42
				opts.Repo = "example-org/my-app"
				return postGitHubPRComment(ctx, opts, []byte("proof"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := "#!/bin/sh\n" +
				"if [ \"${TEST_ARD_PASSWORD+x}\" = x ]; then exit 89; fi\n" +
				"[ \"$CRABBOX_TEST_KEEP\" = preserved ] || exit 90\n" +
				tc.authCheck +
				"printf ok\n"
			if err := os.WriteFile(filepath.Join(dir, tc.command), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TEST_ARD_PASSWORD", "must-not-reach-publisher")
			t.Setenv("CRABBOX_TEST_KEEP", "preserved")
			tc.authSetup(t)
			opts := artifactPublishOptions{ChildEnvDenylist: []string{"test_ard_password"}}
			if err := tc.publish(t, context.Background(), opts, dir); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func artifactPublisherTestFile(t *testing.T, dir string) (artifactFile, func()) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	files, cleanup, err := snapshotArtifactData(root, artifactFile{
		Kind: artifactKindForPath("proof.png"),
		Name: "proof.png",
		Path: filepath.Join(dir, "proof.png"),
	}, []byte("proof"))
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	return files[0], func() {
		cleanup()
		_ = root.Close()
	}
}

func TestArtifactsPublishScrubsConfiguredExternalDesktopPasswordEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell publisher fixtures")
	}
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	config := `provider: external
target: macos
external:
  connection:
    desktop:
      username: screen-user
      passwordEnv: TEST_ARTIFACTS_DESKTOP_PASSWORD
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "publisher.log")
	script := "#!/bin/sh\n" +
		"if [ \"${TEST_ARTIFACTS_DESKTOP_PASSWORD+x}\" = x ]; then exit 89; fi\n" +
		"[ \"$CRABBOX_TEST_KEEP\" = preserved ] || exit 90\n" +
		"printf '%s\\n' \"${0##*/}\" >> \"$CRABBOX_TEST_ARTIFACT_PUBLISH_LOG\"\n"
	for _, name := range []string{"aws", "gh"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bundleDir := filepath.Join(root, "bundle")
	if err := os.Mkdir(bundleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(bundleDir, "proof.png"), "png")

	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("CRABBOX_EXTERNAL_DESKTOP_USERNAME", "")
	t.Setenv("CRABBOX_EXTERNAL_DESKTOP_PASSWORD_ENV", "")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_ARTIFACTS_DESKTOP_PASSWORD", "must-not-reach-publisher")
	t.Setenv("CRABBOX_TEST_KEEP", "preserved")
	t.Setenv("CRABBOX_TEST_ARTIFACT_PUBLISH_LOG", logPath)

	err := (App{Stdout: io.Discard, Stderr: io.Discard}).artifactsPublish(context.Background(), []string{
		"--dir", bundleDir,
		"--storage", "s3",
		"--bucket", "qa",
		"--skip-manifest",
		"--pr", "42",
		"--repo", "example-org/my-app",
	})
	if err != nil {
		t.Fatal(err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(logData)
	for _, name := range []string{"aws\n", "gh\n"} {
		if !strings.Contains(logText, name) {
			t.Fatalf("publisher log missing %q: %q", name, logText)
		}
	}
}

func TestArtifactsPublishRejectsInvalidExternalDesktopPasswordEnvironmentBeforePublisher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell publisher fixture")
	}
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	config := `provider: external
target: macos
external:
  command: provider-adapter
  connection:
    desktop:
      username: screen-user
      passwordEnv: PATH
`
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	invokedPath := filepath.Join(root, "publisher-invoked")
	script := "#!/bin/sh\nprintf invoked > \"$CRABBOX_TEST_ARTIFACT_PUBLISH_INVOKED\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "aws"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	bundleDir := filepath.Join(root, "bundle")
	if err := os.Mkdir(bundleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(bundleDir, "proof.png"), "png")

	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("CRABBOX_EXTERNAL_DESKTOP_USERNAME", "")
	t.Setenv("CRABBOX_EXTERNAL_DESKTOP_PASSWORD_ENV", "")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_TEST_ARTIFACT_PUBLISH_INVOKED", invokedPath)

	err := (App{Stdout: io.Discard, Stderr: io.Discard}).artifactsPublish(context.Background(), []string{
		"--dir", bundleDir,
		"--storage", "s3",
		"--bucket", "qa",
		"--skip-manifest",
		"--no-comment",
	})
	if err == nil || !strings.Contains(err.Error(), "passwordEnv PATH is reserved") {
		t.Fatalf("error=%v", err)
	}
	if _, statErr := os.Stat(invokedPath); !os.IsNotExist(statErr) {
		t.Fatalf("publisher invoked before config rejection: %v", statErr)
	}
}

func TestArtifactTemplateMarkdownUsesInlineImages(t *testing.T) {
	body := artifactTemplateMarkdown("mantis", "fixed login", "before.png", "https://cdn.example.com/after.gif", []artifactFile{
		{Kind: "logs", Name: "logs.txt", URL: "https://cdn.example.com/logs.txt"},
		{Kind: "screenshot", Name: "screenshot.png", URL: "https://s3.example.com/screenshot.png?X-Amz-Signature=abc"},
	})
	for _, want := range []string{
		"## Mantis QA Artifacts",
		"fixed login",
		"![before](before.png)",
		"![after](https://cdn.example.com/after.gif)",
		"[logs.txt](https://cdn.example.com/logs.txt)",
		"![screenshot.png](https://s3.example.com/screenshot.png?X-Amz-Signature=abc)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("markdown missing %q:\n%s", want, body)
		}
	}
}

func TestArtifactMarkdownForAssetIgnoresQueryString(t *testing.T) {
	got := artifactMarkdownForAsset("after", "https://cdn.example.com/after.gif?token=secret")
	if got != "![after](https://cdn.example.com/after.gif?token=secret)" {
		t.Fatalf("markdown=%s", got)
	}
}

func TestArtifactKindClassifiesBeforeAfterVideosAsVideo(t *testing.T) {
	for _, name := range []string{"before.mp4", "after.mov", "nested/after.webm"} {
		t.Run(name, func(t *testing.T) {
			if got := artifactKindForPath(name); got != "video" {
				t.Fatalf("kind=%q", got)
			}
			markdown := artifactMarkdownForFile(artifactFile{Kind: artifactKindForPath(name), Name: filepath.Base(name)}, "https://cdn.example.com/"+filepath.Base(name))
			if strings.HasPrefix(markdown, "![") {
				t.Fatalf("video markdown should be a link, got %s", markdown)
			}
			if !strings.HasPrefix(markdown, "[") {
				t.Fatalf("video markdown should be a link, got %s", markdown)
			}
		})
	}
}

func TestArtifactKindClassifiesProofSidecars(t *testing.T) {
	tests := map[string]string{
		"screen.contact.png":     "contact-sheet",
		"screenshot.contact.png": "contact-sheet",
		"diagnostics.txt":        "diagnostics",
	}
	for name, want := range tests {
		if got := artifactKindForPath(name); got != want {
			t.Fatalf("kind for %s=%q want %q", name, got, want)
		}
		markdown := artifactMarkdownForFile(artifactFile{Kind: artifactKindForPath(name), Name: filepath.Base(name)}, "https://cdn.example.com/"+filepath.Base(name))
		if strings.HasSuffix(name, ".png") && !strings.HasPrefix(markdown, "![") {
			t.Fatalf("contact sheet should render inline, got %s", markdown)
		}
	}
}

func TestWindowsDesktopVideoRemoteCommandCapturesInteractiveFrames(t *testing.T) {
	frames, intervalMS := windowsDesktopVideoFrameTiming(2*time.Second, 4)
	if frames != 8 || intervalMS != 250 {
		t.Fatalf("frames=%d intervalMS=%d", frames, intervalMS)
	}
	script := windowsDesktopVideoCaptureScript(`C:\ProgramData\crabbox\cv-1-frames`, `C:\ProgramData\crabbox\cv-1.zip`, frames, intervalMS)
	for _, want := range []string{
		`$Frames = 8`,
		`$IntervalMS = 250`,
		"CopyFromScreen",
		"frame-{0:D6}.jpg",
		"Compress-Archive",
		`$Zip + ".done"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("windows video script missing %q:\n%s", want, script)
		}
	}

	got := windowsDesktopVideoRemoteCommand(`C:\ProgramData\crabbox\cv-1.ps1`, `C:\ProgramData\crabbox\cv-1-frames`, `C:\ProgramData\crabbox\cv-1.zip`, 2*time.Second)
	for _, want := range []string{
		"CrabboxVideo-",
		`$outDir = 'C:\ProgramData\crabbox\cv-1-frames'`,
		`$zip = 'C:\ProgramData\crabbox\cv-1.zip'`,
		`$done = $zip + ".done"`,
		`$script = 'C:\ProgramData\crabbox\cv-1.ps1'`,
		"-File $script",
		"[Console]::OpenStandardOutput().Write",
		"scheduled interactive video did not produce output",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("windows video command missing %q:\n%s", want, got)
		}
	}
}

func TestWindowsDesktopVideoEncoderScrubsTargetChildEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell ffmpeg fixture")
	}
	dir := t.TempDir()
	ffmpeg := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\nif [ \"${TEST_ARD_PASSWORD+x}\" = x ]; then echo leaked >&2; exit 89; fi\nif [ \"$CRABBOX_TEST_KEEP\" != preserved ]; then echo missing >&2; exit 90; fi\nprintf encoded\n"
	if err := os.WriteFile(ffmpeg, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_ARD_PASSWORD", "must-not-reach-ffmpeg")
	t.Setenv("CRABBOX_TEST_KEEP", "preserved")
	target := SSHTarget{ChildEnvDenylist: []string{"TEST_ARD_PASSWORD"}}
	out, err := windowsDesktopVideoEncoderCommand(context.Background(), target, "-version").CombinedOutput()
	if err != nil {
		t.Fatalf("fake ffmpeg: %v: %s", err, out)
	}
	if string(out) != "encoded" {
		t.Fatalf("fake ffmpeg output=%q", out)
	}
}

func TestListArtifactBundleFilesSkipsPublishedMarkdown(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "screenshot.png"), "png")
	mustWriteFile(t, filepath.Join(dir, "published-artifacts.md"), "old")
	mustWriteFile(t, filepath.Join(dir, artifactManifestFilename), "{}")
	mustWriteFile(t, filepath.Join(dir, "nested", "logs.txt"), "logs")
	mustWriteFile(t, filepath.Join(dir, "nested", "published-artifacts.md", "child.txt"), "child")
	files, err := listArtifactBundleFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range files {
		names = append(names, file.Name)
	}
	got := strings.Join(names, ",")
	if got != "nested/logs.txt,nested/published-artifacts.md/child.txt,screenshot.png" {
		t.Fatalf("files=%s", got)
	}
}

func TestArtifactsPublishRejectsSymlinksBeforeSideEffects(t *testing.T) {
	for _, linkName := range []string{"screenshot.png", artifactManifestFilename, "published-artifacts.md"} {
		t.Run(linkName, func(t *testing.T) {
			dir := t.TempDir()
			outside := filepath.Join(t.TempDir(), "outside-secret.txt")
			mustWriteFile(t, outside, "outside-secret")
			mustWriteFile(t, filepath.Join(dir, "safe.txt"), "safe")
			if err := os.Symlink(outside, filepath.Join(dir, linkName)); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}

			var stdout bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: io.Discard}).artifactsPublish(context.Background(), []string{
				"--dir", dir,
				"--storage", "local",
			})
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
				t.Fatalf("error=%v, want exit 2", err)
			}
			if !strings.Contains(exitErr.Message, "symlink") || !strings.Contains(exitErr.Message, linkName) {
				t.Fatalf("message=%q", exitErr.Message)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout=%q, want no publish output", stdout.String())
			}
			if got, readErr := os.ReadFile(outside); readErr != nil || string(got) != "outside-secret" {
				t.Fatalf("outside file changed: data=%q err=%v", got, readErr)
			}
		})
	}
}

func TestSnapshotArtifactFilesRejectsSymlinkSwapAfterValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "screenshot.png")
	mustWriteFile(t, path, "safe-bytes")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := listArtifactBundleFilesRoot(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-secret.txt")
	mustWriteFile(t, outside, "outside-secret")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, cleanup, err := snapshotArtifactFiles(root, files)
	defer cleanup()
	if err == nil || !strings.Contains(err.Error(), "screenshot.png") {
		t.Fatalf("error=%v, want changed artifact rejection", err)
	}
	if got, readErr := os.ReadFile(outside); readErr != nil || string(got) != "outside-secret" {
		t.Fatalf("outside file changed: data=%q err=%v", got, readErr)
	}
}

func TestValidateArtifactBundleRootRejectsParentSwap(t *testing.T) {
	base := t.TempDir()
	bundle := filepath.Join(base, "bundle")
	replacement := filepath.Join(base, "replacement")
	mustWriteFile(t, filepath.Join(bundle, "safe.txt"), "safe")
	mustWriteFile(t, filepath.Join(replacement, "outside-secret.txt"), "outside-secret")
	root, err := os.OpenRoot(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	original := filepath.Join(base, "bundle-original")
	if err := os.Rename(bundle, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, bundle); err != nil {
		t.Fatal(err)
	}
	absBundle, err := filepath.Abs(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateArtifactBundleRoot(root, absBundle); err == nil || !strings.Contains(err.Error(), "artifact directory changed") {
		t.Fatalf("error=%v, want root identity mismatch", err)
	}
	file, err := root.Open("safe.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil || string(data) != "safe" {
		t.Fatalf("anchored root data=%q err=%v", data, err)
	}
}

func TestSnapshotArtifactFilesRejectsNestedDirectorySwap(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	mustWriteFile(t, filepath.Join(nested, "safe.txt"), "safe-bytes")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := listArtifactBundleFilesRoot(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	mustWriteFile(t, filepath.Join(outsideDir, "safe.txt"), "outside-secret")
	if err := os.Rename(nested, nested+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, nested); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, cleanup, err := snapshotArtifactFiles(root, files)
	defer cleanup()
	if err == nil || !strings.Contains(err.Error(), "nested/safe.txt") {
		t.Fatalf("error=%v, want nested swap rejection", err)
	}
}

func TestSnapshotArtifactFilesSupportsMaximumLengthComponent(t *testing.T) {
	dir := t.TempDir()
	name := "a." + strings.Repeat("x", 252)
	mustWriteFile(t, filepath.Join(dir, name), "safe-bytes")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := listArtifactBundleFilesRoot(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	_, cleanup, err := snapshotArtifactFiles(root, files)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
}

func TestBindArtifactSummaryFileAllowsExternalSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside-secret.md")
	mustWriteFile(t, outside, "external-summary")
	path := filepath.Join(dir, "summary.md")
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	binding, err := bindArtifactSummaryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer binding.file.Close()
	data, err := io.ReadAll(binding.file)
	if err != nil || string(data) != "external-summary" {
		t.Fatalf("summary=%q err=%v", data, err)
	}
}

func TestArtifactsPublishAllowsExternalParentRelativeSummarySymlink(t *testing.T) {
	bundle := t.TempDir()
	mustWriteFile(t, filepath.Join(bundle, "result.txt"), "safe-result")
	external := t.TempDir()
	mustWriteFile(t, filepath.Join(external, "notes.md"), "external-summary")
	summaryDir := filepath.Join(external, "sub")
	if err := os.MkdirAll(summaryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	summaryPath := filepath.Join(summaryDir, "summary.md")
	if err := os.Symlink(filepath.Join("..", "notes.md"), summaryPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := (App{Stdout: io.Discard, Stderr: io.Discard}).artifactsPublish(context.Background(), []string{
		"--dir", bundle,
		"--storage", "local",
		"--summary-file", summaryPath,
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(bundle, "published-artifacts.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "external-summary") {
		t.Fatalf("published markdown omitted external summary:\n%s", body)
	}
}

func TestArtifactPublishSummaryRejectsExternalAliasToSwappedBundleFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "summary.md")
	mustWriteFile(t, path, "safe-summary")
	alias := filepath.Join(t.TempDir(), "summary-alias.md")
	if err := os.Symlink(path, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	original := path + ".original"
	if err := os.Rename(path, original); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside-secret.md")
	mustWriteFile(t, outside, "outside-secret")
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	binding, err := bindArtifactSummaryFile(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer binding.file.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(original, path); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := listArtifactBundleRoot(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	inside := artifactSummaryInsideBundle(dir, dir, mustArtifactRootInfo(t, root), binding, files)
	if !inside {
		t.Fatal("external alias target inside the bundle was not classified as bundle input")
	}
	_, cleanup, err := artifactPublishSummaryText("", binding, inside, root, files)
	defer cleanup()
	if err == nil || !strings.Contains(err.Error(), "summary file changed") {
		t.Fatalf("error=%v, want outside identity rejection", err)
	}
}

func TestArtifactPublishSummaryRejectsSymlinkDotDotSwap(t *testing.T) {
	bundle := t.TempDir()
	nested := filepath.Join(bundle, "nested")
	mustWriteFile(t, filepath.Join(bundle, "summary.md"), "safe-summary")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	aliasBase := t.TempDir()
	bridge := filepath.Join(aliasBase, "bridge")
	if err := os.MkdirAll(bridge, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nested, filepath.Join(bridge, "nested-link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	alias := filepath.Join(aliasBase, "summary-alias.md")
	aliasTarget := "bridge" + string(filepath.Separator) + "nested-link" + string(filepath.Separator) + ".." + string(filepath.Separator) + "summary.md"
	if err := os.Symlink(aliasTarget, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	originalNested := nested + ".original"
	if err := os.Rename(nested, originalNested); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(outside, "summary.md"), "outside-secret")
	if err := os.Symlink(filepath.Join(outside, "child"), nested); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	binding, err := bindArtifactSummaryFile(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer binding.file.Close()
	if err := os.Remove(nested); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(originalNested, nested); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := listArtifactBundleRoot(root, bundle)
	if err != nil {
		t.Fatal(err)
	}
	inside := artifactSummaryInsideBundle(bundle, bundle, mustArtifactRootInfo(t, root), binding, files)
	if !inside {
		t.Fatal("component-wise symlink target inside bundle was classified as external")
	}
	_, cleanup, err := artifactPublishSummaryText("", binding, inside, root, files)
	defer cleanup()
	if err == nil || !strings.Contains(err.Error(), "summary file changed") {
		t.Fatalf("error=%v, want outside identity rejection", err)
	}
}

func TestArtifactPublishSummaryClassifiesNestedDirectoryIdentity(t *testing.T) {
	bundle := t.TempDir()
	nested := filepath.Join(bundle, "nested")
	mustWriteFile(t, filepath.Join(nested, "safe.txt"), "safe")
	root, err := os.OpenRoot(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := listArtifactBundleRoot(root, bundle)
	if err != nil {
		t.Fatal(err)
	}
	nestedInfo, err := os.Stat(nested)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	mustWriteFile(t, outside, "outside-secret")
	outsideInfo, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	binding := &artifactSummaryBinding{
		path:           outside,
		resolvedPath:   outside,
		fileInfo:       outsideInfo,
		directoryInfos: []os.FileInfo{nestedInfo},
	}
	if !artifactSummaryInsideBundle(bundle, bundle, mustArtifactRootInfo(t, root), binding, files) {
		t.Fatal("nested bundle directory identity was classified as external")
	}
}

func TestArtifactPublishSummaryRejectsCaseAliasToSwappedBundleDirectory(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "BundleCase")
	nested := filepath.Join(dir, "nested")
	path := filepath.Join(nested, "summary.md")
	mustWriteFile(t, path, "safe-summary")
	caseAliasDir := filepath.Join(base, "bundlecase")
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(caseAliasDir)
	if err != nil || !os.SameFile(dirInfo, aliasInfo) {
		t.Skip("test requires a case-insensitive filesystem")
	}
	alias := filepath.Join(t.TempDir(), "summary-alias.md")
	if err := os.Symlink(filepath.Join(caseAliasDir, "nested", "summary.md"), alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	originalNested := nested + ".original"
	if err := os.Rename(nested, originalNested); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "summary.md"), "outside-secret")
	if err := os.Symlink(outside, nested); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	binding, err := bindArtifactSummaryFile(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer binding.file.Close()
	if err := os.Remove(nested); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(originalNested, nested); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := listArtifactBundleRoot(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	inside := artifactSummaryInsideBundle(dir, dir, mustArtifactRootInfo(t, root), binding, files)
	if !inside {
		t.Fatal("case-equivalent alias target inside the bundle was not classified as bundle input")
	}
	_, cleanup, err := artifactPublishSummaryText("", binding, inside, root, files)
	defer cleanup()
	if err == nil || !strings.Contains(err.Error(), "summary file changed") {
		t.Fatalf("error=%v, want outside identity rejection", err)
	}
}

func TestArtifactPathWithinPreservesCaseSensitiveSiblings(t *testing.T) {
	base := t.TempDir()
	upper := filepath.Join(base, "BundleCase")
	lower := filepath.Join(base, "bundlecase")
	if err := os.Mkdir(upper, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lower, 0o755); err != nil {
		t.Skip("test requires a case-sensitive filesystem")
	}
	upperInfo, err := os.Stat(upper)
	if err != nil {
		t.Fatal(err)
	}
	lowerInfo, err := os.Stat(lower)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(upperInfo, lowerInfo) {
		t.Skip("test requires distinct case-sensitive siblings")
	}
	if artifactPathWithin(upper, filepath.Join(lower, "summary.md")) {
		t.Fatal("case-sensitive sibling was classified inside bundle")
	}
}

func TestBindArtifactSummaryFileAllowsExternalRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.md")
	mustWriteFile(t, path, "external-summary")
	binding, err := bindArtifactSummaryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer binding.file.Close()
	data, err := io.ReadAll(binding.file)
	if err != nil || string(data) != "external-summary" {
		t.Fatalf("summary=%q err=%v", data, err)
	}
}

func TestArtifactPublishSummaryUsesValidatedSnapshotThroughDirectoryAlias(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "summary.md")
	mustWriteFile(t, path, "safe-summary")
	alias := filepath.Join(t.TempDir(), "bundle-alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	binding, err := bindArtifactSummaryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer binding.file.Close()
	root, err := os.OpenRoot(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := listArtifactBundleRoot(root, alias)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, cleanupSnapshots, err := snapshotArtifactFiles(root, files)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSnapshots()
	outside := filepath.Join(t.TempDir(), "outside-secret.md")
	mustWriteFile(t, outside, "outside-secret")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	resolvedAlias, err := filepath.EvalSymlinks(alias)
	if err != nil {
		t.Fatal(err)
	}
	inside := artifactSummaryInsideBundle(alias, resolvedAlias, mustArtifactRootInfo(t, root), binding, files)
	if !inside {
		t.Fatal("canonical summary path should match symlinked bundle root")
	}
	got, cleanupSummary, err := artifactPublishSummaryText("prefix", binding, inside, root, snapshots)
	defer cleanupSummary()
	if err != nil {
		t.Fatal(err)
	}
	if got != "prefix\n\nsafe-summary" {
		t.Fatalf("summary=%q, want validated snapshot", got)
	}
}

func TestArtifactsPublishAllowsGeneratedOutputSummaryFile(t *testing.T) {
	for _, name := range []string{
		"published-artifacts.md",
		filepath.Join("nested", artifactManifestFilename),
	} {
		t.Run(filepath.ToSlash(name), func(t *testing.T) {
			dir := t.TempDir()
			mustWriteFile(t, filepath.Join(dir, "result.txt"), "safe-result")
			summaryPath := filepath.Join(dir, name)
			mustWriteFile(t, summaryPath, "existing-summary")
			if err := (App{Stdout: io.Discard, Stderr: io.Discard}).artifactsPublish(context.Background(), []string{
				"--dir", dir,
				"--storage", "local",
				"--summary-file", summaryPath,
			}); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(filepath.Join(dir, "published-artifacts.md"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), "existing-summary") {
				t.Fatalf("published markdown omitted generated-file summary:\n%s", body)
			}
		})
	}
}

func TestArtifactsPublishAllowsGeneratedOutputSummaryCaseAlias(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "BundleCase")
	mustWriteFile(t, filepath.Join(dir, "result.txt"), "safe-result")
	mustWriteFile(t, filepath.Join(dir, "published-artifacts.md"), "existing-summary")
	alias := filepath.Join(base, "bundlecase")
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(alias)
	if err != nil || !os.SameFile(dirInfo, aliasInfo) {
		t.Skip("test requires a case-insensitive filesystem")
	}
	if err := (App{Stdout: io.Discard, Stderr: io.Discard}).artifactsPublish(context.Background(), []string{
		"--dir", dir,
		"--storage", "local",
		"--summary-file", filepath.Join(alias, "published-artifacts.md"),
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "published-artifacts.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "existing-summary") {
		t.Fatalf("published markdown omitted case-aliased summary:\n%s", body)
	}
}

func TestArtifactPublishSummaryRejectsIdentityChangeBeforeValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "summary.md")
	mustWriteFile(t, path, "safe-summary")
	binding, err := bindArtifactSummaryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer binding.file.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, path, "replacement-summary")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := listArtifactBundleRoot(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	inside := artifactSummaryInsideBundle(dir, dir, mustArtifactRootInfo(t, root), binding, files)
	if !inside {
		t.Fatal("summary path should remain classified inside the bundle")
	}
	_, cleanup, err := artifactPublishSummaryText("", binding, inside, root, files)
	defer cleanup()
	if err == nil || !strings.Contains(err.Error(), "summary file changed") {
		t.Fatalf("error=%v, want identity change rejection", err)
	}
}

func TestArtifactPublishSummaryRejectsCanonicalNestedParentReswap(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	path := filepath.Join(nested, "summary.md")
	mustWriteFile(t, path, "safe-summary")
	alias := filepath.Join(t.TempDir(), "bundle-alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	root, err := os.OpenRoot(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := listArtifactBundleRoot(root, alias)
	if err != nil {
		t.Fatal(err)
	}
	originalNested := nested + "-original"
	if err := os.Rename(nested, originalNested); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "summary.md"), "outside-secret")
	if err := os.Symlink(outside, nested); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	binding, err := bindArtifactSummaryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer binding.file.Close()
	inside := artifactSummaryInsideBundle(alias, dir, mustArtifactRootInfo(t, root), binding, files)
	if !inside {
		t.Fatal("canonical nested summary should remain classified inside aliased bundle")
	}
	_, cleanup, err := artifactPublishSummaryText("", binding, inside, root, files)
	defer cleanup()
	if err == nil || !strings.Contains(err.Error(), "summary file changed") {
		t.Fatalf("error=%v, want outside identity rejection", err)
	}
}

func TestWriteArtifactManifestUsesValidatedHandleForLocalStorage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "result.txt")
	mustWriteFile(t, path, "safe-bytes")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := listArtifactBundleFilesRoot(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := hashValidatedArtifactFiles(root, files)
	if err != nil {
		t.Fatal(err)
	}
	if validated[0].snapshotFile != nil {
		t.Fatal("local manifest unexpectedly copied file to a snapshot")
	}
	outside := filepath.Join(t.TempDir(), "outside-secret.txt")
	mustWriteFile(t, outside, "outside-secret")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, data, err := writeArtifactManifest(root, artifactPublishOptions{
		Directory: dir,
		Storage:   "local",
	}, validated)
	if err != nil {
		t.Fatal(err)
	}
	var manifest artifactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 1 {
		t.Fatalf("files=%#v", manifest.Files)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte("safe-bytes")))
	if got := manifest.Files[0]; got.SHA256 != wantHash || got.Size != int64(len("safe-bytes")) {
		t.Fatalf("manifest file=%#v, want safe snapshot hash=%s", got, wantHash)
	}
	if got, readErr := os.ReadFile(outside); readErr != nil || string(got) != "outside-secret" {
		t.Fatalf("outside file changed: data=%q err=%v", got, readErr)
	}
}

func TestSnapshotArtifactDataUsesGeneratedBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, artifactManifestFilename)
	mustWriteFile(t, path, "path-bytes")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, cleanup, err := snapshotArtifactData(root, artifactFile{
		Kind: "manifest",
		Name: artifactManifestFilename,
		Path: path,
	}, []byte("generated-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(files) != 1 {
		t.Fatalf("files=%#v", files)
	}
	got, err := io.ReadAll(io.NewSectionReader(files[0].snapshotFile, files[0].snapshotOffset, files[0].snapshotSize))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "generated-bytes" {
		t.Fatalf("snapshot=%q, want generated bytes", got)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte("generated-bytes")))
	if files[0].snapshotHash != wantHash || files[0].snapshotSize != int64(len("generated-bytes")) {
		t.Fatalf("snapshot metadata=%#v, want hash=%s", files[0], wantHash)
	}
}

func TestPublishArtifactFilesBrokerUsesValidatedSnapshotAfterPathSwap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "screenshot.png")
	mustWriteFile(t, path, "safe-bytes")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := listArtifactBundleFilesRoot(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, cleanup, err := snapshotArtifactFiles(root, files)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	outside := filepath.Join(t.TempDir(), "outside-secret.txt")
	mustWriteFile(t, outside, "leak-bytes")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte("safe-bytes")))
	var uploaded string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/artifacts/uploads":
			var req CoordinatorArtifactUploadRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode upload request: %v", err)
			}
			if len(req.Files) != 1 || req.Files[0].SHA256 != wantHash || req.Files[0].Size != int64(len("safe-bytes")) {
				t.Fatalf("request=%#v", req)
			}
			w.Header().Set("content-type", "application/json")
			_, _ = fmt.Fprintf(w, `{"files":[{"name":"screenshot.png","key":"runs/abc/screenshot.png","upload":{"method":"PUT","url":%q,"headers":{"content-length":"10"}},"url":"https://artifacts.example.com/screenshot.png"}]}`, server.URL+"/upload")
		case "/upload":
			data, _ := io.ReadAll(r.Body)
			uploaded = string(data)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, err = publishArtifactFilesBroker(context.Background(), &CoordinatorClient{
		BaseURL: server.URL,
		Token:   "token",
		Client:  server.Client(),
	}, artifactPublishOptions{Storage: "broker", Prefix: "runs/abc"}, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	if uploaded != "safe-bytes" {
		t.Fatalf("uploaded=%q, want validated snapshot", uploaded)
	}
}

func TestWriteArtifactBundleFileDoesNotFollowReservedOutputSymlink(t *testing.T) {
	for _, name := range []string{artifactManifestFilename, "published-artifacts.md"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			outside := filepath.Join(t.TempDir(), "outside-secret.txt")
			mustWriteFile(t, outside, "outside-secret")
			if err := os.Symlink(outside, filepath.Join(dir, name)); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()

			err = writeArtifactBundleFile(root, name, []byte("generated-output"), 0o644)
			if got, readErr := os.ReadFile(outside); readErr != nil || string(got) != "outside-secret" {
				t.Fatalf("outside file changed: data=%q err=%v writeErr=%v", got, readErr, err)
			}
			if err != nil {
				return
			}
			info, statErr := os.Lstat(filepath.Join(dir, name))
			if statErr != nil {
				t.Fatal(statErr)
			}
			if !info.Mode().IsRegular() {
				t.Fatalf("generated output mode=%v, want regular file", info.Mode())
			}
			got, readErr := os.ReadFile(filepath.Join(dir, name))
			if readErr != nil || string(got) != "generated-output" {
				t.Fatalf("generated output=%q err=%v", got, readErr)
			}
		})
	}
}

func TestWriteArtifactBundleFilePreservesExistingMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, artifactManifestFilename)
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	initialInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	wantMode := initialInfo.Mode().Perm()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := writeArtifactBundleFile(root, artifactManifestFilename, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != wantMode {
		t.Fatalf("mode=%#o, want preserved %#o", got, wantMode)
	}
}

func TestArtifactsPublishRejectsReservedOutputDirectoriesBeforeSideEffects(t *testing.T) {
	for _, reservedName := range []string{
		artifactManifestFilename,
		"published-artifacts.md",
	} {
		t.Run(reservedName, func(t *testing.T) {
			dir := t.TempDir()
			mustWriteFile(t, filepath.Join(dir, "safe.txt"), "safe")
			mustWriteFile(t, filepath.Join(dir, reservedName, "nested.txt"), "nested")

			var stdout bytes.Buffer
			err := (App{Stdout: &stdout, Stderr: io.Discard}).artifactsPublish(context.Background(), []string{
				"--dir", dir,
				"--storage", "local",
			})
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 {
				t.Fatalf("error=%v, want exit 2", err)
			}
			if !strings.Contains(exitErr.Message, "reserved output path") || !strings.Contains(exitErr.Message, reservedName) {
				t.Fatalf("message=%q", exitErr.Message)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout=%q, want no publish output", stdout.String())
			}
		})
	}
}

func TestArtifactsPublishWritesManifestByDefault(t *testing.T) {
	dir := t.TempDir()
	data := []byte("png-data")
	mustWriteFile(t, filepath.Join(dir, "screenshot.png"), string(data))

	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	err := app.artifactsPublish(context.Background(), []string{
		"--dir", dir,
		"--storage", "local",
		"--base-url", "https://artifacts.example.com/proof",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "manifest:") {
		t.Fatalf("stdout missing manifest path:\n%s", stdout.String())
	}
	manifest, _, err := readArtifactManifestRef(context.Background(), filepath.Join(dir, artifactManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 1 || manifest.Storage.Backend != "local" || len(manifest.Files) != 1 {
		t.Fatalf("manifest=%#v", manifest)
	}
	file := manifest.Files[0]
	if file.Name != "screenshot.png" || file.ContentType != "image/png" || file.Size != int64(len(data)) || file.SHA256 == "" {
		t.Fatalf("file=%#v", file)
	}
	if file.URL != "https://artifacts.example.com/proof/screenshot.png" {
		t.Fatalf("url=%q", file.URL)
	}
	if file.AccessPolicy != "public" {
		t.Fatalf("accessPolicy=%q", file.AccessPolicy)
	}
}

func TestArtifactsPublishSkipManifest(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "screenshot.png"), "png-data")
	app := App{Stdout: io.Discard, Stderr: io.Discard}
	err := app.artifactsPublish(context.Background(), []string{
		"--dir", dir,
		"--storage", "local",
		"--skip-manifest",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, artifactManifestFilename)); !os.IsNotExist(err) {
		t.Fatalf("manifest should not exist, stat err=%v", err)
	}
}

func TestArtifactsPublishSkipManifestDoesNotReadUnusedFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "local", args: []string{"--storage", "local"}},
		{name: "hosted-dry-run", args: []string{"--storage", "s3", "--bucket", "qa", "--dry-run"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "already-hosted.bin")
			mustWriteFile(t, path, "bytes-not-needed")
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(path, 0o600)
			if file, err := os.Open(path); err == nil {
				_ = file.Close()
				t.Skip("filesystem does not enforce unreadable mode")
			}

			args := append([]string{"--dir", dir, "--skip-manifest", "--no-comment"}, tc.args...)
			err := (App{Stdout: io.Discard, Stderr: io.Discard}).artifactsPublish(context.Background(), args)
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPublishArtifactFilesDryRunS3(t *testing.T) {
	files := []artifactFile{{Kind: "gif", Name: "screen.gif", Path: "screen.gif"}}
	opts := artifactPublishOptions{
		Storage: "s3",
		Bucket:  "qa",
		Region:  "us-east-1",
		Prefix:  "runs/abc",
		DryRun:  true,
		Expires: time.Hour,
	}
	published, err := publishArtifactFiles(context.Background(), opts, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 || published[0].URL != "https://qa.s3.us-east-1.amazonaws.com/runs/abc/screen.gif" {
		t.Fatalf("published=%#v", published)
	}
}

func TestPublishArtifactFilesDryRunS3BaseURLWinsOverPresign(t *testing.T) {
	files := []artifactFile{{Kind: "screenshot", Name: "screenshot.png", Path: "screenshot.png"}}
	opts := artifactPublishOptions{
		Storage: "r2",
		Bucket:  "qa",
		Region:  "auto",
		Prefix:  "runs/abc",
		BaseURL: "https://artifacts.example.com",
		Presign: true,
		DryRun:  true,
		Expires: time.Hour,
	}
	published, err := publishArtifactFiles(context.Background(), opts, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 || published[0].URL != "https://artifacts.example.com/runs/abc/screenshot.png" {
		t.Fatalf("published=%#v", published)
	}
}

func TestPublishArtifactFilesBrokerUploadsViaGrantedURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "screenshot.png")
	mustWriteFile(t, path, "png-data")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := listArtifactBundleFilesRoot(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, cleanup, err := snapshotArtifactFiles(root, files)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte("png-data")))
	var uploaded string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/artifacts/uploads":
			var req CoordinatorArtifactUploadRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode upload request: %v", err)
			}
			if len(req.Files) != 1 || req.Files[0].Name != "screenshot.png" || req.Files[0].SHA256 != wantHash {
				t.Fatalf("request=%#v", req)
			}
			w.Header().Set("content-type", "application/json")
			_, _ = fmt.Fprintf(w, `{
				"backend":"r2",
				"bucket":"qa",
				"prefix":"runs/abc",
				"expiresAt":"2026-05-08T00:00:00Z",
				"files":[{
					"name":"screenshot.png",
					"key":"runs/abc/screenshot.png",
					"upload":{"method":"PUT","url":%q,"headers":{"content-type":"image/png","content-length":"8"},"expiresAt":"2026-05-08T00:00:00Z"},
					"url":"https://artifacts.example.com/runs/abc/screenshot.png"
				}]
			}`, server.URL+"/upload/screenshot.png")
		case "/upload/screenshot.png":
			if r.Method != http.MethodPut {
				t.Fatalf("method=%s", r.Method)
			}
			if r.ContentLength != int64(len("png-data")) {
				t.Fatalf("content length=%d", r.ContentLength)
			}
			if len(r.TransferEncoding) > 0 {
				t.Fatalf("transfer encoding=%v", r.TransferEncoding)
			}
			data, _ := io.ReadAll(r.Body)
			uploaded = string(data)
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	published, err := publishArtifactFilesBroker(context.Background(), &CoordinatorClient{
		BaseURL: server.URL,
		Token:   "token",
		Client:  server.Client(),
	}, artifactPublishOptions{Storage: "broker", Prefix: "runs/abc"}, snapshots)
	if err != nil {
		t.Fatal(err)
	}
	if uploaded != "png-data" {
		t.Fatalf("uploaded=%q", uploaded)
	}
	if len(published) != 1 || published[0].URL != "https://artifacts.example.com/runs/abc/screenshot.png" {
		t.Fatalf("published=%#v", published)
	}
	if published[0].Key != "runs/abc/screenshot.png" {
		t.Fatalf("key=%q", published[0].Key)
	}
}

func TestArtifactsPublishBrokerDryRunDoesNotRequireTemporaryStorage(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "result.txt"), "safe-bytes")
	configPath := filepath.Join(t.TempDir(), "missing.yaml")
	unusableTemp := filepath.Join(t.TempDir(), "missing-temp")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/artifacts/uploads" {
			http.NotFound(w, r)
			return
		}
		requests++
		var input CoordinatorArtifactUploadRequest
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		response := CoordinatorArtifactUploadResponse{Backend: "r2", Prefix: input.Prefix}
		for _, file := range input.Files {
			grant := CoordinatorArtifactUploadGrant{
				Name: file.Name,
				Key:  input.Prefix + "/" + file.Name,
				URL:  "https://artifacts.example.test/" + file.Name,
			}
			grant.Upload.Method = http.MethodPut
			grant.Upload.URL = "https://upload.example.test/" + file.Name
			grant.Upload.Headers = map[string]string{"content-length": strconv.FormatInt(file.Size, 10)}
			response.Files = append(response.Files, grant)
		}
		w.Header().Set("content-type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(configPath))
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "token")
	t.Setenv("TMPDIR", unusableTemp)
	t.Setenv("TMP", unusableTemp)
	t.Setenv("TEMP", unusableTemp)
	err := (App{Stdout: io.Discard, Stderr: io.Discard}).artifactsPublish(context.Background(), []string{
		"--dir", dir,
		"--storage", "broker",
		"--dry-run",
		"--skip-manifest",
		"--no-comment",
	})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("upload grant requests=%d, want 1", requests)
	}
}

func TestArtifactsPullDownloadsAndVerifiesManifest(t *testing.T) {
	payload := []byte("png-data")
	hash := fmt.Sprintf("%x", sha256.Sum256(payload))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/screenshot.png" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "image/png")
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	dir := t.TempDir()
	manifest := artifactManifest{
		SchemaVersion: 1,
		GeneratedAt:   "2026-05-25T00:00:00Z",
		Storage:       artifactManifestStore{Backend: "local"},
		Files: []artifactManifestFile{{
			Kind:        "screenshot",
			Name:        "nested/screenshot.png",
			URL:         server.URL + "/screenshot.png",
			ContentType: "image/png",
			Size:        int64(len(payload)),
			SHA256:      hash,
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, artifactManifestFilename)
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "pull")
	result, err := pullArtifactManifest(context.Background(), manifestPath, output, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0].SHA256 != hash {
		t.Fatalf("result=%#v", result)
	}
	got, err := os.ReadFile(filepath.Join(output, "nested", "screenshot.png"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload=%q", got)
	}

	remoteManifest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write(data)
	}))
	defer remoteManifest.Close()
	listed, ref, err := readArtifactManifestRef(context.Background(), remoteManifest.URL)
	if err != nil {
		t.Fatal(err)
	}
	if ref != remoteManifest.URL || len(listed.Files) != 1 {
		t.Fatalf("listed=%#v ref=%s", listed, ref)
	}
}

func TestDownloadArtifactURLRejectsContentLengthAboveLimit(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/octet-stream")
		w.Header().Set("content-length", fmt.Sprint(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	outPath := filepath.Join(t.TempDir(), "artifact.bin")
	_, _, _, err := downloadArtifactURL(context.Background(), artifactManifestFile{
		Name: "artifact.bin",
		URL:  server.URL,
		Size: 4,
	}, outPath)
	if err == nil || !strings.Contains(err.Error(), "content-length 1024 exceeds limit 4") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Fatalf("artifact output was created: %v", err)
	}
}

func TestDownloadArtifactURLStopsStreamingAboveDeclaredSize(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload[:4])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		_, _ = w.Write(payload[4:])
	}))
	defer server.Close()

	outPath := filepath.Join(t.TempDir(), "artifact.bin")
	_, _, _, err := downloadArtifactURL(context.Background(), artifactManifestFile{
		Name: "artifact.bin",
		URL:  server.URL,
		Size: 4,
	}, outPath)
	if err == nil || !strings.Contains(err.Error(), "response exceeds limit 4") {
		t.Fatalf("err=%v", err)
	}
	info, statErr := os.Stat(outPath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Size() > 4 {
		t.Fatalf("artifact output grew past declared size: %d", info.Size())
	}
}

func TestArtifactHTTPFlowsRejectCrossOriginRedirects(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetRequests.Add(1)
	}))
	defer target.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/collect", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	dir := t.TempDir()
	uploadPath := filepath.Join(dir, "upload.txt")
	mustWriteFile(t, uploadPath, "private artifact")
	signedURL := redirect.URL + "/artifact?X-Amz-Signature=secret-signature"
	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "manifest",
			run: func() error {
				_, _, err := readArtifactManifestRef(context.Background(), signedURL)
				return err
			},
		},
		{
			name: "download",
			run: func() error {
				_, _, _, err := downloadArtifactURL(context.Background(), artifactManifestFile{
					Name: "artifact.txt",
					URL:  signedURL,
					Size: 64,
				}, filepath.Join(dir, "download.txt"))
				return err
			},
		},
		{
			name: "upload",
			run: func() error {
				return uploadArtifactGrant(context.Background(), uploadPath, CoordinatorArtifactUploadGrant{
					Name: "artifact.txt",
					Upload: struct {
						Method    string            `json:"method"`
						URL       string            `json:"url"`
						Headers   map[string]string `json:"headers"`
						ExpiresAt string            `json:"expiresAt"`
					}{Method: http.MethodPut, URL: signedURL},
				})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			if err == nil || !strings.Contains(err.Error(), errArtifactCrossOriginRedirect.Error()) {
				t.Fatalf("error=%v, want cross-origin redirect rejection", err)
			}
			if strings.Contains(err.Error(), "secret-signature") {
				t.Fatalf("error leaked signed URL: %v", err)
			}
		})
	}
	if got := targetRequests.Load(); got != 0 {
		t.Fatalf("cross-origin target received %d requests", got)
	}
}

func TestArtifactHTTPFlowsAllowSameOriginRedirects(t *testing.T) {
	manifest := `{"schemaVersion":1,"generatedAt":"2026-07-05T00:00:00Z","storage":{"backend":"broker"},"files":[]}`
	payload := "private artifact"
	var uploadedMethod, uploadedBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest-start":
			http.Redirect(w, r, "/manifest", http.StatusFound)
		case "/manifest":
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, manifest)
		case "/download-start":
			http.Redirect(w, r, "/download", http.StatusFound)
		case "/download":
			_, _ = io.WriteString(w, payload)
		case "/upload-start":
			http.Redirect(w, r, "/upload", http.StatusTemporaryRedirect)
		case "/upload":
			uploadedMethod = r.Method
			body, _ := io.ReadAll(r.Body)
			uploadedBody = string(body)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	listed, _, err := readArtifactManifestRef(context.Background(), server.URL+"/manifest-start")
	if err != nil || listed.SchemaVersion != 1 {
		t.Fatalf("manifest=%#v err=%v", listed, err)
	}
	dir := t.TempDir()
	_, size, _, err := downloadArtifactURL(context.Background(), artifactManifestFile{
		Name: "artifact.txt",
		URL:  server.URL + "/download-start",
		Size: int64(len(payload)),
	}, filepath.Join(dir, "download.txt"))
	if err != nil || size != int64(len(payload)) {
		t.Fatalf("download size=%d err=%v", size, err)
	}
	uploadPath := filepath.Join(dir, "upload.txt")
	mustWriteFile(t, uploadPath, payload)
	err = uploadArtifactGrant(context.Background(), uploadPath, CoordinatorArtifactUploadGrant{
		Name: "artifact.txt",
		Upload: struct {
			Method    string            `json:"method"`
			URL       string            `json:"url"`
			Headers   map[string]string `json:"headers"`
			ExpiresAt string            `json:"expiresAt"`
		}{Method: http.MethodPut, URL: server.URL + "/upload-start"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if uploadedMethod != http.MethodPut || uploadedBody != payload {
		t.Fatalf("redirected upload method=%q body=%q", uploadedMethod, uploadedBody)
	}
}

func TestArtifactsPullRejectsNegativeManifestSize(t *testing.T) {
	dir := t.TempDir()
	manifest := artifactManifest{
		SchemaVersion: 1,
		GeneratedAt:   "2026-05-25T00:00:00Z",
		Storage:       artifactManifestStore{Backend: "local"},
		Files: []artifactManifestFile{{
			Name: "screenshot.png",
			Path: "screenshot.png",
			Size: -1,
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, artifactManifestFilename)
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = pullArtifactManifest(context.Background(), manifestPath, filepath.Join(dir, "pull"), false)
	if err == nil || !strings.Contains(err.Error(), "artifact size for screenshot.png is invalid: -1") {
		t.Fatalf("err=%v", err)
	}
}

func TestArtifactsPullAllowsOutputAfterManifestRef(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("png-data")
	hash := fmt.Sprintf("%x", sha256.Sum256(payload))
	if err := os.WriteFile(filepath.Join(dir, "screenshot.png"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := artifactManifest{
		SchemaVersion: 1,
		GeneratedAt:   "2026-05-25T00:00:00Z",
		Storage:       artifactManifestStore{Backend: "local"},
		Files: []artifactManifestFile{{
			Kind:        "screenshot",
			Name:        "screenshot.png",
			Path:        "screenshot.png",
			ContentType: "image/png",
			Size:        int64(len(payload)),
			SHA256:      hash,
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, artifactManifestFilename)
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "pull")
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	if err := app.artifactsPull(context.Background(), []string{manifestPath, "--output", output}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "pulled:") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	got, err := os.ReadFile(filepath.Join(output, "screenshot.png"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload=%q", got)
	}
}

func TestArtifactsPullUsesLocalPathForR2ManifestURL(t *testing.T) {
	dir := t.TempDir()
	payload := []byte("png-data")
	hash := fmt.Sprintf("%x", sha256.Sum256(payload))
	if err := os.WriteFile(filepath.Join(dir, "screenshot.png"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := artifactManifest{
		SchemaVersion: 1,
		GeneratedAt:   "2026-05-25T00:00:00Z",
		Storage:       artifactManifestStore{Backend: "cloudflare"},
		Files: []artifactManifestFile{{
			Kind:        "screenshot",
			Name:        "screenshot.png",
			Path:        "screenshot.png",
			URL:         "r2://qa-artifacts/runs/abc/screenshot.png",
			ContentType: "image/png",
			Size:        int64(len(payload)),
			SHA256:      hash,
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, artifactManifestFilename)
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "pull")
	result, err := pullArtifactManifest(context.Background(), manifestPath, output, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0].URL != "r2://qa-artifacts/runs/abc/screenshot.png" {
		t.Fatalf("result=%#v", result)
	}
	got, err := os.ReadFile(filepath.Join(output, "screenshot.png"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload=%q", got)
	}
}

func TestArtifactsPullRejectsHashMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("changed"))
	}))
	defer server.Close()
	dir := t.TempDir()
	manifest := artifactManifest{
		SchemaVersion: 1,
		GeneratedAt:   "2026-05-25T00:00:00Z",
		Storage:       artifactManifestStore{Backend: "local"},
		Files: []artifactManifestFile{{
			Name:   "screenshot.png",
			URL:    server.URL,
			SHA256: strings.Repeat("0", 64),
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, artifactManifestFilename)
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "pull")
	if err := os.MkdirAll(output, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(output, "screenshot.png")
	if err := os.WriteFile(existing, []byte("known-good"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = pullArtifactManifest(context.Background(), manifestPath, output, true)
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("err=%v", err)
	}
	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "known-good" {
		t.Fatalf("existing output was replaced: %q", got)
	}
	temps, err := filepath.Glob(filepath.Join(output, ".screenshot.png.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temporary outputs left behind: %v", temps)
	}
}

func TestArtifactsPullRejectsRemotePathOnlyManifest(t *testing.T) {
	manifest := artifactManifest{
		SchemaVersion: 1,
		GeneratedAt:   "2026-05-25T00:00:00Z",
		Storage:       artifactManifestStore{Backend: "broker"},
		Files: []artifactManifestFile{{
			Name: "screenshot.png",
			Path: "/etc/passwd",
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write(data)
	}))
	defer server.Close()
	_, err = pullArtifactManifest(context.Background(), server.URL, filepath.Join(t.TempDir(), "pull"), false)
	if err == nil || !strings.Contains(err.Error(), "requires url") {
		t.Fatalf("err=%v", err)
	}
}

func TestArtifactsPullRejectsEscapingLocalPath(t *testing.T) {
	dir := t.TempDir()
	manifest := artifactManifest{
		SchemaVersion: 1,
		GeneratedAt:   "2026-05-25T00:00:00Z",
		Storage:       artifactManifestStore{Backend: "local"},
		Files: []artifactManifestFile{{
			Name: "screenshot.png",
			Path: "../secret.txt",
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, artifactManifestFilename)
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = pullArtifactManifest(context.Background(), manifestPath, filepath.Join(dir, "pull"), false)
	if err == nil || !strings.Contains(err.Error(), "invalid artifact source path") {
		t.Fatalf("err=%v", err)
	}
}

func TestArtifactsPullRejectsSymlinkedLocalSource(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "bundle")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(bundle, "secret")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	manifest := artifactManifest{
		SchemaVersion: 1,
		GeneratedAt:   "2026-05-25T00:00:00Z",
		Storage:       artifactManifestStore{Backend: "local"},
		Files: []artifactManifestFile{{
			Name: "secret",
			Path: "secret",
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(bundle, artifactManifestFilename)
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "pull")
	_, err = pullArtifactManifest(context.Background(), manifestPath, output, false)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(output, "secret")); !os.IsNotExist(err) {
		t.Fatalf("pulled symlink source should not exist, stat err=%v", err)
	}
}

func TestArtifactsPullRejectsSymlinkedOutputParent(t *testing.T) {
	payload := []byte("png-data")
	hash := fmt.Sprintf("%x", sha256.Sum256(payload))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/artifact-manifest.json" {
			manifest := artifactManifest{
				SchemaVersion: 1,
				GeneratedAt:   "2026-05-25T00:00:00Z",
				Storage:       artifactManifestStore{Backend: "broker"},
				Files: []artifactManifestFile{{
					Kind:   "screenshot",
					Name:   "link/owned.txt",
					URL:    "http://" + r.Host + "/owned.txt",
					Size:   int64(len(payload)),
					SHA256: hash,
				}},
			}
			_ = json.NewEncoder(w).Encode(manifest)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	dir := t.TempDir()
	output := filepath.Join(dir, "pull")
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(output, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(output, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := pullArtifactManifest(context.Background(), server.URL+"/artifact-manifest.json", output, false)
	if err == nil || !strings.Contains(err.Error(), "symlinked parent") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "owned.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside file should not exist, stat err=%v", err)
	}
}

func TestUploadArtifactGrantRejectsSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "screenshot.png")
	mustWriteFile(t, path, "png-data")

	err := uploadArtifactGrant(context.Background(), path, CoordinatorArtifactUploadGrant{
		Name: "screenshot.png",
		Upload: struct {
			Method    string            `json:"method"`
			URL       string            `json:"url"`
			Headers   map[string]string `json:"headers"`
			ExpiresAt string            `json:"expiresAt"`
		}{
			Method:  "PUT",
			URL:     "https://artifacts.example.com/upload",
			Headers: map[string]string{"content-length": "1"},
		},
	})
	if err == nil {
		t.Fatal("expected size mismatch error")
	}
	if !strings.Contains(err.Error(), "size changed after broker grant") {
		t.Fatalf("err=%v", err)
	}
}

func TestArtifactsPublishValidatesSummaryBeforeMarkdownSideEffects(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "screenshot.png"), "png")
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.artifactsPublish(context.Background(), []string{
		"--dir", dir,
		"--storage", "s3",
		"--bucket", "qa",
		"--dry-run",
		"--summary-file", filepath.Join(dir, "missing.md"),
	})
	if err == nil {
		t.Fatal("expected missing summary file error")
	}
	if !strings.Contains(err.Error(), "read summary file") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "published-artifacts.md")); !os.IsNotExist(statErr) {
		t.Fatalf("published markdown should not be written before summary validation: %v", statErr)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout should be empty before publish side effects, got %q", stdout.String())
	}
}

func TestPrintArtifactWarningUsesRescueShape(t *testing.T) {
	var out bytes.Buffer
	printArtifactWarning(&out, artifactWarning{
		Problem: "WebVNC daemon not running",
		Detail:  "portal has no active bridge",
		Rescue:  []string{"crabbox webvnc reset --id blue --open"},
	})
	for _, want := range []string{
		"problem: WebVNC daemon not running\n",
		"detail: portal has no active bridge\n",
		"rescue: crabbox webvnc reset --id blue --open\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("warning missing %q:\n%s", want, out.String())
		}
	}
}

func TestArtifactCollectFailureJSONIsParseable(t *testing.T) {
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	result := artifactCollectResult{
		Directory: "/tmp/bundle",
		Metadata:  artifactBundleMetadata{LeaseID: "cbx_123"},
		Files:     []artifactFile{{Kind: "metadata", Name: "metadata.json", Path: "/tmp/bundle/metadata.json"}},
	}
	err := app.finishArtifactCollectFailure(&result, true, exit(5, "capture screenshot: boom"), artifactWarning{
		Problem: rescueScreenshotCaptureBroken,
		Detail:  "capture screenshot: boom",
		Rescue:  []string{"crabbox desktop doctor --id cbx_123"},
	})
	if err == nil {
		t.Fatal("expected original collection error")
	}
	if strings.Contains(stdout.String(), "problem:") {
		t.Fatalf("json stdout contains human rescue text: %q", stdout.String())
	}
	var decoded artifactCollectResult
	if decodeErr := json.Unmarshal(stdout.Bytes(), &decoded); decodeErr != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", decodeErr, stdout.String())
	}
	if decoded.Error == nil || decoded.Error.Code != "screenshot-capture-broken" {
		t.Fatalf("decoded error=%#v", decoded.Error)
	}
	if decoded.Files == nil || len(decoded.Files) != 1 {
		t.Fatalf("files should remain a JSON array: %#v", decoded.Files)
	}
	if len(decoded.Warnings) != 1 || decoded.Warnings[0].Problem != rescueScreenshotCaptureBroken {
		t.Fatalf("warnings=%#v", decoded.Warnings)
	}
}

func TestContactSheetWarningJSONIsParseable(t *testing.T) {
	var stdout bytes.Buffer
	result := artifactCollectResult{
		Directory: "/tmp/bundle",
		Metadata:  artifactBundleMetadata{LeaseID: "cbx_123"},
		Files:     []artifactFile{{Kind: "video", Name: "screen.mp4", Path: "/tmp/bundle/screen.mp4"}},
	}
	appendContactSheetWarning(&result.Warnings, exit(2, "ffprobe is required"))
	if err := json.NewEncoder(&stdout).Encode(result); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "warning:") || strings.Contains(stdout.String(), "problem:") {
		t.Fatalf("json stdout contains human warning text: %q", stdout.String())
	}
	var decoded artifactCollectResult
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout.String())
	}
	if len(decoded.Warnings) != 1 {
		t.Fatalf("warnings=%#v", decoded.Warnings)
	}
	if decoded.Warnings[0].Problem != rescueArtifactCaptureFailed || !strings.Contains(decoded.Warnings[0].Detail, "contact-sheet skipped") {
		t.Fatalf("warning=%#v", decoded.Warnings[0])
	}
}

func TestArtifactsCollectValidationThroughKongStripsCommandPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.Run(context.Background(), []string{"artifacts", "collect", "--gif", "--id", "dummy"})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "--gif requires --video or --all") {
		t.Fatalf("err=%v stderr=%s stdout=%s", err, stderr.String(), stdout.String())
	}
}

func TestWriteArtifactWebVNCStatusRecordsWarningsWithoutStdout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/leases/cbx_123/webvnc/status" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(CoordinatorWebVNCStatus{
			LeaseID:         "cbx_123",
			Slug:            "blue-lobster",
			BridgeConnected: false,
			ViewerConnected: true,
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	var stdout bytes.Buffer
	app := App{Stdout: &stdout, Stderr: io.Discard}
	var warnings []artifactWarning
	path, ok, err := app.writeArtifactWebVNCStatus(context.Background(), Config{
		Provider:    "aws",
		TargetOS:    targetLinux,
		Coordinator: server.URL,
		CoordToken:  "token",
	}, SSHTarget{TargetOS: targetLinux}, "cbx_123", dir, &warnings)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || path == "" {
		t.Fatalf("path=%q ok=%t", path, ok)
	}
	if stdout.Len() != 0 {
		t.Fatalf("status helper should not write rescue text directly to stdout, got %q", stdout.String())
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings=%#v", warnings)
	}
	if warnings[0].Problem != rescueVNCBridgeNotRunning {
		t.Fatalf("warnings=%#v", warnings)
	}
}

func mustWriteFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustArtifactRootInfo(t *testing.T, root *os.Root) os.FileInfo {
	t.Helper()
	info, err := root.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	return info
}
