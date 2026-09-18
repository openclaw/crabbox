package remoteruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

func openControlStage(nonce string) (*stage, error) {
	path := stagePath(nonce)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("command stage owner unavailable")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	s := &stage{path: path, info: info, root: root, nonce: nonce}
	inside, err := root.Stat(".")
	if err == nil && !os.SameFile(info, inside) {
		err = errors.New("command stage identity unavailable")
	}
	if err == nil {
		var recorded []byte
		recorded, err = s.readRecord(".nonce", 32)
		if err == nil && string(recorded) != nonce {
			err = errors.New("command stage nonce unavailable")
		}
	}
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	return s, nil
}

func (s *stage) readRecord(name string, limit int64) ([]byte, error) {
	f, err := s.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("invalid command stage record")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.Join(errors.New("invalid command stage record"), err)
	}
	return data, nil
}

func (s *stage) requestCancellation() error {
	f, err := s.file(".cancel")
	if errors.Is(err, os.ErrExist) {
		info, statErr := s.root.Lstat(".cancel")
		if statErr != nil || !info.Mode().IsRegular() {
			return errors.New("command cancellation marker unavailable")
		}
		return nil
	}
	if err != nil {
		return err
	}
	return f.Close()
}

func control(ctx context.Context, nonce, action string, output io.Writer) error {
	switch action {
	case "observe", "cancel", "retire", "cleanup":
	default:
		return errors.New("invalid remote runtime control")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	s, err := openControlStage(nonce)
	if err != nil {
		if action == "cleanup" && errors.Is(err, os.ErrNotExist) {
			// Only an absent stage is terminal; a missing record is not.
			if _, pathErr := os.Lstat(stagePath(nonce)); errors.Is(pathErr, os.ErrNotExist) {
				return context.Cause(ctx)
			}
		}
		return err
	}
	defer s.root.Close()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	requested := false
	for {
		if err := s.sameDirectory(); err != nil {
			if action == "cleanup" {
				if _, pathErr := os.Lstat(s.path); errors.Is(pathErr, os.ErrNotExist) {
					return context.Cause(ctx)
				}
			}
			return err
		}
		record, err := s.readRecord(".completion", CompletionLimit)
		if err == nil {
			if _, err := ParseCompletion(record, nonce); err != nil {
				return err
			}
			if action == "retire" || action == "cleanup" {
				if _, err := s.root.Lstat("scratch"); !errors.Is(err, os.ErrNotExist) {
					return errors.New("command scratch removal unconfirmed")
				}
				if action == "cleanup" {
					return context.Cause(ctx)
				}
				if err := s.sameDirectory(); err != nil {
					return err
				}
				if err := context.Cause(ctx); err != nil {
					return err
				}
				return errors.Join(os.RemoveAll(s.path), context.Cause(ctx))
			}
			if err := context.Cause(ctx); err != nil {
				return err
			}
			_, err = output.Write(record)
			return errors.Join(err, context.Cause(ctx))
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if !requested && (action == "cancel" || action == "cleanup") {
			// The running owner performs teardown; collectors never signal PIDs.
			if err := s.requestCancellation(); err != nil {
				return err
			}
			requested = true
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("command completion unconfirmed: %w", context.Cause(ctx))
		case <-ticker.C:
		}
	}
}
