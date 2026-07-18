package cli

import (
	"bytes"
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
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, exit(2, "read artifact directory: %v", err)
	}
	defer root.Close()
	return listArtifactBundleFilesRoot(root, dir)
}

func listArtifactBundleFilesRoot(root *os.Root, dir string) ([]artifactFile, error) {
	return listArtifactBundleRoot(root, dir)
}

func listArtifactBundleRoot(root *os.Root, dir string) ([]artifactFile, error) {
	var files []artifactFile
	var directoryInfos []os.FileInfo
	var generatedOutputs []artifactBundleOutput
	err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifact bundle contains symlink: %s", filepath.ToSlash(path))
		}
		name := entry.Name()
		generatedOutput := name == "published-artifacts.md" || name == artifactManifestFilename
		rootEntry := !strings.Contains(path, "/")
		reservedOutputPath := rootEntry && generatedOutput
		if reservedOutputPath && info.IsDir() {
			return fmt.Errorf("artifact bundle contains directory at reserved output path: %s", filepath.ToSlash(path))
		}
		if info.IsDir() {
			directoryInfos = append(directoryInfos, info)
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifact bundle contains non-regular file: %s", filepath.ToSlash(path))
		}
		if generatedOutput {
			generatedOutputs = append(generatedOutputs, artifactBundleOutput{
				rootName: filepath.ToSlash(path),
				info:     info,
			})
			return nil
		}
		rootName := filepath.ToSlash(path)
		files = append(files, artifactFile{
			Kind:       artifactKindForPath(rootName),
			Name:       rootName,
			Path:       filepath.Join(dir, filepath.FromSlash(rootName)),
			rootName:   rootName,
			sourceInfo: info,
		})
		return nil
	})
	if err != nil {
		return nil, exit(2, "read artifact directory: %v", err)
	}
	for i := range files {
		files[i].bundleDirInfos = directoryInfos
		files[i].bundleOutputs = generatedOutputs
	}
	sortArtifactFiles(files)
	return files, nil
}

func artifactDirectoryInfo(path string) (os.FileInfo, error) {
	dir, err := openArtifactReadOnly(path)
	if err != nil {
		return nil, err
	}
	info, statErr := dir.Stat()
	closeErr := dir.Close()
	if statErr != nil {
		return nil, statErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", path)
	}
	return info, nil
}

func validateArtifactBundleRoot(root *os.Root, absDirectory string) (string, os.FileInfo, error) {
	rootInfo, err := root.Stat(".")
	if err != nil {
		return "", nil, exit(2, "read artifact directory: %v", err)
	}
	resolvedDirectory, err := filepath.EvalSymlinks(absDirectory)
	if err != nil {
		return "", nil, exit(2, "read artifact directory: %v", err)
	}
	resolvedInfo, err := artifactDirectoryInfo(resolvedDirectory)
	if err != nil {
		return "", nil, exit(2, "read artifact directory: %v", err)
	}
	if !os.SameFile(rootInfo, resolvedInfo) {
		return "", nil, exit(2, "artifact directory changed while opening it")
	}
	return resolvedDirectory, rootInfo, nil
}

