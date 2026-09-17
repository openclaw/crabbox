package remoteruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type stage struct {
	path  string
	root  *os.Root
	info  os.FileInfo
	nonce string
}

func stagePath(nonce string) string { return "/tmp/crabbox-command-" + nonce }

func newStage(nonce string) (_ *stage, err error) {
	path := stagePath(nonce)
	if err := os.Mkdir(path, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("new command stage retained at %s: %w", path, err)
	}
	s := &stage{path: path, nonce: nonce, info: info}
	defer func() {
		if err == nil {
			return
		}
		if s.root != nil {
			err = errors.Join(err, s.root.Close())
		}
		if identityErr := s.sameDirectory(); identityErr != nil {
			err = errors.Join(err, identityErr)
		} else {
			err = errors.Join(err, os.RemoveAll(path))
		}
	}()
	// Do not change the process umask: the workload must inherit its caller's.
	if err := os.Chmod(path, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	s.root = root
	err = s.write(".nonce", []byte(nonce))
	if err == nil {
		err = root.Mkdir("scratch", 0700)
	}
	if err == nil {
		err = root.Chmod("scratch", 0700)
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *stage) file(name string) (*os.File, error) {
	f, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (s *stage) write(name string, data []byte) error {
	f, err := s.file(name)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Close())
}

func (s *stage) sameDirectory() error {
	info, err := os.Lstat(s.path)
	if err != nil || !os.SameFile(s.info, info) {
		return errors.New("command stage identity unavailable")
	}
	return nil
}

func (s *stage) canceled(ctx context.Context) (bool, error) {
	if ctx.Err() != nil {
		return true, nil
	}
	_, err := s.root.Lstat(".cancel")
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (s *stage) finish(preflight bool, code int, reason string) error {
	if err := s.sameDirectory(); err != nil {
		return err
	}
	if !preflight {
		return os.RemoveAll(s.path)
	}
	if err := s.root.RemoveAll("scratch"); err != nil {
		return err
	}
	state := reason
	if state == "" {
		state = "worker-failed"
		switch code {
		case 0:
			state = "ready"
		case 20:
			state = "missing-python3"
		case 21:
			state = "venv-unavailable"
		case 22:
			state = "pip-unavailable"
		}
	}
	record, err := completionRecord(s.nonce, state)
	if err != nil {
		return err
	}
	if err := s.write(".completion.tmp", record); err != nil {
		return err
	}
	return s.root.Rename(".completion.tmp", ".completion")
}

func (s *stage) receive(reader io.Reader, name string, size int64) error {
	f, err := s.file(name)
	if err != nil {
		return err
	}
	_, err = io.CopyN(f, reader, size)
	return errors.Join(err, f.Close())
}

func (s *stage) commandPath() string { return filepath.Join(s.path, "command") }
