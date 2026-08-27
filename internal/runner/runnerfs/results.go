package runnerfs

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	AutoMaxFiles      = 50
	AutoMaxFileBytes  = 16 << 20
	AutoMaxTotalBytes = 64 << 20
	AutoSniffBytes    = 4 << 10
	AutoFailureBytes  = 1 << 20
)

type Warning struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

type ResultOptions struct {
	Paths              []string
	Auto               bool
	After              time.Time
	ExplicitMaxBytes   int64
	ExplicitTotalBytes int64
}

type Results struct {
	Files    []File
	Warnings []Warning
}

type resultCandidate struct {
	name     string
	failed   bool
	identity os.FileInfo
}

// The bounded discovery prefix is not a full report parse. Tokenize it so log
// text in comments or CDATA cannot displace reports with real failure elements.
func containsJUnitFailure(data []byte) bool {
	// Discovery only inspects ASCII markup, not decoded diagnostic text. Keep
	// ASCII-compatible declarations and invalid text bytes from hiding tags;
	// returned report bytes and the full parser's validation remain unchanged.
	data = bytes.ToValidUTF8(data, []byte("\uFFFD"))
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	for {
		// A long attribute can cross the sniff limit. At a token boundary the
		// tag's name is sufficient; comments and CDATA are consumed as tokens.
		prefix := bytes.TrimSpace(data[decoder.InputOffset():])
		for _, name := range []string{"<failure", "<error"} {
			if bytes.HasPrefix(prefix, []byte(name)) && len(prefix) > len(name) && strings.ContainsRune("\t\r\n />", rune(prefix[len(name)])) {
				return true
			}
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		if element, ok := token.(xml.StartElement); ok && (element.Name.Local == "failure" || element.Name.Local == "error") {
			return true
		}
	}
}

// CollectResults reads explicit files before bounded automatic discovery. An
// external symlink is never treated as a report, even if its filename matches.
func (r *Root) CollectResults(ctx context.Context, options ResultOptions) (Results, error) {
	var result Results
	if err := ctx.Err(); err != nil {
		return result, err
	}
	var identities []os.FileInfo
	total := int64(0)
	if options.ExplicitMaxBytes < 1 || options.ExplicitTotalBytes < 1 {
		if len(options.Paths) != 0 {
			return result, errors.New("explicit result byte limits must be positive")
		}
	}
	for _, name := range options.Paths {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		remaining := options.ExplicitTotalBytes - total
		limit := min(options.ExplicitMaxBytes, remaining)
		file, err := r.readDistinct(ctx, name, limit, identities)
		if err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			if errors.Is(err, ErrLimit) || errors.Is(err, ErrChanged) {
				result.Warnings = append(result.Warnings, Warning{name, err.Error()})
			}
			continue
		}
		result.Files = append(result.Files, file)
		identities = append(identities, file.identity)
		total += int64(len(file.Data))
	}
	if !options.Auto {
		return result, ctx.Err()
	}
	candidates, warnings, err := r.resultCandidates(ctx, options.After, identities)
	result.Warnings = append(result.Warnings, warnings...)
	if err != nil {
		return result, err
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].failed != candidates[j].failed {
			return candidates[i].failed
		}
		return candidates[i].name < candidates[j].name
	})
	autoTotal := int64(0)
	autoCount := 0
	for _, candidate := range candidates {
		if autoCount >= AutoMaxFiles {
			break
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		file, err := r.readDistinct(ctx, candidate.name, AutoMaxFileBytes, identities)
		if err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			if errors.Is(err, ErrLimit) {
				result.Warnings = append(result.Warnings, Warning{candidate.name, fmt.Sprintf("report exceeds %d-byte per-file limit", AutoMaxFileBytes)})
			} else if errors.Is(err, ErrChanged) {
				result.Warnings = append(result.Warnings, Warning{candidate.name, err.Error()})
			}
			continue
		}
		if !options.After.IsZero() && file.ModTime.Before(options.After) {
			continue
		}
		if !bytes.Contains(file.Data[:min(len(file.Data), AutoSniffBytes)], []byte("<testsuite")) {
			continue
		}
		if autoTotal+int64(len(file.Data)) > AutoMaxTotalBytes {
			result.Warnings = append(result.Warnings, Warning{candidate.name, fmt.Sprintf("report exceeds remaining %d-byte aggregate limit", AutoMaxTotalBytes)})
			continue
		}
		autoTotal += int64(len(file.Data))
		autoCount++
		result.Files = append(result.Files, file)
		identities = append(identities, file.identity)
	}
	return result, ctx.Err()
}

