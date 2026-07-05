package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func parseArtifactPublishOptions(args []string, stderr io.Writer) (artifactPublishOptions, error) {
	fs := newFlagSet("artifacts publish", stderr)
	dir := fs.String("dir", os.Getenv("CRABBOX_ARTIFACTS_DIR"), "artifact bundle directory")
	storage := fs.String("storage", firstNonBlank(os.Getenv("CRABBOX_ARTIFACTS_STORAGE"), "auto"), "storage backend: auto, broker, local, s3, cloudflare, or r2")
	bucket := fs.String("bucket", os.Getenv("CRABBOX_ARTIFACTS_BUCKET"), "storage bucket")
	prefix := fs.String("prefix", os.Getenv("CRABBOX_ARTIFACTS_PREFIX"), "object key prefix")
	baseURL := fs.String("base-url", os.Getenv("CRABBOX_ARTIFACTS_BASE_URL"), "public base URL for inline-ready asset links")
	pr := fs.Int("pr", 0, "GitHub pull request number to comment on")
	repo := fs.String("repo", "", "GitHub repository slug for gh, e.g. openclaw/crabbox")
	template := fs.String("template", "openclaw", "comment template: openclaw or mantis")
	summary := fs.String("summary", "", "summary text")
	summaryFile := fs.String("summary-file", "", "summary markdown file")
	region := fs.String("region", firstNonBlank(os.Getenv("CRABBOX_ARTIFACTS_AWS_REGION"), os.Getenv("AWS_REGION"), os.Getenv("AWS_DEFAULT_REGION")), "AWS region for S3 URLs/CLI")
	profile := fs.String("profile", firstNonBlank(os.Getenv("CRABBOX_ARTIFACTS_AWS_PROFILE"), os.Getenv("AWS_PROFILE")), "AWS profile for S3 CLI")
	endpointURL := fs.String("endpoint-url", os.Getenv("CRABBOX_ARTIFACTS_ENDPOINT_URL"), "S3-compatible endpoint URL")
	acl := fs.String("acl", os.Getenv("CRABBOX_ARTIFACTS_S3_ACL"), "optional S3 ACL, e.g. public-read")
	presign := fs.Bool("presign", envBool("CRABBOX_ARTIFACTS_PRESIGN"), "use aws s3 presign URLs after upload")
	expires := fs.Duration("expires", envDuration("CRABBOX_ARTIFACTS_EXPIRES", 7*24*time.Hour), "presigned URL lifetime")
	dryRun := fs.Bool("dry-run", false, "print upload/comment commands without running them")
	noComment := fs.Bool("no-comment", false, "skip GitHub PR comment")
	skipManifest := fs.Bool("skip-manifest", false, "skip artifact-manifest.json")
	noManifest := fs.Bool("no-manifest", false, "alias for --skip-manifest")
	if err := parseFlags(fs, args); err != nil {
		return artifactPublishOptions{}, err
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		explicit[f.Name] = true
	})
	opts := artifactPublishOptions{
		Directory:   strings.TrimSpace(*dir),
		Storage:     normalizeArtifactStorage(*storage),
		Bucket:      strings.TrimSpace(*bucket),
		Prefix:      strings.Trim(strings.TrimSpace(*prefix), "/"),
		BaseURL:     strings.TrimRight(strings.TrimSpace(*baseURL), "/"),
		PR:          *pr,
		Repo:        strings.TrimSpace(*repo),
		Template:    strings.TrimSpace(*template),
		Summary:     *summary,
		SummaryFile: strings.TrimSpace(*summaryFile),
		Region:      strings.TrimSpace(*region),
		Profile:     strings.TrimSpace(*profile),
		EndpointURL: strings.TrimSpace(*endpointURL),
		ACL:         strings.TrimSpace(*acl),
		Presign:     *presign,
		Expires:     *expires,
		DryRun:      *dryRun,
		NoComment:   *noComment,
		NoManifest:  *skipManifest || *noManifest,
	}
	if opts.Storage == "r2" {
		if !explicit["profile"] {
			opts.Profile = firstNonBlank(os.Getenv("CRABBOX_ARTIFACTS_R2_AWS_PROFILE"), opts.Profile)
		}
		if !explicit["endpoint-url"] {
			opts.EndpointURL = firstNonBlank(os.Getenv("CRABBOX_ARTIFACTS_R2_ENDPOINT_URL"), opts.EndpointURL)
		}
		if !explicit["region"] {
			opts.Region = firstNonBlank(os.Getenv("CRABBOX_ARTIFACTS_R2_AWS_REGION"), "auto")
		}
	}
	if opts.Directory == "" {
		return artifactPublishOptions{}, exit(2, "artifacts publish requires --dir")
	}
	if opts.PR < 0 {
		return artifactPublishOptions{}, exit(2, "artifacts publish --pr must be positive")
	}
	if opts.Expires <= 0 {
		return artifactPublishOptions{}, exit(2, "artifacts publish --expires must be positive")
	}
	switch opts.Storage {
	case "auto", "broker", "local":
	case "s3", "cloudflare", "r2":
		if opts.Bucket == "" {
			return artifactPublishOptions{}, exit(2, "artifacts publish --storage %s requires --bucket", opts.Storage)
		}
	default:
		return artifactPublishOptions{}, exit(2, "artifacts publish --storage must be auto, broker, local, s3, cloudflare, or r2")
	}
	if opts.Storage == "r2" && opts.EndpointURL == "" {
		return artifactPublishOptions{}, exit(2, "artifacts publish --storage r2 requires --endpoint-url or CRABBOX_ARTIFACTS_R2_ENDPOINT_URL")
	}
	if (opts.Storage == "cloudflare" || opts.Storage == "r2") && opts.PR > 0 && !opts.NoComment && opts.BaseURL == "" {
		return artifactPublishOptions{}, exit(2, "artifacts publish --storage %s --pr requires --base-url for inline-ready R2 asset links", opts.Storage)
	}
	return opts, nil
}

