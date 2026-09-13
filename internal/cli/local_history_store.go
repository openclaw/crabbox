package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	localHistoryMetadataLimit = 256 << 10
	localHistoryResultsLimit  = 1 << 20
	localHistoryReservation   = 10 << 20
	localHistoryCapacity      = 256 << 20
	localHistoryInactiveLimit = 100
	localHistoryMaxAge        = 30 * 24 * time.Hour
)

var errLocalHistoryBusy = errors.New("local history record is busy")

type localHistoryWriter struct {
	mu     sync.Mutex
	root   *os.Root
	lock   *os.File
	record localHistoryRecord
	done   bool
}

func localHistoryID(id string) bool {
	if len(id) != 36 || !strings.HasPrefix(id, "run_") {
		return false
	}
	for _, c := range id[4:] {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// The state namespace is separate from lease claims. Opening a reader never creates it.
func localHistoryRoot(create bool) (*os.Root, error) {
	state, err := CrabboxStateDir()
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(state) {
		return nil, fmt.Errorf("local history state path must be absolute")
	}
	logicalRoot, err := crabboxStateRootDir()
	if err != nil {
		return nil, err
	}
	for current := filepath.Clean(state); ; current = filepath.Dir(current) {
		if info, err := os.Lstat(current); err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("invalid local history state directory")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if current == filepath.Clean(logicalRoot) {
			break
		}
		if filepath.Dir(current) == current {
			return nil, fmt.Errorf("invalid local history state root")
		}
	}
	path := filepath.Join(state, "history")
	if create {
		if err := ensurePrivateDirectoryDurableWithSync(state, state, syncControllerDirectory); err != nil {
			return nil, err
		}
		if info, err := os.Lstat(path); err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("invalid local history directory")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		} else if err := ensurePrivateRunOutputDir(path); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("invalid local history directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	file, err := root.Open(".")
	if err == nil {
		err = localHistoryPrivate(file, true)
		file.Close()
	}
	if err != nil {
		root.Close()
		return nil, err
	}
	if create {
		if err := localHistoryMkdir(root, "runs"); err != nil {
			root.Close()
			return nil, err
		}
		if err := syncControllerDirectory(state); err != nil {
			root.Close()
			return nil, err
		}
	}
	return root, nil
}

func localHistoryMkdir(root *os.Root, name string) error {
	if _, err := localHistoryCreateDirectory(root, name); err != nil {
		return err
	}
	child, err := localHistoryChild(root, name)
	if err != nil {
		return err
	}
	child.Close()
	return localHistorySync(root)
}
func localHistoryChild(root *os.Root, name string) (*os.Root, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("invalid local history directory %s", name)
	}
	child, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	file, err := child.Open(".")
	if err == nil {
		err = localHistoryPrivate(file, true)
		file.Close()
	}
	if err != nil {
		child.Close()
		return nil, err
	}
	return child, nil
}

func localHistoryLock(root *os.Root, create, wait bool) (*os.File, error) {
	if create {
		f, err := openArtifactBundleTemp(root, "write.lock", 0o600, true)
		if err == nil {
			err = f.Sync()
			f.Close()
			if err != nil {
				return nil, err
			}
		} else if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	info, err := root.Lstat("write.lock")
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != 0 {
		return nil, fmt.Errorf("invalid local history lock")
	}
	file, err := localHistoryOpen(root, "write.lock", true)
	if err != nil {
		return nil, err
	}
	if err := localHistoryPrivate(file, false); err != nil {
		file.Close()
		return nil, err
	}
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		file.Close()
		return nil, fmt.Errorf("local history lock changed while opening")
	}
	// Namespace waits are bounded; record locks are always attempted without waiting.
	deadline := time.Now().Add(5 * time.Second)
	for {
		locked, err := tryLockWebVNCDaemonFile(file)
		if err != nil {
			file.Close()
			return nil, err
		}
		if locked {
			return file, nil
		}
		if !wait || time.Now().After(deadline) {
			file.Close()
			return nil, errLocalHistoryBusy
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func localHistoryUnlock(file *os.File) error {
	if file == nil {
		return nil
	}
	return errors.Join(unlockWebVNCDaemonFile(file), file.Close())
}

func beginLocalHistory(record localHistoryRecord) (*localHistoryWriter, error) {
	if !localHistoryID(record.ID) {
		return nil, fmt.Errorf("invalid local history run ID")
	}
	root, err := localHistoryRoot(true)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	lock, err := localHistoryLock(root, true, true)
	if err != nil {
		return nil, err
	}
	defer localHistoryUnlock(lock)
	if _, err := localHistoryPruneLocked(root, localHistoryReservation); err != nil {
		return nil, err
	}
	runs, err := localHistoryChild(root, "runs")
	if err != nil {
		return nil, err
	}
	defer runs.Close()
	created, err := localHistoryCreateDirectory(runs, record.ID)
	cleanup := created
	defer func() {
		if cleanup {
			_ = runs.RemoveAll(record.ID)
		}
	}()
	if err != nil {
		return nil, err
	}
	if !created {
		return nil, &os.PathError{Op: "mkdir", Path: record.ID, Err: os.ErrExist}
	}
	dir, err := localHistoryChild(runs, record.ID)
	if err != nil {
		return nil, err
	}
	held, err := localHistoryLock(dir, true, false)
	if err != nil {
		dir.Close()
		return nil, err
	}
	writer := &localHistoryWriter{root: dir, lock: held}
	record.Version = 1
	record.RecordingState = "active"
	record.Phase = "admitted"
	if record.StartedAt == "" {
		record.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	record.UpdatedAt = record.StartedAt
	if err := writer.Checkpoint(record); err != nil {
		localHistoryUnlock(held)
		dir.Close()
		return nil, err
	}
	if err := localHistorySync(runs); err != nil {
		localHistoryUnlock(held)
		dir.Close()
		return nil, err
	}
	cleanup = false
	return writer, nil
}

func (w *localHistoryWriter) Checkpoint(record localHistoryRecord) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.root == nil || w.done {
		return fmt.Errorf("local history writer is closed")
	}
	if w.record.ID != "" && record.ID != w.record.ID {
		return fmt.Errorf("local history run ID changed")
	}
	record.Version = 1
	record.RecordingState = "active"
	record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	record.LogSHA256 = ""
	record.ResultsSHA256 = ""
	bounded, data, err := localHistoryRecordBytes(record)
	if err != nil {
		return err
	}
	if err := localHistoryWrite(w.root, "record.json", data); err != nil {
		return err
	}
	w.record = bounded
	return nil
}

// Payloads are published first; only the last metadata replacement makes them terminal.
func (w *localHistoryWriter) Commit(record localHistoryRecord, log runLogSnapshot, results *TestResultSummary) (err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.root == nil || w.done {
		return fmt.Errorf("local history writer is closed")
	}
	if record.ID != w.record.ID {
		return fmt.Errorf("local history run ID changed")
	}
	defer func() {
		if err != nil {
			incomplete := w.record
			incomplete.RecordingState = "incomplete"
			incomplete.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			if _, data, encodeErr := localHistoryRecordBytes(incomplete); encodeErr == nil {
				_ = localHistoryWrite(w.root, "record.json", data)
			}
		}
		w.done = true
	}()
	if len(log.Log) > maxRunLogBytes || !utf8.ValidString(log.Log) {
		return fmt.Errorf("local history log exceeds bounded UTF-8 snapshot")
	}
	record.Version = 1
	record.RecordingState = "terminal"
	record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if record.EndedAt == "" {
		record.EndedAt = record.UpdatedAt
	}
	record.LogBytes = len(log.Log)
	record.LogTruncated = log.Truncated
	record.LogFullSHA256 = log.FullSHA256
	record.LogSHA256 = localHistoryDigest([]byte(log.Log))
	record.ResultsSHA256 = ""
	resultData, err := localHistoryResultBytes(results, &record)
	if err != nil {
		return err
	}
	if resultData != nil {
		record.ResultsSHA256 = localHistoryDigest(resultData)
	}
	bounded, data, err := localHistoryRecordBytes(record)
	if err != nil {
		return err
	}
	if err := localHistoryWrite(w.root, "log.txt", []byte(log.Log)); err != nil {
		return err
	}
	if resultData != nil {
		if err := localHistoryWrite(w.root, "results.json", resultData); err != nil {
			return err
		}
	}
	if err := localHistoryWrite(w.root, "record.json", data); err != nil {
		return err
	}
	w.record = bounded
	return nil
}
func (w *localHistoryWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.root == nil {
		return nil
	}
	err := errors.Join(localHistoryUnlock(w.lock), w.root.Close())
	w.root = nil
	w.lock = nil
	_, pruneErr := pruneLocalHistory()
	return errors.Join(err, pruneErr)
}
func localHistoryDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Each fixed temporary name is exclusive and bounded. A crash leaves at most one
// payload temporary of each kind; abandoned entries participate in ordinary pruning.
func localHistoryWrite(root *os.Root, name string, data []byte) error {
	tmp := "." + name + ".tmp"
	file, err := openArtifactBundleTemp(root, tmp, 0o600, true)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		return err
	}
	return localHistorySync(root)
}

func localHistoryReadBytes(root *os.Root, name string, maximum int64) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > maximum {
		return nil, fmt.Errorf("invalid or oversized local history %s", name)
	}
	file, err := localHistoryOpen(root, name, false)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err := localHistoryPrivate(file, false); err != nil {
		return nil, err
	}
	actual, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, actual) {
		return nil, fmt.Errorf("local history file changed")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("oversized local history %s", name)
	}
	return data, nil
}
func localHistoryReadRecord(dir *os.Root, id string) (localHistoryRecord, error) {
	var record localHistoryRecord
	data, err := localHistoryReadBytes(dir, "record.json", localHistoryMetadataLimit)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, err
	}
	if record.ID != id || record.Version != 1 {
		return record, fmt.Errorf("invalid local history metadata identity/version")
	}
	if _, _, err := localHistoryRecordBytes(record); err != nil {
		return record, err
	}
	return record, nil
}

// A process may stop after creating its directory but before publishing its
// first metadata. Represent only that absent-file window, never corrupt data.
func localHistoryReadInitialRecord(dir *os.Root, id string, missingLock bool) (localHistoryRecord, error) {
	record, err := localHistoryReadRecord(dir, id)
	if err == nil {
		if missingLock {
			return record, fmt.Errorf("local history metadata has no write lock")
		}
		return record, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return record, err
	}
	info, err := dir.Stat(".")
	if err != nil {
		return record, err
	}
	return localHistoryRecord{
		Version: 1, ID: id, RecordingState: "incomplete", Source: "local",
		CoordinatorState: "unknown", Phase: "initialization-incomplete",
		UpdatedAt:    info.ModTime().UTC().Format(time.RFC3339Nano),
		CaptureScope: "unavailable", ResultsState: "unavailable",
		Stdout: localHistoryStream{Availability: "unavailable"},
		Stderr: localHistoryStream{Availability: "unavailable"},
	}, nil
}

func localHistoryClip(value string, maximum int) (string, bool) {
	if maximum <= 0 {
		return "", len(value) > 0
	}
	changed := len(value) > maximum
	// A bounded lookahead completes a rune crossing the byte limit. Normalize
	// before clipping so an earlier malformed byte cannot erase a valid suffix.
	prefix := value[:min(len(value), maximum+utf8.UTFMax-1)]
	if !utf8.ValidString(prefix) {
		changed = true
		prefix = strings.ToValidUTF8(prefix, "�")
	}
	if len(prefix) > maximum {
		end := maximum
		for end > 0 && !utf8.RuneStart(prefix[end]) {
			end--
		}
		prefix = prefix[:end]
	}
	return strings.Clone(prefix), changed
}

func localHistoryRecordBytes(r localHistoryRecord) (localHistoryRecord, []byte, error) {
	fail := func() (localHistoryRecord, []byte, error) {
		return r, nil, fmt.Errorf("invalid local history metadata")
	}
	if !localHistoryID(r.ID) || r.Version != 1 {
		return fail()
	}
	if r.LeaseID != "" && (!validLeaseClaimPathID(r.LeaseID) || len(r.LeaseID) > 1024) {
		return fail()
	}
	if r.CoordinatorRunID != "" && (!validLeaseClaimPathID(r.CoordinatorRunID) || len(r.CoordinatorRunID) > 128) {
		return fail()
	}
	if r.CoordinatorReference != "" {
		reference := strings.TrimPrefix(r.CoordinatorReference, "sha256:")
		if len(reference) != 64 {
			return fail()
		}
		if _, err := hex.DecodeString(reference); err != nil {
			return fail()
		}
	}
	for _, value := range []string{r.StartedAt, r.UpdatedAt} {
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			return fail()
		}
	}
	if r.EndedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, r.EndedAt); err != nil {
			return fail()
		}
	}
	if r.RecordingState != "active" && r.RecordingState != "incomplete" && r.RecordingState != "terminal" {
		return fail()
	}
	if r.Source == "" {
		r.Source = "local"
	}
	if r.Source != "local" && r.Source != "both" {
		return fail()
	}
	if r.CoordinatorState == "" {
		r.CoordinatorState = "none"
	}
	if r.CoordinatorState != "none" && r.CoordinatorState != "recorded" && r.CoordinatorState != "unknown" {
		return fail()
	}
	if r.ResultsState == "" {
		r.ResultsState = "unavailable"
	}
	if r.ResultsState != "collected" && r.ResultsState != "no-results" && r.ResultsState != "unsupported" && r.ResultsState != "unavailable" {
		return fail()
	}
	for _, stream := range []*localHistoryStream{&r.Stdout, &r.Stderr} {
		if stream.Availability == "" {
			stream.Availability = "unavailable"
		}
		if stream.Availability != "retained" && stream.Availability != "omitted" && stream.Availability != "unavailable" {
			return fail()
		}
		stream.Reason, _ = localHistoryClip(stream.Reason, 4096)
	}
	var clipped bool
	r.CommandDisplay, clipped = localHistoryClip(r.CommandDisplay, 16<<10)
	r.CommandDisplayTruncated = r.CommandDisplayTruncated || clipped
	r.Diagnostic, clipped = localHistoryClip(r.Diagnostic, 16<<10)
	r.DiagnosticTruncated = r.DiagnosticTruncated || clipped
	for _, value := range []*string{&r.Provider, &r.Slug, &r.Target} {
		*value, _ = localHistoryClip(*value, 1024)
	}
	if len(r.Phase) > 64 || len(r.CaptureScope) > 64 || len(r.RunStatus) > 64 || len(r.ErrorKind) > 64 {
		return fail()
	}
	for _, digest := range []string{r.LogSHA256, r.ResultsSHA256} {
		if digest != "" {
			if len(digest) != 64 {
				return fail()
			}
			decoded, err := hex.DecodeString(digest)
			if err != nil || len(decoded) != 32 {
				return fail()
			}
		}
	}
	if r.LogFullSHA256 != "" {
		if len(r.LogFullSHA256) != len("sha256:")+64 || !strings.HasPrefix(r.LogFullSHA256, "sha256:") {
			return fail()
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(r.LogFullSHA256, "sha256:")); err != nil {
			return fail()
		}
	}
	data, err := json.Marshal(r)
	if err != nil {
		return r, nil, err
	}
	if len(data) > localHistoryMetadataLimit {
		return fail()
	}
	return r, data, nil
}