func snapshotArtifactFiles(root *os.Root, files []artifactFile) ([]artifactFile, func(), error) {
	snapshot, cleanup, err := createArtifactSnapshotFile(root)
	if err != nil {
		return nil, func() {}, err
	}
	snapshots := make([]artifactFile, 0, len(files))
	var offset int64
	for _, file := range files {
		if strings.TrimSpace(file.rootName) == "" || file.sourceInfo == nil {
			cleanup()
			return nil, func() {}, exit(2, "artifact %s is missing validated bundle identity", file.Name)
		}
		source, err := openArtifactRootReadOnly(root, file.rootName)
		if err != nil {
			cleanup()
			return nil, func() {}, exit(2, "open validated artifact %s: %v", file.Name, err)
		}
		info, statErr := source.Stat()
		if statErr != nil {
			_ = source.Close()
			cleanup()
			return nil, func() {}, exit(2, "stat validated artifact %s: %v", file.Name, statErr)
		}
		if !info.Mode().IsRegular() || !os.SameFile(file.sourceInfo, info) {
			_ = source.Close()
			cleanup()
			return nil, func() {}, exit(2, "artifact %s changed after validation", file.Name)
		}
		hash := sha256.New()
		size, copyErr := io.Copy(io.MultiWriter(snapshot, hash), source)
		closeSourceErr := source.Close()
		if copyErr != nil {
			cleanup()
			return nil, func() {}, exit(2, "snapshot artifact %s: %v", file.Name, copyErr)
		}
		if closeSourceErr != nil {
			cleanup()
			return nil, func() {}, exit(2, "close validated artifact %s: %v", file.Name, closeSourceErr)
		}
		file.snapshotFile = snapshot
		file.snapshotOffset = offset
		file.snapshotSize = size
		file.snapshotHash = hex.EncodeToString(hash.Sum(nil))
		file.snapshotValid = true
		snapshots = append(snapshots, file)
		offset += size
	}
	return snapshots, cleanup, nil
}

func hashValidatedArtifactFiles(root *os.Root, files []artifactFile) ([]artifactFile, error) {
	validated := make([]artifactFile, 0, len(files))
	for _, file := range files {
		if strings.TrimSpace(file.rootName) == "" || file.sourceInfo == nil {
			return nil, exit(2, "artifact %s is missing validated bundle identity", file.Name)
		}
		source, err := openArtifactRootReadOnly(root, file.rootName)
		if err != nil {
			return nil, exit(2, "open validated artifact %s: %v", file.Name, err)
		}
		info, statErr := source.Stat()
		if statErr != nil {
			_ = source.Close()
			return nil, exit(2, "stat validated artifact %s: %v", file.Name, statErr)
		}
		if !info.Mode().IsRegular() || !os.SameFile(file.sourceInfo, info) {
			_ = source.Close()
			return nil, exit(2, "artifact %s changed after validation", file.Name)
		}
		hash := sha256.New()
		size, hashErr := io.Copy(hash, source)
		closeErr := source.Close()
		if hashErr != nil {
			return nil, exit(2, "hash validated artifact %s: %v", file.Name, hashErr)
		}
		if closeErr != nil {
			return nil, exit(2, "close validated artifact %s: %v", file.Name, closeErr)
		}
		file.snapshotSize = size
		file.snapshotHash = hex.EncodeToString(hash.Sum(nil))
		file.snapshotValid = true
		validated = append(validated, file)
	}
	return validated, nil
}

func snapshotArtifactData(root *os.Root, file artifactFile, data []byte) ([]artifactFile, func(), error) {
	snapshot, cleanup, err := createArtifactSnapshotFile(root)
	if err != nil {
		return nil, func() {}, err
	}
	if n, writeErr := snapshot.Write(data); writeErr != nil {
		cleanup()
		return nil, func() {}, exit(2, "write private snapshot for artifact %s: %v", file.Name, writeErr)
	} else if n != len(data) {
		cleanup()
		return nil, func() {}, exit(2, "write private snapshot for artifact %s: %v", file.Name, io.ErrShortWrite)
	}
	file = validatedArtifactData(file, data)
	file.snapshotFile = snapshot
	return []artifactFile{file}, cleanup, nil
}

func validatedArtifactData(file artifactFile, data []byte) artifactFile {
	hash := sha256.Sum256(data)
	file.snapshotSize = int64(len(data))
	file.snapshotHash = hex.EncodeToString(hash[:])
	file.snapshotValid = true
	return file
}

func createArtifactSnapshotFile(root *os.Root) (*os.File, func(), error) {
	rootInfo, err := root.Stat(".")
	if err != nil {
		return nil, func() {}, exit(2, "inspect artifact directory for private snapshot: %v", err)
	}
	tempBase, err := resolveArtifactSnapshotBase(rootInfo, root.Name(), os.TempDir())
	if err != nil {
		return nil, func() {}, err
	}
	snapshot, err := os.CreateTemp(tempBase, "crabbox-artifact-publish-*")
	if err != nil {
		return nil, func() {}, exit(2, "create private artifact snapshot: %v", err)
	}
	path := snapshot.Name()
	cleanup := func() {
		_ = snapshot.Truncate(0)
		_ = snapshot.Close()
		_ = os.Remove(path)
	}
	if _, err := resolveArtifactSnapshotBase(rootInfo, root.Name(), path); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return snapshot, cleanup, nil
}

