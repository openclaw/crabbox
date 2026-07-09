package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	artifactManifestFilename = "artifact-manifest.json"
	maxPulledArtifactBytes   = int64(1024 * 1024 * 1024)
)

var errArtifactCrossOriginRedirect = errors.New("refused cross-origin artifact redirect")

type artifactManifest struct {
	SchemaVersion int                    `json:"schemaVersion"`
	GeneratedAt   string                 `json:"generatedAt"`
	Storage       artifactManifestStore  `json:"storage"`
	Files         []artifactManifestFile `json:"files"`
}

type artifactManifestStore struct {
	Backend string `json:"backend"`
	Bucket  string `json:"bucket,omitempty"`
	Prefix  string `json:"prefix,omitempty"`
	BaseURL string `json:"baseUrl,omitempty"`
}

type artifactManifestFile struct {
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	Path         string `json:"path,omitempty"`
	URL          string `json:"url,omitempty"`
	Key          string `json:"key,omitempty"`
	ContentType  string `json:"contentType,omitempty"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256,omitempty"`
	ExpiresAt    string `json:"expiresAt,omitempty"`
	AccessPolicy string `json:"accessPolicy,omitempty"`
}

type artifactPullResult struct {
	Directory string               `json:"directory"`
	Files     []artifactPulledFile `json:"files"`
}

type artifactPulledFile struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	URL         string `json:"url,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256,omitempty"`
}

func writeArtifactManifest(opts artifactPublishOptions, files []artifactFile) (string, error) {
	manifest := artifactManifest{
		SchemaVersion: 1,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		Storage: artifactManifestStore{
			Backend: opts.Storage,
			Bucket:  opts.Bucket,
			Prefix:  opts.Prefix,
			BaseURL: opts.BaseURL,
		},
		Files: make([]artifactManifestFile, 0, len(files)),
	}
	for _, file := range files {
		info, err := os.Stat(file.Path)
		if err != nil {
			return "", exit(2, "stat artifact %s: %v", file.Name, err)
		}
		hash, err := fileSHA256(file.Path)
		if err != nil {
			return "", err
		}
		manifest.Files = append(manifest.Files, artifactManifestFile{
			Kind:         file.Kind,
			Name:         file.Name,
			Path:         file.Name,
			URL:          file.URL,
			Key:          file.Key,
			ContentType:  artifactContentType(file.Path),
			Size:         info.Size(),
			SHA256:       hash,
			ExpiresAt:    artifactURLExpiresAt(file.URL),
			AccessPolicy: artifactAccessPolicy(file.URL, opts.Storage),
		})
	}
	path := filepath.Join(opts.Directory, artifactManifestFilename)
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", exit(2, "encode artifact manifest: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", exit(2, "write artifact manifest: %v", err)
	}
	return path, nil
}

func readArtifactManifestRef(ctx context.Context, ref string) (artifactManifest, string, error) {
	path := strings.TrimSpace(ref)
	if path == "" {
		path = artifactManifestFilename
	}
	if isHTTPArtifactRef(path) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		if err != nil {
			return artifactManifest{}, path, exit(2, "create artifact manifest request: %v", err)
		}
		resp, err := artifactHTTPClient(req.URL).Do(req)
		if err != nil {
			return artifactManifest{}, path, exit(2, "download artifact manifest: %v", artifactRequestError(err))
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return artifactManifest{}, path, exit(2, "download artifact manifest: http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
		if err != nil {
			return artifactManifest{}, path, exit(2, "read artifact manifest response: %v", err)
		}
		manifest, err := decodeArtifactManifest(data, path)
		return manifest, path, err
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, artifactManifestFilename)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return artifactManifest{}, path, exit(2, "read artifact manifest: %v", err)
	}
	manifest, err := decodeArtifactManifest(data, path)
	return manifest, path, err
}

