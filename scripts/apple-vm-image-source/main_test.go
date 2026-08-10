package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testImage  = "https://cloud-images.ubuntu.com/releases/resolute/release-20260731/ubuntu-26.04-server-cloudimg-arm64.img"
	testDigest = "3e113fdd41f39e13729375173bb2ae793f87dc6db4294e5251ff2476971788ba"
)

func parseTestFile(t *testing.T, source string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "test.go", source, 0)
	if err != nil {
		t.Fatalf("parse test source: %v", err)
	}
	return file
}

func portableSource(imageExpression, digestExpression string) string {
	return fmt.Sprintf(`package cli
var osImageSpecs = map[string]osImageSpec{
	"ubuntu:26.04": {
		AppleVMImage: %s,
		AppleVMSHA256: %s,
	},
}
`, imageExpression, digestExpression)
}

func TestExtractPortableSourceIgnoresCommentsAndStringDecoys(t *testing.T) {
	source := fmt.Sprintf(`package cli
// "ubuntu:26.04": { AppleVMImage: "https://comment.invalid/image.img" }
var interpretedDecoy = "AppleVMImage: %s AppleVMSHA256: %s"
var rawDecoy = %s
var osImageSpecs = map[string]osImageSpec{
	"ubuntu:26.04": {
		AppleVMImage: %q,
		AppleVMSHA256: %q,
	},
}
`, testImage, testDigest, "`\"ubuntu:26.04\": { AppleVMImage: \"https://raw.invalid/image.img\" }`", testImage, testDigest)

	got, err := extractPortableSource(parseTestFile(t, source))
	if err != nil {
		t.Fatalf("extract portable source: %v", err)
	}
	if got != (imageSource{image: testImage, digest: testDigest}) {
		t.Fatalf("extractPortableSource() = %#v", got)
	}
}

func TestExtractPortableSourceRejectsNonLiteralExpressions(t *testing.T) {
	tests := []struct {
		name   string
		image  string
		digest string
	}{
		{name: "identifier", image: "imageURL", digest: fmt.Sprintf("%q", testDigest)},
		{name: "call", image: "imageURL()", digest: fmt.Sprintf("%q", testDigest)},
		{name: "concatenation", image: `"https://cloud-images.ubuntu.com/" + "image.img"`, digest: fmt.Sprintf("%q", testDigest)},
		{name: "digest identifier", image: fmt.Sprintf("%q", testImage), digest: "imageDigest"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := extractPortableSource(parseTestFile(t, portableSource(test.image, test.digest)))
			if err == nil || !strings.Contains(err.Error(), "direct string literal") {
				t.Fatalf("extractPortableSource() error = %v", err)
			}
		})
	}
}

func TestExtractPortableSourceRejectsDuplicateAndMissingFields(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{
			name: "duplicate field",
			source: fmt.Sprintf(`package cli
var osImageSpecs = map[string]osImageSpec{
	"ubuntu:26.04": {AppleVMImage: %q, AppleVMImage: %q, AppleVMSHA256: %q},
}`, testImage, testImage, testDigest),
		},
		{
			name: "missing field",
			source: fmt.Sprintf(`package cli
var osImageSpecs = map[string]osImageSpec{
	"ubuntu:26.04": {AppleVMImage: %q},
}`, testImage),
		},
		{
			name: "duplicate entry",
			source: fmt.Sprintf(`package cli
var osImageSpecs = map[string]osImageSpec{
	"ubuntu:26.04": {AppleVMImage: %q, AppleVMSHA256: %q},
	"ubuntu:26.04": {AppleVMImage: %q, AppleVMSHA256: %q},
}`, testImage, testDigest, testImage, testDigest),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := extractPortableSource(parseTestFile(t, test.source)); err == nil || !strings.Contains(err.Error(), "exactly once") {
				t.Fatalf("extractPortableSource() error = %v", err)
			}
		})
	}
}

func TestExtractFinalFallbackUsesFinalTopLevelReturn(t *testing.T) {
	source := fmt.Sprintf(`package applevm
func defaultAppleVMImage(osImage string) string {
	if osImage != "" {
		return "https://nested.invalid/image.img"
	}
	for false {
		return "https://nested-loop.invalid/image.img"
	}
	return %q
}
`, testImage)
	got, err := extractFinalFallback(parseTestFile(t, source), "defaultAppleVMImage")
	if err != nil {
		t.Fatalf("extract final fallback: %v", err)
	}
	if got != testImage {
		t.Fatalf("extractFinalFallback() = %q, want %q", got, testImage)
	}
}