func normalizeArtifactStorage(storage string) string {
	switch strings.ToLower(strings.TrimSpace(storage)) {
	case "", "auto":
		return "auto"
	case "broker", "coordinator":
		return "broker"
	case "local":
		return "local"
	case "s3", "aws", "aws-s3":
		return "s3"
	case "r2", "cloudflare-r2":
		return "r2"
	case "cloudflare", "cf":
		return "cloudflare"
	default:
		return strings.ToLower(strings.TrimSpace(storage))
	}
}

func listArtifactBundleFiles(dir string) ([]artifactFile, error) {
	var files []artifactFile
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifact bundle contains symlink: %s", filepath.ToSlash(rel))
		}
		name := entry.Name()
		generatedOutput := name == "published-artifacts.md" || name == artifactManifestFilename
		reservedOutputPath := rel == "published-artifacts.md" || rel == artifactManifestFilename
		if reservedOutputPath && info.IsDir() {
			return fmt.Errorf("artifact bundle contains directory at reserved output path: %s", filepath.ToSlash(rel))
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifact bundle contains non-regular file: %s", filepath.ToSlash(rel))
		}
		if generatedOutput {
			return nil
		}
		files = append(files, artifactFile{Kind: artifactKindForPath(path), Name: filepath.ToSlash(rel), Path: path})
		return nil
	})
	if err != nil {
		return nil, exit(2, "read artifact directory: %v", err)
	}
	sortArtifactFiles(files)
	return files, nil
}

