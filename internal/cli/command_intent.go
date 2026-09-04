package cli

import (
	"errors"
	"slices"
)

// CommandIntent separates command meaning from an adapter's execution transport.
// Its arguments/source are owned snapshots; providers still choose the shell,
// working directory, environment, and any outer command wrapping.
type CommandIntent struct {
	args   []string
	source string
	shell  bool
}

func ParseCommandIntent(command []string, shellMode bool, literalArgs map[int]bool) (CommandIntent, error) {
	if len(command) == 0 {
		return CommandIntent{}, errors.New("missing command")
	}
	if shellMode || shouldUseShellWithLiteralArgs(command, literalArgs) {
		return CommandIntent{
			shell:  true,
			source: runCommandShellStringWithLiteralArgs(command, shellMode, literalArgs),
		}, nil
	}
	return CommandIntent{args: slices.Clone(command)}, nil
}

// Argv applies the transport's shell prefix only to shell intent, including an
// explicitly empty shell source. Both the prefix and literal arguments are copied.
func (c CommandIntent) Argv(shellPrefix ...string) []string {
	if c.shell {
		return append(slices.Clone(shellPrefix), c.source)
	}
	return slices.Clone(c.args)
}