func localHistoryResultBytes(summary *TestResultSummary, record *localHistoryRecord) ([]byte, error) {
	if summary == nil {
		return nil, nil
	}
	out := *summary
	out.Files = []string{}
	out.Failed = []TestFailure{}
	record.ResultsClippedFields = 0
	record.ResultsOmittedFiles = 0
	record.ResultsOmittedFailures = 0
	clip := func(value string) string {
		result, clipped := localHistoryClip(value, 4096)
		if clipped {
			record.ResultsClippedFields++
		}
		return result
	}
	out.Format = clip(out.Format)
	data, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	size := len(data)
	// Incremental encoded sizes avoid repeatedly serializing the growing summary.
	// Only bounded details are encoded, and scalar totals never depend on retention.
	for i, value := range summary.Files {
		priorClipped := record.ResultsClippedFields
		value = clip(value)
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		extra := len(encoded)
		if len(out.Files) > 0 {
			extra++
		}
		if size+extra > localHistoryResultsLimit {
			record.ResultsClippedFields = priorClipped
			record.ResultsOmittedFiles = len(summary.Files) - i
			break
		}
		out.Files = append(out.Files, value)
		size += extra
	}
	for i, value := range summary.Failed {
		priorClipped := record.ResultsClippedFields
		value.Suite = clip(value.Suite)
		value.Name = clip(value.Name)
		value.Classname = clip(value.Classname)
		value.File = clip(value.File)
		value.Message = clip(value.Message)
		value.Type = clip(value.Type)
		value.Kind = clip(value.Kind)
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		extra := len(encoded)
		if len(out.Failed) > 0 {
			extra++
		}
		if size+extra > localHistoryResultsLimit {
			record.ResultsClippedFields = priorClipped
			record.ResultsOmittedFailures = len(summary.Failed) - i
			break
		}
		out.Failed = append(out.Failed, value)
		size += extra
	}
	record.ResultsDetailsTruncated = record.ResultsOmittedFiles > 0 || record.ResultsOmittedFailures > 0 || record.ResultsClippedFields > 0
	data, err = json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(data) > localHistoryResultsLimit {
		return nil, fmt.Errorf("local history result projection exceeds budget")
	}
	return data, nil
}

