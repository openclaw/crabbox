package main

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	portableSourcePath = "internal/cli/os_image.go"
	providerSourcePath = "internal/providers/applevm/backend.go"
	maxChecksumBytes   = 1024 * 1024
	requestTimeout     = 20 * time.Second
)

var (
	canonicalImagePath = regexp.MustCompile(`^/releases/resolute/release-[0-9]{8}/ubuntu-26[.]04-server-cloudimg-arm64[.]img$`)
	checksumLine       = regexp.MustCompile(`^([0-9A-Fa-f]{64})[ \t]+[*]?(.+)$`)
	errRedirect        = errors.New("redirects are disabled")
)

type imageSource struct {
	image  string
	digest string
}

func main() {
	if len(os.Args) > 2 {
		fmt.Fprintln(os.Stderr, "Apple VM image source check failed: expected at most one source-root argument")
		os.Exit(1)
	}
	root := "."
	if len(os.Args) == 2 {
		root = os.Args[1]
	}
	if err := run(context.Background(), root, newHTTPClient()); err != nil {
		fmt.Fprintf(os.Stderr, "Apple VM image source check failed: %s\n", err)
		os.Exit(1)
	}
	fmt.Println("Apple VM Ubuntu 26.04 image source verified")
}

func run(ctx context.Context, root string, client *http.Client) error {
	portableFile, err := parseRequiredFile(root, portableSourcePath)
	if err != nil {
		return err
	}
	portable, err := extractPortableSource(portableFile)
	if err != nil {
		return err
	}

	providerFile, err := parseRequiredFile(root, providerSourcePath)
	if err != nil {
		return err
	}
	provider := imageSource{}
	provider.image, err = extractFinalFallback(providerFile, "defaultAppleVMImage")
	if err != nil {
		return err
	}
	provider.digest, err = extractFinalFallback(providerFile, "defaultAppleVMImageSHA256")
	if err != nil {
		return err
	}
	if provider != portable {
		return errors.New("Apple VM defensive fallback does not match the portable Ubuntu 26.04 source")
	}

	imageURL, err := validateSource(portable)
	if err != nil {
		return err
	}
	return verifyRemoteSource(ctx, client, imageURL, portable.digest)
}

func parseRequiredFile(root, relativePath string) (*ast.File, error) {
	contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relativePath)))
	if err != nil {
		return nil, fmt.Errorf("required source file is unreadable: %s", relativePath)
	}
	file, err := parser.ParseFile(token.NewFileSet(), relativePath, contents, 0)
	if err != nil {
		return nil, fmt.Errorf("required source file is invalid Go: %s", relativePath)
	}
	return file, nil
}

func extractPortableSource(file *ast.File) (imageSource, error) {
	var declarations []ast.Expr
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.VAR {
			continue
		}
		for _, specification := range general.Specs {
			valueSpec, ok := specification.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, name := range valueSpec.Names {
				if name.Name != "osImageSpecs" {
					continue
				}
				if index >= len(valueSpec.Values) {
					return imageSource{}, errors.New("portable OS image specification must have a direct initializer")
				}
				declarations = append(declarations, valueSpec.Values[index])
			}
		}
	}
	if len(declarations) != 1 {
		return imageSource{}, errors.New("portable OS image specification must be declared exactly once")
	}

	imageMap, ok := declarations[0].(*ast.CompositeLit)
	if !ok {
		return imageSource{}, errors.New("portable OS image specification must be a map literal")
	}
	if _, ok := imageMap.Type.(*ast.MapType); !ok {
		return imageSource{}, errors.New("portable OS image specification must be a map literal")
	}

	var entries []*ast.CompositeLit
	for _, element := range imageMap.Elts {
		keyValue, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := directStringLiteral(keyValue.Key)
		if !ok || key != "ubuntu:26.04" {
			continue
		}
		entry, ok := keyValue.Value.(*ast.CompositeLit)
		if !ok {
			return imageSource{}, errors.New("portable Ubuntu 26.04 image specification must be a struct literal")
		}
		entries = append(entries, entry)
	}
	if len(entries) != 1 {
		return imageSource{}, errors.New("portable Ubuntu 26.04 image specification must appear exactly once")
	}

	image, err := extractLiteralField(entries[0], "AppleVMImage")
	if err != nil {
		return imageSource{}, err
	}
	digest, err := extractLiteralField(entries[0], "AppleVMSHA256")
	if err != nil {
		return imageSource{}, err
	}
	return imageSource{image: image, digest: digest}, nil
}