func decodeArtifactManifest(data []byte, path string) (artifactManifest, error) {
	var manifest artifactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return artifactManifest{}, exit(2, "parse artifact manifest %s: %v", path, err)
	}
	if manifest.SchemaVersion != 1 {
		return artifactManifest{}, exit(2, "unsupported artifact manifest schemaVersion=%d", manifest.SchemaVersion)
	}
	return manifest, nil
}

func pullArtifactManifest(ctx context.Context, ref, output string, overwrite bool) (artifactPullResult, error) {
	manifest, manifestPath, err := readArtifactManifestRef(ctx, ref)
	if err != nil {
		return artifactPullResult{}, err
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		return artifactPullResult{}, exit(2, "create output directory: %v", err)
	}
	result := artifactPullResult{Directory: output, Files: make([]artifactPulledFile, 0, len(manifest.Files))}
	for _, file := range manifest.Files {
		if strings.TrimSpace(file.Name) == "" {
			return artifactPullResult{}, exit(2, "artifact manifest contains an unnamed file")
		}
		if file.Size < 0 {
			return artifactPullResult{}, exit(2, "artifact size for %s is invalid: %d", file.Name, file.Size)
		}
		outPath, err := safeArtifactOutputPath(output, file.Name)
		if err != nil {
			return artifactPullResult{}, err
		}
		if !overwrite {
			if _, err := os.Stat(outPath); err == nil {
				return artifactPullResult{}, exit(2, "artifact output already exists: %s", outPath)
			}
		}
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return artifactPullResult{}, exit(2, "create artifact output directory: %v", err)
		}
		if err := rejectSymlinkedArtifactOutputParents(output, file.Name); err != nil {
			return artifactPullResult{}, err
		}
		tempPath, err := createArtifactOutputTemp(outPath)
		if err != nil {
			return artifactPullResult{}, err
		}
		contentType, size, hash, err := pullArtifactFile(ctx, manifestPath, file, tempPath)
		if err != nil {
			_ = os.Remove(tempPath)
			return artifactPullResult{}, err
		}
		if file.SHA256 != "" && !strings.EqualFold(file.SHA256, hash) {
			_ = os.Remove(tempPath)
			return artifactPullResult{}, exit(2, "artifact hash mismatch for %s: got %s, want %s", file.Name, hash, file.SHA256)
		}
		if file.Size > 0 && file.Size != size {
			_ = os.Remove(tempPath)
			return artifactPullResult{}, exit(2, "artifact size mismatch for %s: got %d, want %d", file.Name, size, file.Size)
		}
		if err := installPulledArtifact(tempPath, outPath, overwrite); err != nil {
			_ = os.Remove(tempPath)
			return artifactPullResult{}, err
		}
		result.Files = append(result.Files, artifactPulledFile{
			Name:        file.Name,
			Path:        outPath,
			URL:         file.URL,
			ContentType: contentType,
			Size:        size,
			SHA256:      hash,
		})
	}
	return result, nil
}

func createArtifactOutputTemp(outPath string) (string, error) {
	temp, err := os.CreateTemp(filepath.Dir(outPath), "."+filepath.Base(outPath)+".tmp-*")
	if err != nil {
		return "", exit(2, "create temporary artifact output: %v", err)
	}
	path := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(path)
		return "", exit(2, "close temporary artifact output: %v", err)
	}
	return path, nil
}

func installPulledArtifact(tempPath, outPath string, overwrite bool) error {
	if overwrite {
		if err := os.Rename(tempPath, outPath); err == nil {
			return nil
		}
		if err := os.Remove(outPath); err != nil && !os.IsNotExist(err) {
			return exit(2, "replace artifact output %s: %v", outPath, err)
		}
	} else if _, err := os.Stat(outPath); err == nil {
		return exit(2, "artifact output already exists: %s", outPath)
	}
	if err := os.Rename(tempPath, outPath); err != nil {
		return exit(2, "install artifact output %s: %v", outPath, err)
	}
	return nil
}

