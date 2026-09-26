package blacksmith

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// Native Blacksmith adds ControlMaster=auto and ControlPersist to its SSH
// commands. OpenSSH's first value wins, so these options keep every connection
// in the command group the run already owns, without touching shared sockets.
func blacksmithNonPersistentSSHLauncher(executable string) string {
	return "#!/bin/sh\nexec " + core.ShellQuote(executable) +
		" -o ControlMaster=no -o ControlPath=none -o ControlPersist=no \"$@\"\n"
}

func blacksmithSSHCommandEnvironment() ([]string, func() error, error) {
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		return nil, nil, fmt.Errorf("resolve native Blacksmith SSH: %w", err)
	}
	ssh, err = filepath.Abs(ssh)
	if err != nil {
		return nil, nil, err
	}
	parent, err := filepath.Abs(os.TempDir())
	if err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp(parent, "crabbox-blacksmith-ssh-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() error { return os.RemoveAll(dir) }
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(blacksmithNonPersistentSSHLauncher(ssh)), 0o700); err != nil {
		_ = cleanup()
		return nil, nil, err
	}
	env := make([]string, 0, len(os.Environ())+2)
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if name != "PATH" && name != "BLACKSMITH_DISABLE_AUTO_UPDATE" {
			env = append(env, value)
		}
	}
	env = append(env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "BLACKSMITH_DISABLE_AUTO_UPDATE=1")
	return env, cleanup, nil
}