func TestExtractFinalFallbackRejectsNonLiteralFinalReturn(t *testing.T) {
	for _, expression := range []string{"imageURL", "imageURL()", `"https://example.invalid/" + "image.img"`} {
		source := fmt.Sprintf(`package applevm
func defaultAppleVMImage(osImage string) string {
	if osImage != "" { return %q }
	return %s
}`, testImage, expression)
		_, err := extractFinalFallback(parseTestFile(t, source), "defaultAppleVMImage")
		if err == nil || !strings.Contains(err.Error(), "direct string literal") {
			t.Fatalf("extractFinalFallback(%q) error = %v", expression, err)
		}
	}
}

func TestExtractCurrentRealSource(t *testing.T) {
	root := filepath.Join("..", "..")
	portableFile, err := parseRequiredFile(root, portableSourcePath)
	if err != nil {
		t.Fatal(err)
	}
	portable, err := extractPortableSource(portableFile)
	if err != nil {
		t.Fatal(err)
	}
	if portable != (imageSource{image: testImage, digest: testDigest}) {
		t.Fatalf("portable source = %#v", portable)
	}

	providerFile, err := parseRequiredFile(root, providerSourcePath)
	if err != nil {
		t.Fatal(err)
	}
	image, err := extractFinalFallback(providerFile, "defaultAppleVMImage")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := extractFinalFallback(providerFile, "defaultAppleVMImageSHA256")
	if err != nil {
		t.Fatal(err)
	}
	if image != portable.image || digest != portable.digest {
		t.Fatalf("provider fallback = %q/%q, portable = %#v", image, digest, portable)
	}
}

func TestProbeImageRejectsRedirectWithoutExposingLocation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.invalid/bearer-secret/image.img?token=private", http.StatusFound)
	}))
	defer server.Close()

	imageURL, err := url.Parse(server.URL + "/image.img")
	if err != nil {
		t.Fatal(err)
	}
	err = probeImage(context.Background(), newHTTPClient(), imageURL)
	if err == nil || !strings.Contains(err.Error(), "redirected") {
		t.Fatalf("probeImage() error = %v", err)
	}
	for _, sensitive := range []string{"bearer-secret", "private", "example.invalid"} {
		if strings.Contains(err.Error(), sensitive) {
			t.Fatalf("redirect error exposed %q: %v", sensitive, err)
		}
	}
}

func TestFetchChecksumManifestRejectsOversizedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("x", maxChecksumBytes+1)))
	}))
	defer server.Close()

	imageURL, err := url.Parse(server.URL + "/release/image.img")
	if err != nil {
		t.Fatal(err)
	}
	_, err = fetchChecksumManifest(context.Background(), newHTTPClient(), imageURL)
	if err == nil || !strings.Contains(err.Error(), "bounded read limit") {
		t.Fatalf("fetchChecksumManifest() error = %v", err)
	}
}

func TestVerifyRemoteSourceRejectsDigestMismatch(t *testing.T) {
	server := newImageServer(t, strings.Repeat("f", 64))
	defer server.Close()

	imageURL, err := url.Parse(server.URL + "/release/image.img")
	if err != nil {
		t.Fatal(err)
	}
	err = verifyRemoteSource(context.Background(), newHTTPClient(), imageURL, testDigest)
	if err == nil || !strings.Contains(err.Error(), "exact shipped filename and digest") {
		t.Fatalf("verifyRemoteSource() error = %v", err)
	}
}

func TestVerifyRemoteSourceSuccessUsesOneByteRange(t *testing.T) {
	var imageRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/release/image.img":
			imageRequests++
			if got := r.Header.Get("Range"); got != "bytes=0-0" {
				t.Errorf("Range = %q", got)
			}
			w.Header().Set("Content-Range", "bytes 0-0/999999999")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte{0})
		case "/release/SHA256SUMS":
			_, _ = fmt.Fprintf(w, "%s *image.img\n", testDigest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	imageURL, err := url.Parse(server.URL + "/release/image.img")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyRemoteSource(context.Background(), newHTTPClient(), imageURL, testDigest); err != nil {
		t.Fatalf("verifyRemoteSource() error = %v", err)
	}
	if imageRequests != 1 {
		t.Fatalf("image requests = %d, want 1", imageRequests)
	}
}

func newImageServer(t *testing.T, manifestDigest string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/release/image.img":
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte{0})
		case "/release/SHA256SUMS":
			_, _ = fmt.Fprintf(w, "%s *image.img\n", manifestDigest)
		default:
			http.NotFound(w, r)
		}
	}))
}
