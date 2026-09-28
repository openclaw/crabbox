package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"
)

const coldSyncReady = "CRABBOX_COLD_SYNC_READY\n"
const coldSyncSkipped = "CRABBOX_COLD_SYNC_SKIPPED\n"
const coldSyncSkippedCode = 83

func coldSyncSupported(target SSHTarget) bool {
	return runtime.GOOS != "windows" && target.TargetOS != targetWindows
}

// Called before pending metadata is written, inside that same owned command.
func remoteColdSyncProbe(workdir string) string {
	return `cold_sync=0
if [ ! -L ` + shellPathQuote(workdir) + ` ]; then
  if [ ! -e ` + shellPathQuote(workdir) + ` ]; then
    cold_sync=1
  elif [ -d ` + shellPathQuote(workdir) + ` ]; then
    cold_sync=1
    for entry in ` + shellPathQuote(workdir) + `/* ` + shellPathQuote(workdir) + `/.[!.]* ` + shellPathQuote(workdir) + `/..?*; do
      if [ -e "$entry" ] || [ -L "$entry" ]; then cold_sync=0; break; fi
    done
  fi
fi
if [ "$cold_sync" = 1 ]; then
  case "$(/usr/bin/env -i PATH=/usr/bin:/bin LANG=C LC_ALL=C /bin/sh -c 'command -v gzip >/dev/null && tar --version' 2>/dev/null || true)" in
    *"(GNU tar)"*|bsdtar\ *) ;;
    *) cold_sync=0 ;;
  esac
fi
`
}

func remoteColdSyncExtract(workdir, token string, compressed bool) string {
	options := "-xpf"
	if compressed {
		options = "-xzpf"
	}
	return remoteHermeticPOSIXControlCommand(`set -e
skip_cold_sync() { printf '` + coldSyncSkipped + `'; exit 83; }
[ ! -L ` + shellPathQuote(workdir) + ` ] || skip_cold_sync
cd ` + shellPathQuote(workdir) + `
for entry in ./* ./.[!.]* ./..?*; do
  if [ -e "$entry" ] || [ -L "$entry" ]; then
    [ "$entry" = ./.crabbox ] || skip_cold_sync
  fi
done
[ -d .crabbox ] && [ ! -L .crabbox ] || skip_cold_sync
for entry in .crabbox/` + remoteSyncPendingManifestName(token) + ` .crabbox/` + remoteSyncPendingDeletedName(token) + `; do
  [ -f "$entry" ] && [ ! -L "$entry" ] || skip_cold_sync
done
for entry in .crabbox/* .crabbox/.[!.]* .crabbox/..?*; do
  if [ -e "$entry" ] || [ -L "$entry" ]; then
    case "$entry" in
      .crabbox/` + remoteSyncPendingManifestName(token) + `|.crabbox/` + remoteSyncPendingDeletedName(token) + `)
        [ -f "$entry" ] && [ ! -L "$entry" ] || skip_cold_sync ;;
      *) skip_cold_sync ;;
    esac
  fi
done
exec tar ` + options + ` -
`)
}

func coldSyncDirectories(root string, files []string) ([]string, bool, error) {
	seen := map[string]bool{}
	var dirs []string
	for _, rel := range files {
		if !safeRepoRel(rel) {
			return nil, false, fmt.Errorf("unsafe cold sync path %q", rel)
		}
		for dir := filepath.ToSlash(filepath.Dir(rel)); dir != "."; dir = filepath.ToSlash(filepath.Dir(dir)) {
			if seen[dir] {
				break
			}
			info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(dir)))
			if err != nil {
				return nil, false, err
			}
			// Keep rsync's existing behavior for symlink ancestors and type changes.
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil, false, nil
			}
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs, true, nil
}

func writeColdSyncArchive(ctx context.Context, output io.Writer, root string, files, dirs []string, compressed bool) error {
	var gz *gzip.Writer
	if compressed {
		var err error
		gz, err = gzip.NewWriterLevel(output, gzip.BestSpeed)
		if err != nil {
			return err
		}
		output = gz
	}
	tw := tar.NewWriter(output)
	managed, err := newManagedSyncScope(root)
	if err != nil {
		return err
	}
	type member struct {
		path, order string
		directory   bool
	}
	members := make([]member, 0, len(dirs)+len(files))
	for _, dir := range dirs {
		members = append(members, member{dir, dir + "/", true})
	}
	for _, file := range files {
		members = append(members, member{file, file, false})
	}
	// GNU tar restores directory metadata when it leaves a subtree. A trailing
	// slash in the sort key also keeps prefix siblings such as "a!" outside "a/".
	sort.Slice(members, func(i, j int) bool { return members[i].order < members[j].order })
	for _, entry := range members {
		protected, err := managed.contains(entry.path)
		if err != nil {
			return err
		}
		if protected {
			return errors.New("cold sync path entered protected managed state")
		}
		if err := appendSyncArchiveMemberWithFormat(ctx, tw, root, entry.path, tar.FormatPAX, entry.directory); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if gz != nil {
		return gz.Close()
	}
	return nil
}

// No replay: the foreground SSH process and producer are joined on every path.
func streamColdSync(ctx context.Context, target SSHTarget, root, workdir, token string, files []string, compression string, timeout time.Duration, stderr io.Writer) (used bool, err error) {
	dirs, eligible, err := coldSyncDirectories(root, files)
	if err != nil || !eligible {
		return false, err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	compressed := syncCompressionEnabled(compression)
	inputPresent := int64(0) // POSIX witness needs presence, not an archive size.
	prepared, err := prepareWorkspaceOwnerRemote(ctx, target, remoteColdSyncExtract(workdir, token, compressed), &inputPresent)
	if err != nil {
		return true, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.close(ctx, target))
		}
	}()
	session, err := newSSHTransportSession(ctx, target, false)
	if err != nil {
		return true, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	args := append(session.commandPrefixWithOptions("10", "3"), session.host(), prepared.command)
	handle := pondMeshExecCommand(ctx, target, directSSHExecutable(), args...)
	reader, writer := io.Pipe()
	producerCtx, cancelProducer := context.WithCancel(ctx)
	defer cancelProducer()
	produced := make(chan error, 1)
	go func() {
		err := writeColdSyncArchive(producerCtx, writer, root, files, dirs, compressed)
		_ = writer.CloseWithError(err)
		produced <- err
	}()
	var output bytes.Buffer
	stdout, commandStderr, finish := workspaceOwnerSetupStreams(prepared.setupMarker, &output, stderr)
	handle.cmd.Stdin, handle.cmd.Stdout, handle.cmd.Stderr = reader, stdout, commandStderr
	stopHeartbeat := startSyncHeartbeat(stderr, time.Now(), 15*time.Second)
	err = handle.Start()
	if err == nil {
		err = handle.Wait()
	}
	stopHeartbeat()
	err = finish(err)
	cancelProducer()
	_ = reader.Close()
	producerErr := <-produced
	if exitCode(err) == coldSyncSkippedCode && output.String() == coldSyncSkipped && ctx.Err() == nil {
		if producerErr != nil && !errors.Is(producerErr, context.Canceled) && !errors.Is(producerErr, io.ErrClosedPipe) {
			return true, producerErr
		}
		return false, nil
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	return true, errors.Join(err, producerErr)
}
