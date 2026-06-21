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
	"strings"
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

func TestArtifactsPublishRejectsReservedOutputDirectoriesBeforeSideEffects(t *testing.T) {
	for _, reservedName := range []string{artifactManifestFilename, "published-artifacts.md"} {
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
	}, artifactPublishOptions{Storage: "broker", Prefix: "runs/abc"}, []artifactFile{
		{Kind: "screenshot", Name: "screenshot.png", Path: path},
	})
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