func publishArtifactFiles(ctx context.Context, opts artifactPublishOptions, files []artifactFile) ([]artifactFile, error) {
	published := make([]artifactFile, 0, len(files))
	for _, file := range files {
		out := file
		key := artifactObjectKey(opts.Prefix, file.Name)
		switch opts.Storage {
		case "local":
			out.Key = key
			if opts.BaseURL != "" {
				out.URL = joinURLPath(opts.BaseURL, file.Name)
			}
		case "s3", "r2":
			url, err := uploadArtifactS3(ctx, opts, file.Path, key)
			if err != nil {
				return nil, err
			}
			out.URL = url
			out.Key = key
		case "cloudflare":
			url, err := uploadArtifactCloudflare(ctx, opts, file.Path, key)
			if err != nil {
				return nil, err
			}
			out.URL = url
			out.Key = key
		}
		published = append(published, out)
	}
	return published, nil
}

func publishArtifactFilesBroker(ctx context.Context, coord *CoordinatorClient, opts artifactPublishOptions, files []artifactFile) ([]artifactFile, error) {
	if !coord.hasConfiguredAuth() {
		return nil, exit(2, "artifacts publish --storage broker requires a configured coordinator; run `crabbox login --url <broker-url>` or pass --storage local|s3|r2")
	}
	ensureArtifactPublishPrefix(&opts)
	input := CoordinatorArtifactUploadRequest{
		Prefix: opts.Prefix,
		Files:  make([]CoordinatorArtifactUploadInput, 0, len(files)),
	}
	for _, file := range files {
		info, err := os.Stat(file.Path)
		if err != nil {
			return nil, exit(2, "stat artifact %s: %v", file.Name, err)
		}
		hash, err := fileSHA256(file.Path)
		if err != nil {
			return nil, err
		}
		input.Files = append(input.Files, CoordinatorArtifactUploadInput{
			Name:        file.Name,
			Size:        info.Size(),
			ContentType: artifactContentType(file.Path),
			SHA256:      hash,
		})
	}
	grants, err := coord.CreateArtifactUploads(ctx, input)
	if err != nil {
		return nil, err
	}
	byName := map[string]CoordinatorArtifactUploadGrant{}
	for _, grant := range grants.Files {
		byName[grant.Name] = grant
	}
	published := make([]artifactFile, 0, len(files))
	for _, file := range files {
		grant, ok := byName[file.Name]
		if !ok {
			return nil, exit(2, "artifact broker did not return an upload grant for %s", file.Name)
		}
		if !opts.DryRun {
			if err := uploadArtifactGrant(ctx, file.Path, grant); err != nil {
				return nil, err
			}
		}
		out := file
		out.URL = grant.URL
		out.Key = grant.Key
		published = append(published, out)
	}
	return published, nil
}

func ensureArtifactPublishPrefix(opts *artifactPublishOptions) {
	if opts == nil || opts.Prefix != "" || opts.Storage == "local" {
		return
	}
	opts.Prefix = defaultArtifactPublishPrefix(*opts, time.Now())
}

