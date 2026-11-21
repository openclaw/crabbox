package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Reuse only a complete matching table whose recorded processes still own its
// forwards. An occupied port or an old env file does not establish ownership.
func runningPondMeshSummary(home, pond string, members []pondMember, daemonOnly bool) (pondMeshSummary, bool, error) {
	hosts, env, err := pondMeshHostsAndEnvPaths(home, pond)
	if err != nil {
		return pondMeshSummary{}, false, err
	}
	names := []string{pondMeshDaemonFileName}
	if !daemonOnly {
		names = append(names, pondMeshForegroundFileName)
	}
	type key struct {
		lease, peer string
		port        int
	}
	wanted := map[key]bool{}
	for _, m := range members {
		for _, port := range m.Ports {
			wanted[key{m.Lease, m.Name, port}] = true
		}
	}
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(filepath.Dir(hosts), name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return pondMeshSummary{}, false, err
		}
		var state pondMeshDaemonState
		if err := json.Unmarshal(data, &state); err != nil {
			return pondMeshSummary{}, false, fmt.Errorf("parse pond forward state: %w", err)
		}
		if state.Pond != pond || len(state.Forwards) == 0 || len(state.Forwards) != len(wanted) {
			continue
		}
		matches := true
		seen := map[key]bool{}
		for _, fwd := range state.Forwards {
			k := key{fwd.LeaseID, fwd.Peer, fwd.RemotePort}
			if !wanted[k] || seen[k] {
				matches = false
				break
			}
			seen[k] = true
		}
		if !matches {
			continue
		}
		commands := map[string]string{}
		for _, process := range pondMeshDaemonProcesses(state) {
			command, alive, err := pondMeshDaemonProcessCommand(process.PID)
			if err != nil {
				return pondMeshSummary{}, false, err
			}
			if alive && isPondMeshDaemonCommand(command, process.Forward) {
				commands[process.Forward.LeaseID] = command
			}
		}
		for _, fwd := range state.Forwards {
			if !isPondMeshDaemonCommand(commands[fwd.LeaseID], fwd) {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		_, exports := renderPondMeshEnvFile(state.Forwards)
		return pondMeshSummary{HostsPath: hosts, EnvPath: env, Exports: exports, Forwards: state.Forwards}, true, nil
	}
	return pondMeshSummary{}, false, nil
}