func resolveArtifactSnapshotBase(rootInfo os.FileInfo, rootPath, path string) (string, error) {
	absRoot, err := filepath.Abs(rootPath)
	if err != nil {
		return "", exit(2, "resolve artifact directory for private snapshot: %v", err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", exit(2, "resolve private artifact snapshot: %v", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", exit(2, "resolve private artifact snapshot: %v", err)
	}
	if artifactPathWithinExact(absRoot, absPath) {
		return "", exit(2, "temporary directory must be outside artifact directory for safe publishing")
	}
	if resolvedRoot, resolveRootErr := filepath.EvalSymlinks(absRoot); resolveRootErr == nil && artifactPathWithinExact(resolvedRoot, resolvedPath) {
		return "", exit(2, "temporary directory must be outside artifact directory for safe publishing")
	}
	for current := filepath.Dir(resolvedPath); ; current = filepath.Dir(current) {
		info, statErr := artifactDirectoryInfo(current)
		if statErr != nil {
			return "", exit(2, "inspect private artifact snapshot: %v", statErr)
		}
		if os.SameFile(rootInfo, info) {
			return "", exit(2, "temporary directory must be outside artifact directory for safe publishing")
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return resolvedPath, nil
}

func requireArtifactSnapshot(file artifactFile) error {
	if err := requireArtifactValidation(file); err != nil {
		return err
	}
	if file.snapshotFile == nil || file.snapshotOffset < 0 {
		return exit(2, "artifact %s is missing a validated publish snapshot", file.Name)
	}
	return nil
}

func requireArtifactValidation(file artifactFile) error {
	if !file.snapshotValid || strings.TrimSpace(file.snapshotHash) == "" || file.snapshotSize < 0 {
		return exit(2, "artifact %s is missing validated publish metadata", file.Name)
	}
	return nil
}

func writeArtifactBundleFile(root *os.Root, name string, data []byte, perm os.FileMode) error {
	token, err := randomHex(12)
	if err != nil {
		return exit(2, "create private temporary name for %s: %v", name, err)
	}
	tempName := "." + filepath.Base(name) + ".crabbox-" + token
	createPerm := perm
	preserveExistingMode := false
	existingMode := os.FileMode(0)
	if info, statErr := root.Lstat(name); statErr == nil {
		if info.Mode().IsRegular() {
			createPerm = 0o600
			existingMode = info.Mode().Perm()
			preserveExistingMode = true
		}
	} else if !os.IsNotExist(statErr) {
		return exit(2, "inspect existing artifact output %s: %v", name, statErr)
	}
	file, err := root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, createPerm)
	if err != nil {
		return exit(2, "create private temporary artifact output for %s: %v", name, err)
	}
	removeTemp := true
	defer func() {
		_ = file.Close()
		if removeTemp {
			_ = root.Remove(tempName)
		}
	}()
	n, err := file.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return exit(2, "write artifact output %s: %v", name, err)
	}
	if preserveExistingMode {
		if err := file.Chmod(existingMode); err != nil {
			return exit(2, "preserve artifact output permissions for %s: %v", name, err)
		}
	}
	if err := file.Close(); err != nil {
		return exit(2, "close artifact output %s: %v", name, err)
	}
	if err := root.Rename(tempName, name); err != nil {
		return exit(2, "replace artifact output %s: %v", name, err)
	}
	removeTemp = false
	return nil
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
			if !opts.DryRun {
				if err := requireArtifactSnapshot(file); err != nil {
					return nil, err
				}
			}
			url, err := uploadArtifactS3(ctx, opts, file, key, artifactContentType(file.Path))
			if err != nil {
				return nil, err
			}
			out.URL = url
			out.Key = key
		case "cloudflare":
			if !opts.DryRun {
				if err := requireArtifactSnapshot(file); err != nil {
					return nil, err
				}
			}
			url, err := uploadArtifactCloudflare(ctx, opts, file, key, artifactContentType(file.Path))
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
		if err := requireArtifactValidation(file); err != nil {
			return nil, err
		}
		input.Files = append(input.Files, CoordinatorArtifactUploadInput{
			Name:        file.Name,
			Size:        file.snapshotSize,
			ContentType: artifactContentType(file.Path),
			SHA256:      file.snapshotHash,
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
			if err := uploadArtifactGrantSnapshot(ctx, file, grant); err != nil {
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

func uploadArtifactGrant(ctx context.Context, path string, grant CoordinatorArtifactUploadGrant) error {
	file, err := os.Open(path)
	if err != nil {
		return exit(2, "open artifact %s: %v", grant.Name, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return exit(2, "stat artifact %s: %v", grant.Name, err)
	}
	return uploadArtifactGrantReader(ctx, file, info.Size(), grant)
}

func uploadArtifactGrantSnapshot(ctx context.Context, file artifactFile, grant CoordinatorArtifactUploadGrant) error {
	if err := requireArtifactSnapshot(file); err != nil {
		return err
	}
	section := io.NewSectionReader(file.snapshotFile, file.snapshotOffset, file.snapshotSize)
	return uploadArtifactGrantReader(ctx, section, file.snapshotSize, grant)
}

func uploadArtifactGrantReader(ctx context.Context, file io.ReaderAt, size int64, grant CoordinatorArtifactUploadGrant) error {
	if grant.Upload.URL == "" {
		return exit(2, "artifact broker returned an empty upload URL for %s", grant.Name)
	}
	method := strings.ToUpper(strings.TrimSpace(grant.Upload.Method))
	if method == "" {
		method = http.MethodPut
	}
	contentLength := size
	if expected, ok, err := grantContentLength(grant.Upload.Headers); err != nil {
		return exit(2, "artifact broker returned an invalid content-length for %s: %v", grant.Name, err)
	} else if ok {
		if expected != size {
			return exit(2, "artifact %s size changed after broker grant: got %d bytes, expected %d", grant.Name, size, expected)
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

func uploadArtifactS3(ctx context.Context, opts artifactPublishOptions, file artifactFile, key, contentType string) (string, error) {
	dest := "s3://" + opts.Bucket + "/" + key
	args := awsBaseArgs(opts)
	source := file.Path
	if !opts.DryRun {
		source = "-"
	}
	args = append(args, "s3", "cp", source, dest, "--content-type", contentType)
	if opts.ACL != "" {
		args = append(args, "--acl", opts.ACL)
	}
	if opts.DryRun {
		return artifactS3URL(opts, key), nil
	}
	if err := requireArtifactSnapshot(file); err != nil {
		return "", err
	}
	args = append(args, "--expected-size", strconv.FormatInt(file.snapshotSize, 10))
	if _, err := exec.LookPath("aws"); err != nil {
		return "", exit(2, "aws CLI is required for artifacts publish --storage s3: %v", err)
	}
	if out, err := artifactPublisherCommandOutputWithInput(ctx, opts, nil, io.NewSectionReader(file.snapshotFile, file.snapshotOffset, file.snapshotSize), "aws", args...); err != nil {
		return "", exit(2, "aws s3 upload failed: %v: %s", err, tailForError(out))
	}
	if opts.Presign && opts.BaseURL == "" {
		presignArgs := awsBaseArgs(opts)
		presignArgs = append(presignArgs, "s3", "presign", dest, "--expires-in", fmt.Sprintf("%.0f", opts.Expires.Seconds()))
		out, err := artifactPublisherCommandOutput(ctx, opts, nil, "aws", presignArgs...)
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

func uploadArtifactCloudflare(ctx context.Context, opts artifactPublishOptions, file artifactFile, key, contentType string) (string, error) {
	if opts.DryRun {
		return artifactCloudflareURL(opts, key), nil
	}
	if err := requireArtifactSnapshot(file); err != nil {
		return "", err
	}
	if _, err := exec.LookPath("wrangler"); err != nil {
		return "", exit(2, "wrangler CLI is required for artifacts publish --storage cloudflare: %v", err)
	}
	out, err := artifactPublisherCommandOutputWithInput(ctx, opts, artifactCloudflareEnv(), io.NewSectionReader(file.snapshotFile, file.snapshotOffset, file.snapshotSize), "wrangler", "r2", "object", "put", opts.Bucket+"/"+key, "--pipe", "--content-type", contentType, "--remote")
	if err != nil {
		return "", exit(2, "wrangler r2 upload failed: %v: %s", err, tailForError(out))
	}
	return artifactCloudflareURL(opts, key), nil
}

func commandOutputWithEnvAndInput(ctx context.Context, env []string, input io.Reader, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = env
	}
	cmd.Stdin = input
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
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

func artifactPublisherCommandOutput(ctx context.Context, opts artifactPublishOptions, env []string, name string, args ...string) (string, error) {
	return artifactPublisherCommandOutputWithInput(ctx, opts, env, nil, name, args...)
}

func artifactPublisherCommandOutputWithInput(ctx context.Context, opts artifactPublishOptions, env []string, input io.Reader, name string, args ...string) (string, error) {
	if env == nil {
		env = os.Environ()
	}
	if len(opts.ChildEnvDenylist) > 0 {
		env = childEnvironmentWithout(env, opts.ChildEnvDenylist...)
	}
	return commandOutputWithEnvAndInput(ctx, env, input, name, args...)
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

type artifactSummaryBinding struct {
	path            string
	resolvedPath    string
	rootName        string
	file            *os.File
	fileInfo        os.FileInfo
	symlinkTargets  []string
	ambiguousParent bool
	directoryInfos  []os.FileInfo
}

func bindArtifactSummaryFile(path string) (*artifactSummaryBinding, error) {
	absPath, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return nil, exit(2, "resolve summary file: %v", err)
	}
	symlinkTargets, ambiguousParent, err := artifactSymlinkTargets(absPath)
	if err != nil {
		return nil, exit(2, "read summary file: %v", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return nil, exit(2, "read summary file: %v", err)
	}
	file, err := openArtifactReadOnly(absPath)
	if err != nil {
		return nil, exit(2, "read summary file: %v", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, exit(2, "read summary file: %v", err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, exit(2, "read summary file: not a regular file")
	}
	resolvedFile, err := openArtifactReadOnly(resolvedPath)
	if err != nil {
		_ = file.Close()
		return nil, exit(2, "read summary file: %v", err)
	}
	resolvedInfo, statErr := resolvedFile.Stat()
	closeErr := resolvedFile.Close()
	if statErr != nil {
		_ = file.Close()
		return nil, exit(2, "read summary file: %v", statErr)
	}
	if closeErr != nil {
		_ = file.Close()
		return nil, exit(2, "read summary file: %v", closeErr)
	}
	if !os.SameFile(info, resolvedInfo) {
		_ = file.Close()
		return nil, exit(2, "summary file changed while opening it")
	}
	identityPaths := append([]string{filepath.Dir(absPath)}, symlinkTargets...)
	return &artifactSummaryBinding{
		path:            absPath,
		resolvedPath:    resolvedPath,
		file:            file,
		fileInfo:        info,
		symlinkTargets:  symlinkTargets,
		ambiguousParent: ambiguousParent,
		directoryInfos:  artifactDirectoryIdentities(identityPaths),
	}, nil
}

func artifactDirectoryIdentities(paths []string) []os.FileInfo {
	var identities []os.FileInfo
	for _, path := range paths {
		for current := filepath.Clean(path); ; current = filepath.Dir(current) {
			if info, err := artifactDirectoryInfo(current); err == nil {
				identities = append(identities, info)
			}
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
		}
	}
	return identities
}

func artifactSymlinkTargets(path string) ([]string, bool, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, false, err
	}
	seen := map[string]bool{}
	var targets []string
	parentReference := false
	var visit func(string, int) error
	visit = func(currentPath string, depth int) error {
		if depth > 64 {
			return fmt.Errorf("too many summary file symlinks")
		}
		for _, prefix := range artifactPathPrefixes(currentPath) {
			info, err := os.Lstat(prefix)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink == 0 || seen[prefix] {
				continue
			}
			seen[prefix] = true
			target, err := os.Readlink(prefix)
			if err != nil {
				return err
			}
			resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(prefix))
			if err != nil {
				return err
			}
			if !filepath.IsAbs(target) {
				if len(resolvedParent) > 0 && !os.IsPathSeparator(resolvedParent[len(resolvedParent)-1]) {
					resolvedParent += string(filepath.Separator)
				}
				target = resolvedParent + target
			}
			ambiguousParent := artifactPathHasSymlinkBeforeParentReference(target)
			if ambiguousParent {
				parentReference = true
			}
			targets = append(targets, target)
			if ambiguousParent {
				continue
			}
			if err := visit(target, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(absPath, 0); err != nil {
		return nil, parentReference, err
	}
	return targets, parentReference, nil
}

func artifactPathHasSymlinkBeforeParentReference(path string) bool {
	volume := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path, volume)
	isSeparator := func(r rune) bool { return r <= 255 && os.IsPathSeparator(uint8(r)) }
	rest = strings.TrimLeftFunc(rest, isSeparator)
	current := volume + string(filepath.Separator)
	sawSymlink := false
	for _, part := range strings.FieldsFunc(rest, isSeparator) {
		switch part {
		case ".":
			continue
		case "..":
			if sawSymlink {
				return true
			}
			current = filepath.Dir(current)
			continue
		}
		current = filepath.Join(current, part)
		if info, err := os.Lstat(current); err == nil && info.Mode()&os.ModeSymlink != 0 {
			sawSymlink = true
		}
	}
	return false
}

func artifactPathPrefixes(path string) []string {
	volume := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path, volume)
	isSeparator := func(r rune) bool { return r <= 255 && os.IsPathSeparator(uint8(r)) }
	rest = strings.TrimLeftFunc(rest, isSeparator)
	current := volume + string(filepath.Separator)
	parts := strings.FieldsFunc(rest, isSeparator)
	prefixes := make([]string, 0, len(parts))
	for _, part := range parts {
		current = filepath.Join(current, part)
		prefixes = append(prefixes, current)
	}
	return prefixes
}

func artifactSummaryInsideBundle(directory, resolvedDirectory string, rootInfo os.FileInfo, binding *artifactSummaryBinding, files []artifactFile) bool {
	if binding == nil {
		return false
	}
	binding.rootName = artifactSummaryRootName(directory, resolvedDirectory, binding)
	for _, file := range files {
		if file.sourceInfo != nil && os.SameFile(binding.fileInfo, file.sourceInfo) {
			return true
		}
	}
	if len(files) > 0 {
		for _, output := range files[0].bundleOutputs {
			if output.info != nil && os.SameFile(binding.fileInfo, output.info) {
				binding.rootName = output.rootName
				return true
			}
		}
	}
	if binding.ambiguousParent {
		return true
	}
	for _, target := range binding.symlinkTargets {
		if artifactPathWithin(directory, target) || artifactPathWithin(resolvedDirectory, target) {
			return true
		}
	}
	for _, info := range binding.directoryInfos {
		if rootInfo != nil && info != nil && os.SameFile(rootInfo, info) {
			return true
		}
		if len(files) > 0 {
			for _, bundleInfo := range files[0].bundleDirInfos {
				if bundleInfo != nil && info != nil && os.SameFile(bundleInfo, info) {
					return true
				}
			}
		}
	}
	if artifactPathWithin(directory, binding.path) ||
		artifactPathWithin(resolvedDirectory, binding.path) ||
		artifactPathWithin(directory, binding.resolvedPath) ||
		artifactPathWithin(resolvedDirectory, binding.resolvedPath) {
		return true
	}
	return false
}

func artifactSummaryRootName(directory, resolvedDirectory string, binding *artifactSummaryBinding) string {
	if binding == nil {
		return ""
	}
	paths := append([]string{binding.path, binding.resolvedPath}, binding.symlinkTargets...)
	for _, rootPath := range []string{directory, resolvedDirectory} {
		for _, candidate := range paths {
			rel, err := filepath.Rel(filepath.Clean(rootPath), filepath.Clean(candidate))
			if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			return filepath.ToSlash(rel)
		}
	}
	return ""
}

func artifactPathWithin(root, path string) bool {
	return artifactPathWithinExact(root, path)
}

func artifactPathWithinExact(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func artifactPublishSummaryText(summary string, binding *artifactSummaryBinding, insideBundle bool, root *os.Root, files []artifactFile) (string, func(), error) {
	cleanup := func() {}
	if binding == nil {
		return strings.TrimSpace(summary), cleanup, nil
	}
	if !insideBundle {
		data, err := io.ReadAll(binding.file)
		if err != nil {
			return "", cleanup, exit(2, "read summary file: %v", err)
		}
		return combineArtifactSummary(summary, data), cleanup, nil
	}
	for _, file := range files {
		if file.sourceInfo == nil || !os.SameFile(binding.fileInfo, file.sourceInfo) {
			continue
		}
		summaryFile := file
		if summaryFile.snapshotFile == nil {
			data, err := io.ReadAll(binding.file)
			if err != nil {
				return "", cleanup, exit(2, "read validated summary file: %v", err)
			}
			if summaryFile.snapshotValid {
				hash := sha256.Sum256(data)
				if int64(len(data)) != summaryFile.snapshotSize || !strings.EqualFold(hex.EncodeToString(hash[:]), summaryFile.snapshotHash) {
					return "", cleanup, exit(2, "summary file changed after artifact validation")
				}
			}
			return combineArtifactSummary(summary, data), cleanup, nil
		}
		data, readErr := io.ReadAll(io.NewSectionReader(summaryFile.snapshotFile, summaryFile.snapshotOffset, summaryFile.snapshotSize))
		if readErr != nil {
			cleanup()
			return "", func() {}, exit(2, "read validated summary file: %v", readErr)
		}
		return combineArtifactSummary(summary, data), cleanup, nil
	}
	if binding.rootName != "" {
		file, err := openArtifactRootReadOnly(root, binding.rootName)
		if err != nil {
			return "", cleanup, exit(2, "summary file changed before artifact bundle validation: %v", err)
		}
		info, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil {
			return "", cleanup, exit(2, "stat validated summary file: %v", statErr)
		}
		if closeErr != nil {
			return "", cleanup, exit(2, "close validated summary file: %v", closeErr)
		}
		if !info.Mode().IsRegular() || !os.SameFile(binding.fileInfo, info) {
			return "", cleanup, exit(2, "summary file changed before artifact bundle validation")
		}
		data, err := io.ReadAll(binding.file)
		if err != nil {
			return "", cleanup, exit(2, "read validated summary file: %v", err)
		}
		return combineArtifactSummary(summary, data), cleanup, nil
	}
	return "", cleanup, exit(2, "summary file changed before artifact bundle validation")
}

func combineArtifactSummary(summary string, data []byte) string {
	if strings.TrimSpace(summary) != "" {
		return strings.TrimSpace(summary) + "\n\n" + strings.TrimSpace(string(data))
	}
	return strings.TrimSpace(string(data))
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

func postGitHubPRComment(ctx context.Context, opts artifactPublishOptions, body []byte) error {
	args := []string{"issue", "comment", strconv.Itoa(opts.PR), "--body-file", "-"}
	if strings.TrimSpace(opts.Repo) != "" {
		args = append(args, "--repo", strings.TrimSpace(opts.Repo))
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return exit(2, "gh CLI is required for artifacts publish --pr: %v", err)
	}
	if out, err := artifactPublisherCommandOutputWithInput(ctx, opts, nil, bytes.NewReader(body), "gh", args...); err != nil {
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