func (r *Root) resultCandidates(ctx context.Context, after time.Time, excluded []os.FileInfo) ([]resultCandidate, []Warning, error) {
	var passing, failing []resultCandidate
	var warnings []Warning
	err := r.walkDirectory(ctx, ".", func(name string, entry fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			directory := entry.Name()
			if runtime.GOOS == "windows" {
				directory = strings.ToLower(directory)
			}
			if directory == ".git" || directory == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !junitFilename(entry.Name()) {
			return nil
		}
		file, err := r.openRegular(filepath.FromSlash(name))
		if err != nil {
			return nil
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || (!after.IsZero() && info.ModTime().Before(after)) {
			return nil
		}
		for _, identity := range excluded {
			if os.SameFile(identity, info) {
				return nil
			}
		}
		sniffBytes := int64(AutoFailureBytes)
		if info.Size() > AutoMaxFileBytes {
			sniffBytes = AutoSniffBytes
		}
		data, err := io.ReadAll(io.LimitReader(file, sniffBytes))
		if err != nil || !bytes.Contains(data[:min(len(data), AutoSniffBytes)], []byte("<testsuite")) {
			return nil
		}
		if info.Size() > AutoMaxFileBytes {
			// Rejected files must not consume eligible report slots. Bound their
			// diagnostics independently of the candidate list.
			if len(warnings) < AutoMaxFiles {
				warnings = append(warnings, Warning{name, fmt.Sprintf("report exceeds %d-byte per-file limit", AutoMaxFileBytes)})
			}
			return nil
		}
		candidate := resultCandidate{name: name, failed: containsJUnitFailure(data), identity: info}
		if candidate.failed {
			// A changed report may cross priority classes between alias reads.
			for index, previous := range passing {
				if os.SameFile(previous.identity, info) {
					candidate.name = min(candidate.name, previous.name)
					passing = append(passing[:index], passing[index+1:]...)
					break
				}
			}
			failing = retainResultCandidate(failing, candidate)
		} else {
			for _, previous := range failing {
				if os.SameFile(previous.identity, info) {
					return nil
				}
			}
			passing = retainResultCandidate(passing, candidate)
		}
		return nil
	})
	return append(failing, passing...), warnings, err
}

// Retain only the earliest candidates in each priority class while still
// inspecting the whole tree. Later failures must outrank earlier passing files.
func retainResultCandidate(values []resultCandidate, next resultCandidate) []resultCandidate {
	for index, previous := range values {
		if previous.identity != nil && next.identity != nil && os.SameFile(previous.identity, next.identity) {
			if previous.name <= next.name {
				return values
			}
			values = append(values[:index], values[index+1:]...)
			break
		}
	}
	index := sort.Search(len(values), func(i int) bool { return values[i].name >= next.name })
	if index >= AutoMaxFiles {
		return values
	}
	values = append(values, resultCandidate{})
	copy(values[index+1:], values[index:])
	values[index] = next
	return values[:min(len(values), AutoMaxFiles)]
}

func junitFilename(name string) bool {
	return junitFilenameForOS(runtime.GOOS, name)
}

func junitFilenameForOS(goos, name string) bool {
	if goos == "windows" {
		lower := strings.ToLower(name)
		return lower == "results.xml" || strings.HasPrefix(lower, "junit") && strings.HasSuffix(lower, ".xml") || strings.HasPrefix(lower, "test-") && strings.HasSuffix(lower, ".xml")
	}
	return name == "results.xml" || strings.HasPrefix(name, "junit") && strings.HasSuffix(name, ".xml") || strings.HasPrefix(name, "TEST-") && strings.HasSuffix(name, ".xml")
}