type localHistoryEntry struct {
	id     string
	size   int64
	stamp  time.Time
	active bool
}

// Called only with the namespace lock. Never wait on a record lock here: a
// finisher may be about to release it and run this same retention pass.
func localHistoryScan(root *os.Root) ([]localHistoryEntry, int64, error) {
	top, err := root.Open(".")
	if err != nil {
		return nil, 0, err
	}
	names, err := top.ReadDir(-1)
	top.Close()
	if err != nil {
		return nil, 0, err
	}
	for _, entry := range names {
		if entry.Name() != "write.lock" && entry.Name() != "runs" {
			return nil, 0, fmt.Errorf("unmanaged local history entry %s", entry.Name())
		}
	}
	runs, err := localHistoryChild(root, "runs")
	if err != nil {
		return nil, 0, err
	}
	defer runs.Close()
	file, err := runs.Open(".")
	if err != nil {
		return nil, 0, err
	}
	entries, err := file.ReadDir(-1)
	file.Close()
	if err != nil {
		return nil, 0, err
	}
	var out []localHistoryEntry
	var total int64 = 3 * 4096
	for _, entry := range entries {
		id := entry.Name()
		if !localHistoryID(id) {
			return nil, 0, fmt.Errorf("unmanaged local history run directory %s", id)
		}
		dir, err := localHistoryChild(runs, id)
		if err != nil {
			return nil, 0, err
		}
		held, lockErr := localHistoryLock(dir, false, false)
		if lockErr != nil && !errors.Is(lockErr, errLocalHistoryBusy) && !errors.Is(lockErr, os.ErrNotExist) {
			dir.Close()
			return nil, 0, lockErr
		}
		item := localHistoryEntry{id: id, size: 4096, active: errors.Is(lockErr, errLocalHistoryBusy)}
		inspect, err := dir.Open(".")
		if err != nil {
			localHistoryUnlock(held)
			dir.Close()
			return nil, 0, err
		}
		info, err := inspect.Stat()
		if err != nil {
			inspect.Close()
			localHistoryUnlock(held)
			dir.Close()
			return nil, 0, err
		}
		item.stamp = info.ModTime()
		children, err := inspect.ReadDir(-1)
		inspect.Close()
		if err == nil {
			for _, child := range children {
				name := child.Name()
				if name != "record.json" && name != "log.txt" && name != "results.json" && name != "write.lock" && name != ".record.json.tmp" && name != ".log.txt.tmp" && name != ".results.json.tmp" {
					err = fmt.Errorf("unmanaged local history payload %s", name)
					break
				}
				info, e := dir.Lstat(name)
				if e != nil {
					if item.active && errors.Is(e, os.ErrNotExist) {
						continue
					}
					err = e
					break
				}
				if !info.Mode().IsRegular() {
					err = fmt.Errorf("invalid local history payload %s", name)
					break
				}
				item.size += max(int64(4096), (info.Size()+4095)/4096*4096)
			}
		}
		if err == nil && !item.active {
			record, e := localHistoryReadRecord(dir, id)
			if e == nil {
				item.stamp, _ = time.Parse(time.RFC3339Nano, record.UpdatedAt)
			} else if !errors.Is(e, os.ErrNotExist) {
				err = fmt.Errorf("corrupt local history %s: %w", id, e)
			}
		}
		localHistoryUnlock(held)
		dir.Close()
		if err != nil {
			return nil, 0, err
		}
		if item.active {
			item.size = max(item.size, int64(localHistoryReservation))
		}
		total += item.size
		out = append(out, item)
	}
	return out, total, nil
}
func localHistoryPruneLocked(root *os.Root, reserve int64) (int, error) {
	entries, total, err := localHistoryScan(root)
	if err != nil {
		return 0, err
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].stamp.Equal(entries[j].stamp) {
			return entries[i].id < entries[j].id
		}
		return entries[i].stamp.Before(entries[j].stamp)
	})
	inactive := 0
	for _, entry := range entries {
		if !entry.active {
			inactive++
		}
	}
	runs, err := localHistoryChild(root, "runs")
	if err != nil {
		return 0, err
	}
	defer runs.Close()
	removed := 0
	for _, entry := range entries {
		if entry.active {
			continue
		}
		if inactive <= localHistoryInactiveLimit && total+reserve <= localHistoryCapacity && time.Since(entry.stamp) <= localHistoryMaxAge {
			continue
		}
		dir, err := localHistoryChild(runs, entry.id)
		if err != nil {
			return removed, err
		}
		held, err := localHistoryLock(dir, true, false)
		if errors.Is(err, errLocalHistoryBusy) {
			dir.Close()
			continue
		}
		if err != nil {
			dir.Close()
			return removed, err
		}
		// All readers acquire under the namespace lock, so no new lock waiter can
		// retain a deleted inode and later write through a recreated directory.
		err = localHistoryRemoveRecord(dir)
		localHistoryUnlock(held)
		dir.Close()
		if err == nil {
			err = runs.Remove(filepath.Join(entry.id, "write.lock"))
		}
		if err == nil {
			err = runs.Remove(entry.id)
		}
		if err != nil {
			return removed, err
		}
		total -= entry.size
		inactive--
		removed++
	}
	if err := localHistorySync(runs); err != nil {
		return removed, err
	}
	if total+reserve > localHistoryCapacity {
		return removed, fmt.Errorf("local history capacity unavailable: active reservations or retained data fill store")
	}
	return removed, nil
}
func localHistoryRemoveRecord(dir *os.Root) error {
	f, err := dir.Open(".")
	if err != nil {
		return err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "write.lock" {
			continue
		}
		switch entry.Name() {
		case "record.json", "log.txt", "results.json", ".record.json.tmp", ".log.txt.tmp", ".results.json.tmp":
		default:
			return fmt.Errorf("unmanaged local history payload %s", entry.Name())
		}
		info, err := dir.Lstat(entry.Name())
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("invalid local history payload")
		}
		if err := dir.Remove(entry.Name()); err != nil {
			return err
		}
	}
	// Windows cannot unlink an open lock; caller closes it before removing it.
	return nil
}