func extractLiteralField(literal *ast.CompositeLit, fieldName string) (string, error) {
	var values []ast.Expr
	for _, element := range literal.Elts {
		keyValue, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		identifier, ok := keyValue.Key.(*ast.Ident)
		if ok && identifier.Name == fieldName {
			values = append(values, keyValue.Value)
		}
	}
	if len(values) != 1 {
		return "", fmt.Errorf("portable Ubuntu 26.04 %s must appear exactly once", fieldName)
	}
	value, ok := directStringLiteral(values[0])
	if !ok {
		return "", fmt.Errorf("portable Ubuntu 26.04 %s must be a direct string literal", fieldName)
	}
	return value, nil
}

func extractFinalFallback(file *ast.File, functionName string) (string, error) {
	var functions []*ast.FuncDecl
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Recv == nil && function.Name.Name == functionName {
			functions = append(functions, function)
		}
	}
	if len(functions) != 1 {
		return "", fmt.Errorf("Apple VM %s fallback must be declared exactly once", functionName)
	}
	function := functions[0]
	if function.Body == nil || len(function.Body.List) == 0 {
		return "", fmt.Errorf("Apple VM %s final defensive fallback is missing", functionName)
	}
	if function.Type.Results == nil || len(function.Type.Results.List) != 1 {
		return "", fmt.Errorf("Apple VM %s fallback must return one string", functionName)
	}
	resultType, ok := function.Type.Results.List[0].Type.(*ast.Ident)
	if !ok || resultType.Name != "string" {
		return "", fmt.Errorf("Apple VM %s fallback must return one string", functionName)
	}

	finalReturn, ok := function.Body.List[len(function.Body.List)-1].(*ast.ReturnStmt)
	if !ok || len(finalReturn.Results) != 1 {
		return "", fmt.Errorf("Apple VM %s final defensive fallback must be a return statement", functionName)
	}
	value, ok := directStringLiteral(finalReturn.Results[0])
	if !ok {
		return "", fmt.Errorf("Apple VM %s final defensive fallback must be a direct string literal", functionName)
	}
	return value, nil
}

func directStringLiteral(expression ast.Expr) (string, bool) {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	return value, err == nil
}

func validateSource(source imageSource) (*url.URL, error) {
	if len(source.digest) != 64 {
		return nil, errors.New("portable Ubuntu 26.04 Apple VM digest is not canonical SHA-256 hex")
	}
	for _, character := range source.digest {
		if character < '0' || (character > '9' && character < 'a') || character > 'f' {
			return nil, errors.New("portable Ubuntu 26.04 Apple VM digest is not canonical SHA-256 hex")
		}
	}

	imageURL, err := url.Parse(source.image)
	if err != nil || imageURL.Scheme != "https" || imageURL.Host != "cloud-images.ubuntu.com" ||
		imageURL.User != nil || imageURL.RawQuery != "" || imageURL.ForceQuery || imageURL.Fragment != "" ||
		imageURL.Opaque != "" || imageURL.RawPath != "" || !canonicalImagePath.MatchString(imageURL.Path) {
		return nil, errors.New("portable Ubuntu 26.04 Apple VM URL is not a credential-free dated Canonical HTTPS release")
	}
	return imageURL, nil
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: requestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errRedirect
		},
	}
}

