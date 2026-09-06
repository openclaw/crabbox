package shared

import (
	core "github.com/openclaw/crabbox/internal/cli"
	"strings"
)

// ShellWorkspaceCommand sets up a POSIX workspace and environment, then executes
// already-classified command intent. The adapter chooses the workload shell.
func ShellWorkspaceCommand(workdir string, env map[string]string, command core.CommandIntent, shellPrefix ...string) string {
	var out strings.Builder
	out.WriteString("mkdir -p ")
	out.WriteString(core.ShellQuote(workdir))
	out.WriteString(" && cd ")
	out.WriteString(core.ShellQuote(workdir))
	for _, key := range sortedShellEnvNames(env) {
		out.WriteString(" && export ")
		out.WriteString(key)
		out.WriteByte('=')
		out.WriteString(core.ShellQuote(env[key]))
	}
	out.WriteString(" && exec ")
	out.WriteString(command.ShellCommand(shellPrefix...))
	return out.String()
}
