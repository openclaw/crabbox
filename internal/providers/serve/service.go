package serve

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

const serviceName = "dev.crabbox.serve"

func service(ctx context.Context, action string, o options, rt core.Runtime) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	var path, body string
	commandArgs := serviceArguments(executable, o)
	switch runtime.GOOS {
	case "darwin":
		path = filepath.Join(home, "Library", "LaunchAgents", serviceName+".plist")
		var values strings.Builder
		for _, arg := range commandArgs {
			var b bytes.Buffer
			if err := xml.EscapeText(&b, []byte(arg)); err != nil {
				return err
			}
			values.WriteString("<string>" + b.String() + "</string>")
		}
		var servicePath bytes.Buffer
		_ = xml.EscapeText(&servicePath, []byte(os.Getenv("PATH")))
		var log bytes.Buffer
		_ = xml.EscapeText(&log, []byte(filepath.Join(o.stateDir, "service.log")))
		body = `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>` + serviceName + `</string><key>ProgramArguments</key><array>` + values.String() + `</array><key>EnvironmentVariables</key><dict><key>PATH</key><string>` + servicePath.String() + `</string></dict><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>StandardOutPath</key><string>` + log.String() + `</string><key>StandardErrorPath</key><string>` + log.String() + `</string></dict></plist>`
	case "linux":
		config, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		path = filepath.Join(config, "systemd", "user", "crabbox-serve.service")
		quoted := make([]string, len(commandArgs))
		for i, arg := range commandArgs {
			arg = strings.ReplaceAll(arg, "%", "%%")
			arg = strings.ReplaceAll(arg, "$", "$$")
			quoted[i] = strconv.Quote(arg)
		}
		body = "[Unit]\nDescription=Crabbox served capacity\nAfter=network-online.target\n[Service]\nExecStart=" + strings.Join(quoted, " ") + "\nRestart=on-failure\nRestartSec=5\n[Install]\nWantedBy=default.target\n"
	default:
		return fmt.Errorf("serve service installation supports macOS and Linux only")
	}
	run := func(name string, args ...string) error {
		result, err := rt.Exec.Run(ctx, core.LocalCommandRequest{Name: name, Args: args, Stdout: rt.Stdout, Stderr: rt.Stderr})
		if err != nil {
			return err
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("%s exited %d", name, result.ExitCode)
		}
		return nil
	}
	if action == "uninstall" {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return nil
		}
		if runtime.GOOS == "darwin" {
			err = run("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid()), path)
		} else {
			err = run("systemctl", "--user", "disable", "--now", "crabbox-serve.service")
		}
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if runtime.GOOS == "linux" {
			return run("systemctl", "--user", "daemon-reload")
		}
		return nil
	}
	if _, err := parseMemory(o.memory); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("service already installed at %s; uninstall before replacing it", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(o.stateDir, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		return run("launchctl", "bootstrap", "gui/"+strconv.Itoa(os.Getuid()), path)
	}
	if err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return run("systemctl", "--user", "enable", "--now", "crabbox-serve.service")
}

// Services cannot inherit a caller's environment, so retain effective nonsecret options.
func serviceArguments(executable string, o options) []string {
	args := []string{executable, "serve", "--cpus", strconv.Itoa(o.cpus), "--memory", o.memory, "--max-leases", strconv.Itoa(o.maxLeases), "--ssh-port", strconv.Itoa(o.sshPort), "--name", o.name, "--runtime", o.runtime, "--poll-interval", o.interval.String(), "--state-dir", o.stateDir}
	for _, address := range o.addresses {
		args = append(args, "--advertise-address", address)
	}
	if o.noLAN {
		args = append(args, "--no-lan-candidates")
	}
	if o.noCache {
		args = append(args, "--no-cache-volumes")
	}
	if o.drain {
		args = append(args, "--drain-on-exit")
	}
	return args
}