func localHistoryAvailable() (bool, error) {
	root, err := localHistoryRoot(false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer root.Close()
	return true, nil
}
func pruneLocalHistory() (int, error) {
	root, err := localHistoryRoot(false)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer root.Close()
	held, err := localHistoryLock(root, true, true)
	if err != nil {
		return 0, err
	}
	defer localHistoryUnlock(held)
	return localHistoryPruneLocked(root, 0)
}

func listLocalHistory(filter localHistoryFilter) ([]localHistoryRecord, error) {
	switch filter.State {
	case "", "active", "incomplete", "terminal", string(RunStatusSucceeded), string(RunStatusFailed), string(RunStatusTimedOut), string(RunStatusCanceled):
	default:
		return nil, fmt.Errorf("invalid local history state %q", filter.State)
	}
	records := []localHistoryRecord{}
	root, err := localHistoryRoot(false)
	if errors.Is(err, os.ErrNotExist) {
		return records, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	held, err := localHistoryLock(root, true, true)
	if err != nil {
		return nil, err
	}
	defer localHistoryUnlock(held)
	runs, err := localHistoryChild(root, "runs")
	if err != nil {
		return nil, err
	}
	defer runs.Close()
	f, err := runs.Open(".")
	if err != nil {
		return nil, err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !localHistoryID(entry.Name()) {
			return nil, fmt.Errorf("invalid local history run ID")
		}
		dir, err := localHistoryChild(runs, entry.Name())
		if err != nil {
			return nil, err
		}
		lock, lockErr := localHistoryLock(dir, false, false)
		if lockErr != nil && !errors.Is(lockErr, errLocalHistoryBusy) && !errors.Is(lockErr, os.ErrNotExist) {
			dir.Close()
			return nil, lockErr
		}
		record, err := localHistoryReadInitialRecord(dir, entry.Name(), errors.Is(lockErr, os.ErrNotExist))
		if err == nil && lockErr == nil && record.RecordingState == "active" {
			record.RecordingState = "incomplete"
			record.Stdout.Availability = "unavailable"
			record.Stderr.Availability = "unavailable"
		}
		localHistoryUnlock(lock)
		dir.Close()
		if err != nil {
			return nil, err
		}
		if filter.LeaseID != "" && filter.LeaseID != record.LeaseID {
			continue
		}
		if filter.State != "" && filter.State != record.RecordingState && filter.State != string(record.RunStatus) {
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, records[i].StartedAt)
		b, _ := time.Parse(time.RFC3339Nano, records[j].StartedAt)
		if a.Equal(b) {
			return records[i].ID < records[j].ID
		}
		return a.After(b)
	})
	if filter.Limit > 0 && len(records) > filter.Limit {
		records = records[:filter.Limit]
	}
	return records, nil
}
func readLocalHistory(id string) (localHistoryRecord, string, *TestResultSummary, error) {
	var record localHistoryRecord
	if !localHistoryID(id) {
		return record, "", nil, fmt.Errorf("invalid local history run ID")
	}
	root, err := localHistoryRoot(false)
	if err != nil {
		return record, "", nil, err
	}
	defer root.Close()
	held, err := localHistoryLock(root, true, true)
	if err != nil {
		return record, "", nil, err
	}
	defer localHistoryUnlock(held)
	runs, err := localHistoryChild(root, "runs")
	if err != nil {
		return record, "", nil, err
	}
	defer runs.Close()
	dir, err := localHistoryChild(runs, id)
	if err != nil {
		return record, "", nil, err
	}
	defer dir.Close()
	lock, lockErr := localHistoryLock(dir, false, false)
	if lockErr != nil && !errors.Is(lockErr, errLocalHistoryBusy) && !errors.Is(lockErr, os.ErrNotExist) {
		return record, "", nil, lockErr
	}
	defer localHistoryUnlock(lock)
	record, err = localHistoryReadInitialRecord(dir, id, errors.Is(lockErr, os.ErrNotExist))
	if err != nil {
		return record, "", nil, err
	}
	if record.RecordingState != "terminal" {
		if lockErr == nil {
			record.RecordingState = "incomplete"
		}
		record.Stdout.Availability = "unavailable"
		record.Stderr.Availability = "unavailable"
		return record, "", nil, nil
	}
	data, err := localHistoryReadBytes(dir, "log.txt", maxRunLogBytes)
	if err != nil {
		return record, "", nil, err
	}
	if !utf8.Valid(data) || localHistoryDigest(data) != record.LogSHA256 || len(data) != record.LogBytes {
		return record, "", nil, fmt.Errorf("local history log integrity mismatch")
	}
	var summary *TestResultSummary
	if record.ResultsSHA256 != "" {
		resultData, err := localHistoryReadBytes(dir, "results.json", localHistoryResultsLimit)
		if err != nil {
			return record, "", nil, err
		}
		if localHistoryDigest(resultData) != record.ResultsSHA256 {
			return record, "", nil, fmt.Errorf("local history results integrity mismatch")
		}
		summary = &TestResultSummary{}
		if err := json.Unmarshal(resultData, summary); err != nil {
			return record, "", nil, err
		}
	}
	return record, string(data), summary, nil
}
func deleteLocalHistory(id string) error {
	if !localHistoryID(id) {
		return fmt.Errorf("invalid local history run ID")
	}
	root, err := localHistoryRoot(false)
	if err != nil {
		return err
	}
	defer root.Close()
	held, err := localHistoryLock(root, true, true)
	if err != nil {
		return err
	}
	defer localHistoryUnlock(held)
	runs, err := localHistoryChild(root, "runs")
	if err != nil {
		return err
	}
	defer runs.Close()
	dir, err := localHistoryChild(runs, id)
	if err != nil {
		return err
	}
	lock, err := localHistoryLock(dir, true, false)
	if err != nil {
		dir.Close()
		return err
	}
	err = localHistoryRemoveRecord(dir)
	localHistoryUnlock(lock)
	dir.Close()
	if err != nil {
		return err
	}
	if err := runs.Remove(filepath.Join(id, "write.lock")); err != nil {
		return err
	}
	if err := runs.Remove(id); err != nil {
		return err
	}
	return localHistorySync(runs)
}