func defaultArtifactPublishPrefix(opts artifactPublishOptions, now time.Time) string {
	scope := "publish"
	if opts.PR > 0 {
		scope = "pr-" + strconv.Itoa(opts.PR)
	}
	bundle := normalizeLeaseSlug(filepath.Base(filepath.Clean(opts.Directory)))
	if bundle == "" || bundle == "." {
		bundle = "bundle"
	}
	stamp := now.UTC().Format("20060102-150405") + "-" + fmt.Sprintf("%09d", now.UTC().Nanosecond())
	return strings.Join([]string{scope, bundle, stamp}, "/")
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", exit(2, "open artifact %s: %v", path, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", exit(2, "hash artifact %s: %v", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func uploadArtifactGrant(ctx context.Context, path string, grant CoordinatorArtifactUploadGrant) error {
	if grant.Upload.URL == "" {
		return exit(2, "artifact broker returned an empty upload URL for %s", grant.Name)
	}
	method := strings.ToUpper(strings.TrimSpace(grant.Upload.Method))
	if method == "" {
		method = http.MethodPut
	}
	file, err := os.Open(path)
	if err != nil {
		return exit(2, "open artifact %s: %v", grant.Name, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return exit(2, "stat artifact %s: %v", grant.Name, err)
	}
	contentLength := info.Size()
	if expected, ok, err := grantContentLength(grant.Upload.Headers); err != nil {
		return exit(2, "artifact broker returned an invalid content-length for %s: %v", grant.Name, err)
	} else if ok {
		if expected != info.Size() {
			return exit(2, "artifact %s size changed after broker grant: got %d bytes, expected %d", grant.Name, info.Size(), expected)
		}
		contentLength = expected
	}
	// Keep redirect replays bound to the already-open file descriptor so a path
	// replacement cannot change the bytes after the broker signs their length.
	requestBody := func() io.ReadCloser {
		return io.NopCloser(io.NewSectionReader(file, 0, contentLength))
	}
	req, err := http.NewRequestWithContext(ctx, method, grant.Upload.URL, requestBody())
	if err != nil {
		return exit(2, "create artifact upload request for %s: %v", grant.Name, err)
	}
	req.ContentLength = contentLength
	req.GetBody = func() (io.ReadCloser, error) {
		return requestBody(), nil
	}
	for key, value := range grant.Upload.Headers {
		if strings.EqualFold(strings.TrimSpace(key), "content-length") {
			continue
		}
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			req.Header.Set(key, value)
		}
	}
	resp, err := artifactHTTPClient(req.URL).Do(req)
	if err != nil {
		return exit(2, "upload artifact %s: %v", grant.Name, artifactRequestError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return exit(2, "upload artifact %s: http %d: %s", grant.Name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func grantContentLength(headers map[string]string) (int64, bool, error) {
	for key, value := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), "content-length") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || n < 0 {
			if err == nil {
				err = fmt.Errorf("negative value")
			}
			return 0, true, err
		}
		return n, true, nil
	}
	return 0, false, nil
}

func uploadArtifactS3(ctx context.Context, opts artifactPublishOptions, path, key string) (string, error) {
	dest := "s3://" + opts.Bucket + "/" + key
	args := awsBaseArgs(opts)
	args = append(args, "s3", "cp", path, dest, "--content-type", artifactContentType(path))
	if opts.ACL != "" {
		args = append(args, "--acl", opts.ACL)
	}
	if opts.DryRun {
		return artifactS3URL(opts, key), nil
	}
	if _, err := exec.LookPath("aws"); err != nil {
		return "", exit(2, "aws CLI is required for artifacts publish --storage s3: %v", err)
	}
	if out, err := commandOutput(ctx, "aws", args...); err != nil {
		return "", exit(2, "aws s3 upload failed: %v: %s", err, tailForError(out))
	}
	if opts.Presign && opts.BaseURL == "" {
		presignArgs := awsBaseArgs(opts)
		presignArgs = append(presignArgs, "s3", "presign", dest, "--expires-in", fmt.Sprintf("%.0f", opts.Expires.Seconds()))
		out, err := commandOutput(ctx, "aws", presignArgs...)
		if err != nil {
			return "", exit(2, "aws s3 presign failed: %v: %s", err, tailForError(out))
		}
		return strings.TrimSpace(out), nil
	}
	return artifactS3URL(opts, key), nil
}

func awsBaseArgs(opts artifactPublishOptions) []string {
	var args []string
	if opts.Profile != "" {
		args = append(args, "--profile", opts.Profile)
	}
	if opts.Region != "" {
		args = append(args, "--region", opts.Region)
	}
	if opts.EndpointURL != "" {
		args = append(args, "--endpoint-url", opts.EndpointURL)
	}
	return args
}

func uploadArtifactCloudflare(ctx context.Context, opts artifactPublishOptions, path, key string) (string, error) {
	if opts.DryRun {
		return artifactCloudflareURL(opts, key), nil
	}
	if _, err := exec.LookPath("wrangler"); err != nil {
		return "", exit(2, "wrangler CLI is required for artifacts publish --storage cloudflare: %v", err)
	}
	out, err := commandOutputWithEnv(ctx, artifactCloudflareEnv(), "wrangler", "r2", "object", "put", opts.Bucket+"/"+key, "--file", path, "--content-type", artifactContentType(path), "--remote")
	if err != nil {
		return "", exit(2, "wrangler r2 upload failed: %v: %s", err, tailForError(out))
	}
	return artifactCloudflareURL(opts, key), nil
}

func artifactCloudflareEnv() []string {
	env := os.Environ()
	if token := firstNonBlank(
		os.Getenv("CRABBOX_ARTIFACTS_CLOUDFLARE_API_TOKEN"),
		os.Getenv("CLOUDFLARE_API_TOKEN"),
	); token != "" {
		env = append(env, "CLOUDFLARE_API_TOKEN="+token)
	}
	if accountID := firstNonBlank(
		os.Getenv("CRABBOX_ARTIFACTS_CLOUDFLARE_ACCOUNT_ID"),
		os.Getenv("CLOUDFLARE_ACCOUNT_ID"),
	); accountID != "" {
		env = append(env, "CLOUDFLARE_ACCOUNT_ID="+accountID)
	}
	return env
}

func artifactS3URL(opts artifactPublishOptions, key string) string {
	if opts.BaseURL != "" {
		return joinURLPath(opts.BaseURL, key)
	}
	if opts.EndpointURL != "" {
		return joinURLPath(opts.EndpointURL, opts.Bucket+"/"+key)
	}
	escapedKey := pathEscapeSegments(key)
	if opts.Region != "" {
		return "https://" + opts.Bucket + ".s3." + opts.Region + ".amazonaws.com/" + escapedKey
	}
	return "https://" + opts.Bucket + ".s3.amazonaws.com/" + escapedKey
}

func artifactCloudflareURL(opts artifactPublishOptions, key string) string {
	if opts.BaseURL != "" {
		return joinURLPath(opts.BaseURL, key)
	}
	return "r2://" + opts.Bucket + "/" + key
}

func artifactObjectKey(prefix, name string) string {
	name = strings.TrimLeft(filepath.ToSlash(name), "/")
	if strings.TrimSpace(prefix) == "" {
		return name
	}
	return strings.Trim(strings.TrimSpace(prefix), "/") + "/" + name
}

func artifactContentType(path string) string {
	if contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); contentType != "" {
		return contentType
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md":
		return "text/markdown; charset=utf-8"
	case ".log":
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func artifactKindForPath(path string) string {
	name := strings.ToLower(filepath.Base(path))
	ext := strings.ToLower(filepath.Ext(path))
	switch {
	case ext == ".gif":
		return "gif"
	case artifactExtIsVideo(ext):
		return "video"
	case strings.Contains(name, "contact"):
		return "contact-sheet"
	case (artifactExtIsImage(ext) || ext == "") && (strings.Contains(name, "screenshot") || strings.Contains(name, "before") || strings.Contains(name, "after")):
		return "screenshot"
	case strings.Contains(name, "diagnostic"):
		return "diagnostics"
	case strings.Contains(name, "doctor"):
		return "doctor"
	case strings.Contains(name, "webvnc"):
		return "webvnc-status"
	case strings.Contains(name, "metadata"):
		return "metadata"
	case strings.Contains(name, "log") || ext == ".txt":
		return "logs"
	default:
		return strings.TrimPrefix(ext, ".")
	}
}

func artifactExtIsVideo(ext string) bool {
	switch strings.ToLower(ext) {
	case ".mp4", ".mov", ".webm":
		return true
	default:
		return false
	}
}

func artifactExtIsImage(ext string) bool {
	switch strings.ToLower(ext) {
	case ".png", ".jpg", ".jpeg", ".gif":
		return true
	default:
		return false
	}
}

func artifactTemplateMarkdown(kind, summary, before, after string, files []artifactFile) string {
	title := "OpenClaw QA Artifacts"
	if strings.EqualFold(strings.TrimSpace(kind), "mantis") {
		title = "Mantis QA Artifacts"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", title)
	if strings.TrimSpace(summary) != "" {
		fmt.Fprintf(&b, "### Summary\n%s\n\n", strings.TrimSpace(summary))
	}
	if before != "" || after != "" {
		b.WriteString("### Before / After\n\n")
		if before != "" {
			fmt.Fprintf(&b, "**Before**\n\n%s\n\n", artifactMarkdownForAsset("before", before))
		}
		if after != "" {
			fmt.Fprintf(&b, "**After**\n\n%s\n\n", artifactMarkdownForAsset("after", after))
		}
	}
	if len(files) > 0 {
		b.WriteString("### Evidence\n\n")
		for _, file := range files {
			location := firstNonBlank(file.URL, file.Path)
			if location == "" {
				continue
			}
			fmt.Fprintf(&b, "- %s: %s\n", file.Kind, artifactMarkdownForFile(file, location))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func artifactMarkdownForFile(file artifactFile, location string) string {
	if artifactFileIsInlineImage(file, location) {
		return fmt.Sprintf("![%s](%s)", file.Name, location)
	}
	return fmt.Sprintf("[%s](%s)", file.Name, location)
}

func artifactMarkdownForAsset(label, location string) string {
	if artifactLocationHasImageExtension(location) {
		return fmt.Sprintf("![%s](%s)", label, location)
	}
	return fmt.Sprintf("[%s](%s)", label, location)
}

func artifactFileIsInlineImage(file artifactFile, location string) bool {
	switch strings.ToLower(strings.TrimSpace(file.Kind)) {
	case "gif", "screenshot", "image":
		return true
	}
	return artifactLocationHasImageExtension(firstNonBlank(file.Name, location))
}

func artifactLocationHasImageExtension(location string) bool {
	location = strings.TrimSpace(location)
	if parsed, err := url.Parse(location); err == nil && parsed.Path != "" {
		location = parsed.Path
	} else {
		location = strings.SplitN(location, "?", 2)[0]
		location = strings.SplitN(location, "#", 2)[0]
	}
	switch strings.ToLower(filepath.Ext(location)) {
	case ".png", ".jpg", ".jpeg", ".gif":
		return true
	default:
		return false
	}
}

func summaryText(summary, summaryFile string) (string, error) {
	if strings.TrimSpace(summaryFile) == "" {
		return strings.TrimSpace(summary), nil
	}
	data, err := os.ReadFile(summaryFile)
	if err != nil {
		return "", exit(2, "read summary file: %v", err)
	}
	if strings.TrimSpace(summary) != "" {
		return strings.TrimSpace(summary) + "\n\n" + strings.TrimSpace(string(data)), nil
	}
	return strings.TrimSpace(string(data)), nil
}

func postGitHubPRComment(ctx context.Context, pr int, repo, bodyPath string) error {
	args := []string{"issue", "comment", strconv.Itoa(pr), "--body-file", bodyPath}
	if strings.TrimSpace(repo) != "" {
		args = append(args, "--repo", strings.TrimSpace(repo))
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return exit(2, "gh CLI is required for artifacts publish --pr: %v", err)
	}
	if out, err := commandOutput(ctx, "gh", args...); err != nil {
		return exit(2, "gh issue comment failed: %v: %s", err, tailForError(out))
	}
	return nil
}

func sortArtifactFiles(files []artifactFile) {
	sort.Slice(files, func(i, j int) bool {
		if files[i].Kind == files[j].Kind {
			return files[i].Name < files[j].Name
		}
		return files[i].Kind < files[j].Kind
	})
}

func joinURLPath(base, rel string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	rel = strings.TrimLeft(filepath.ToSlash(rel), "/")
	if base == "" {
		return rel
	}
	return base + "/" + pathEscapeSegments(rel)
}

func pathEscapeSegments(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func envBool(name string) bool {
	value := strings.TrimSpace(os.Getenv(name))
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return duration
}