func verifyRemoteSource(ctx context.Context, client *http.Client, imageURL *url.URL, digest string) error {
	if err := probeImage(ctx, client, imageURL); err != nil {
		return err
	}
	manifest, err := fetchChecksumManifest(ctx, client, imageURL)
	if err != nil {
		return err
	}
	return verifyChecksumEntry(manifest, imageURL, digest)
}

func probeImage(ctx context.Context, client *http.Client, imageURL *url.URL) error {
	response, cancel, err := fetchDirect(ctx, client, imageURL, "image probe", map[int]bool{
		http.StatusOK:             true,
		http.StatusPartialContent: true,
	}, map[string]string{
		"Accept":          "application/octet-stream",
		"Accept-Encoding": "identity",
		"Range":           "bytes=0-0",
		"User-Agent":      "crabbox-apple-vm-image-source-check",
	})
	if err != nil {
		return err
	}
	defer cancel()
	defer response.Body.Close()

	var firstByte [1]byte
	if _, err := io.ReadFull(response.Body, firstByte[:]); err != nil {
		return errors.New("image probe response did not contain the requested byte")
	}
	return nil
}

func fetchChecksumManifest(ctx context.Context, client *http.Client, imageURL *url.URL) (string, error) {
	checksumURL := imageURL.ResolveReference(&url.URL{Path: path.Join(path.Dir(imageURL.Path), "SHA256SUMS")})
	response, cancel, err := fetchDirect(ctx, client, checksumURL, "checksum manifest", map[int]bool{
		http.StatusOK: true,
	}, map[string]string{
		"Accept":          "text/plain",
		"Accept-Encoding": "identity",
		"User-Agent":      "crabbox-apple-vm-image-source-check",
	})
	if err != nil {
		return "", err
	}
	defer cancel()
	defer response.Body.Close()

	if response.ContentLength > maxChecksumBytes {
		return "", errors.New("checksum manifest exceeds the bounded read limit")
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxChecksumBytes+1))
	if err != nil {
		return "", errors.New("checksum manifest body read failed")
	}
	if len(contents) > maxChecksumBytes {
		return "", errors.New("checksum manifest exceeds the bounded read limit")
	}
	return string(contents), nil
}

func fetchDirect(ctx context.Context, client *http.Client, requestedURL *url.URL, label string, acceptedStatuses map[int]bool, headers map[string]string) (*http.Response, context.CancelFunc, error) {
	requestContext, cancel := context.WithTimeout(ctx, requestTimeout)
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, requestedURL.String(), nil)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("%s request could not be created", label)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	response, err := client.Do(request)
	if err != nil {
		cancel()
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if errors.Is(err, errRedirect) {
			return nil, nil, fmt.Errorf("%s response redirected", label)
		}
		return nil, nil, fmt.Errorf("%s request failed", label)
	}
	if response.Request == nil || response.Request.URL == nil ||
		response.Request.URL.Scheme != requestedURL.Scheme || response.Request.URL.Host != requestedURL.Host {
		response.Body.Close()
		cancel()
		return nil, nil, fmt.Errorf("%s response changed scheme or host", label)
	}
	if len(response.Header.Values("Location")) != 0 {
		response.Body.Close()
		cancel()
		return nil, nil, fmt.Errorf("%s response contains a redirect location", label)
	}
	if !acceptedStatuses[response.StatusCode] {
		response.Body.Close()
		cancel()
		return nil, nil, fmt.Errorf("%s returned HTTP %d", label, response.StatusCode)
	}
	return response, cancel, nil
}

func verifyChecksumEntry(manifest string, imageURL *url.URL, digest string) error {
	filename := path.Base(imageURL.Path)
	matchingFilename := 0
	matchingDigest := ""
	for _, line := range strings.Split(strings.ReplaceAll(manifest, "\r\n", "\n"), "\n") {
		match := checksumLine.FindStringSubmatch(line)
		if match == nil || match[2] != filename {
			continue
		}
		matchingFilename++
		matchingDigest = match[1]
	}
	if matchingFilename != 1 || matchingDigest != digest {
		return errors.New("checksum manifest does not contain the exact shipped filename and digest")
	}
	return nil
}