func pullArtifactFile(ctx context.Context, manifestPath string, file artifactManifestFile, outPath string) (string, int64, string, error) {
	if isHTTPArtifactRef(file.URL) {
		return downloadArtifactURL(ctx, file, outPath)
	}
	if strings.TrimSpace(file.URL) != "" && isHTTPArtifactRef(manifestPath) {
		return "", 0, "", exit(2, "remote artifact manifest entry %s has non-downloadable url: %s", file.Name, file.URL)
	}
	if strings.TrimSpace(file.Path) == "" {
		return "", 0, "", exit(2, "artifact %s has no url or path", file.Name)
	}
	if isHTTPArtifactRef(manifestPath) {
		return "", 0, "", exit(2, "remote artifact manifest entry %s requires url", file.Name)
	}
	sourcePath, err := localArtifactSourcePath(manifestPath, file.Path)
	if err != nil {
		return "", 0, "", err
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return "", 0, "", exit(2, "open artifact %s: %v", file.Name, err)
	}
	defer source.Close()
	dest, err := os.Create(outPath)
	if err != nil {
		return "", 0, "", exit(2, "create artifact %s: %v", outPath, err)
	}
	defer dest.Close()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(dest, hash), source)
	if err != nil {
		return "", 0, "", exit(2, "copy artifact %s: %v", file.Name, err)
	}
	return firstNonBlank(file.ContentType, artifactContentType(sourcePath)), written, hex.EncodeToString(hash.Sum(nil)), nil
}

func localArtifactSourcePath(manifestPath, artifactPath string) (string, error) {
	cleanPath := filepath.Clean(filepath.FromSlash(strings.TrimSpace(artifactPath)))
	if cleanPath == "." || cleanPath == "" || filepath.IsAbs(cleanPath) || cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) {
		return "", exit(2, "invalid artifact source path: %s", artifactPath)
	}
	baseDir := filepath.Dir(manifestPath)
	baseAbs, err := filepath.Abs(baseDir)
	if err != nil {
		return "", exit(2, "resolve artifact manifest directory: %v", err)
	}
	sourceAbs, err := filepath.Abs(filepath.Join(baseDir, cleanPath))
	if err != nil {
		return "", exit(2, "resolve artifact source path: %v", err)
	}
	rel, err := filepath.Rel(baseAbs, sourceAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", exit(2, "invalid artifact source path: %s", artifactPath)
	}
	if err := rejectSymlinkedArtifactSourcePath(baseAbs, cleanPath); err != nil {
		return "", err
	}
	return sourceAbs, nil
}

func rejectSymlinkedArtifactSourcePath(baseAbs, cleanPath string) error {
	current := baseAbs
	for _, part := range strings.Split(cleanPath, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return exit(2, "inspect artifact source path %s: %v", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return exit(2, "artifact source path uses symlink: %s", cleanPath)
		}
	}
	return nil
}

func downloadArtifactURL(ctx context.Context, file artifactManifestFile, outPath string) (string, int64, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, file.URL, nil)
	if err != nil {
		return "", 0, "", exit(2, "create artifact download request for %s: %v", file.Name, err)
	}
	resp, err := artifactHTTPClient(req.URL).Do(req)
	if err != nil {
		return "", 0, "", exit(2, "download artifact %s: %v", file.Name, artifactRequestError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", 0, "", exit(2, "download artifact %s: http %d: %s", file.Name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	limit := artifactDownloadLimit(file)
	if resp.ContentLength > limit {
		return "", 0, "", exit(2, "artifact %s is too large: content-length %d exceeds limit %d", file.Name, resp.ContentLength, limit)
	}
	dest, err := os.Create(outPath)
	if err != nil {
		return "", 0, "", exit(2, "create artifact %s: %v", outPath, err)
	}
	defer dest.Close()
	hash := sha256.New()
	written, exceeded, err := copyArtifactResponse(io.MultiWriter(dest, hash), resp.Body, limit)
	if err != nil {
		return "", 0, "", exit(2, "write artifact %s: %v", file.Name, err)
	}
	if exceeded {
		return "", 0, "", exit(2, "artifact %s is too large: response exceeds limit %d", file.Name, limit)
	}
	return resp.Header.Get("content-type"), written, hex.EncodeToString(hash.Sum(nil)), nil
}

func artifactHTTPClient(origin *url.URL) *http.Client {
	return redirectCheckedHTTPClient(nil, func(req *http.Request) error {
		if !sameHTTPOrigin(origin, req.URL) {
			return errArtifactCrossOriginRedirect
		}
		return nil
	})
}

func artifactRequestError(err error) error {
	if errors.Is(err, errArtifactCrossOriginRedirect) {
		return errArtifactCrossOriginRedirect
	}
	return err
}

func artifactDownloadLimit(file artifactManifestFile) int64 {
	if file.Size > 0 && file.Size < maxPulledArtifactBytes {
		return file.Size
	}
	return maxPulledArtifactBytes
}

func copyArtifactResponse(dest io.Writer, body io.Reader, limit int64) (int64, bool, error) {
	written, err := io.Copy(dest, io.LimitReader(body, limit))
	if err != nil {
		return written, false, err
	}
	var extra [1]byte
	n, err := body.Read(extra[:])
	if n > 0 {
		return written, true, nil
	}
	if err != nil && err != io.EOF {
		return written, false, err
	}
	return written, false, nil
}

func safeArtifactOutputPath(root, name string) (string, error) {
	cleanName := filepath.Clean(filepath.FromSlash(strings.TrimLeft(name, "/")))
	if cleanName == "." || strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) || cleanName == ".." || filepath.IsAbs(cleanName) {
		return "", exit(2, "invalid artifact output path: %s", name)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", exit(2, "resolve output directory: %v", err)
	}
	outAbs, err := filepath.Abs(filepath.Join(root, cleanName))
	if err != nil {
		return "", exit(2, "resolve artifact output path: %v", err)
	}
	rel, err := filepath.Rel(rootAbs, outAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", exit(2, "invalid artifact output path: %s", name)
	}
	if err := rejectSymlinkedArtifactOutputParents(root, cleanName); err != nil {
		return "", err
	}
	return outAbs, nil
}

func rejectSymlinkedArtifactOutputParents(root, name string) error {
	cleanName := filepath.Clean(filepath.FromSlash(strings.TrimLeft(name, "/")))
	if cleanName == "." || cleanName == ".." || filepath.IsAbs(cleanName) || strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) {
		return exit(2, "invalid artifact output path: %s", name)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return exit(2, "resolve output directory: %v", err)
	}
	current := rootAbs
	parts := strings.Split(cleanName, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return exit(2, "inspect artifact output parent %s: %v", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return exit(2, "artifact output path uses symlinked parent: %s", name)
		}
		if !info.IsDir() {
			return exit(2, "artifact output parent is not a directory: %s", current)
		}
	}
	return nil
}

func artifactAccessPolicy(rawURL, storage string) string {
	if strings.TrimSpace(rawURL) == "" {
		return "local"
	}
	parsed, err := url.Parse(rawURL)
	if err == nil {
		query := parsed.Query()
		if query.Get("X-Amz-Signature") != "" || query.Get("X-Amz-Credential") != "" {
			return "signed-url"
		}
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(rawURL)), "http://") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(rawURL)), "https://") {
		return "public"
	}
	switch storage {
	case "broker", "s3", "r2", "cloudflare":
		return "public"
	default:
		return storage
	}
}

func artifactURLExpiresAt(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	expires := parsed.Query().Get("X-Amz-Expires")
	date := parsed.Query().Get("X-Amz-Date")
	if expires == "" || date == "" {
		return ""
	}
	seconds, err := strconv.Atoi(expires)
	if err != nil || seconds <= 0 {
		return ""
	}
	start, err := time.Parse("20060102T150405Z", date)
	if err != nil {
		return ""
	}
	return start.Add(time.Duration(seconds) * time.Second).UTC().Format(time.RFC3339)
}

func isHTTPArtifactRef(ref string) bool {
	lower := strings.ToLower(strings.TrimSpace(ref))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}
