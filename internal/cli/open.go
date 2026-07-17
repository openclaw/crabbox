package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

type editorTargetCapabilities struct {
	supportsWindows       bool
	supportsTokenUsername bool
}

type editorHandoffSpec struct {
	displayName         string
	connectInstructions string
	targets             editorTargetCapabilities
}

var editorHandoffSpecs = map[string]editorHandoffSpec{
	"zed": {
		displayName: "Zed Remote Projects",
		connectInstructions: "1. Open Remote Projects and select Connect New Server.\n" +
			"2. Paste: %s\n" +
			"3. Open: %s\n" +
			"Keep this process running to maintain lease activity; press Ctrl-C when the Zed session is finished.\n",
		targets: editorTargetCapabilities{
			supportsWindows:       false,
			supportsTokenUsername: false,
		},
	},
}

func (a App) open(ctx context.Context, args []string) error {
	var editorName string
	var editor editorHandoffSpec
	resolved, err := a.resolveSSHCommandTargetWithOptions(ctx, "open", args, false, sshCommandResolveOptions{
		registerFlags: func(fs *flag.FlagSet) {
			fs.StringVar(&editorName, "editor", "", "editor: "+strings.Join(editorHandoffNames(), ", "))
		},
		validateFlags: func() error {
			editorName = strings.ToLower(strings.TrimSpace(editorName))
			if editorName == "" {
				return exit(2, "usage: crabbox open --editor=<name> --id <lease-id-or-slug>")
			}
			var ok bool
			editor, ok = editorHandoffSpecs[editorName]
			if !ok {
				return exit(2, "unknown editor %q; available editors: %s", editorName, strings.Join(editorHandoffNames(), ", "))
			}
			return nil
		},
	})
	if err != nil {
		return err
	}
	return a.runEditorHandoff(ctx, editorName, editor, resolved)
}

func editorHandoffNames() []string {
	names := make([]string, 0, len(editorHandoffSpecs))
	for name := range editorHandoffSpecs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (a App) runEditorHandoff(ctx context.Context, editorName string, editor editorHandoffSpec, resolved resolvedSSHCommandTarget) error {
	if err := validateEditorTarget(editorName, editor, resolved.Config, resolved.Lease.SSH); err != nil {
		return err
	}
	repo, err := findRepo()
	if err != nil {
		return err
	}
	_, folder, hydratedByActions := codeWorkspace(ctx, resolved.Lease.SSH, resolved.Config, resolved.Lease.LeaseID, repo)
	if err := runSSHQuiet(ctx, resolved.Lease.SSH, "test -d "+shellQuote(folder)); err != nil {
		return exit(5, "remote folder %q is not ready; sync it first with: crabbox run --id %s --sync-only", folder, resolved.Lease.LeaseID)
	}
	if hydratedByActions {
		fmt.Fprintf(a.Stderr, "using GitHub Actions workspace for %s\n", resolved.Lease.LeaseID)
	}

	stopLeaseActivity := a.startInteractiveSSHLeaseActivity(ctx, resolved.Config, resolved.Lease)
	defer stopLeaseActivity()
	writeEditorInstructions(a.Stdout, editor, resolved.Lease.SSH, folder)

	<-ctx.Done()
	return nil
}

func validateEditorTarget(editorName string, editor editorHandoffSpec, cfg Config, target SSHTarget) error {
	if target.AuthSecret && !editor.targets.supportsTokenUsername {
		return exit(2, "%s does not support token-as-username SSH credentials; use a key-based SSH provider", editorName)
	}
	if firstNonBlank(target.TargetOS, cfg.TargetOS) == targetWindows && !editor.targets.supportsWindows {
		return exit(2, "%s remote projects require a Linux or macOS target", editorName)
	}
	return nil
}

func editorSSHCommandLine(target SSHTarget) string {
	args := append(sshBaseArgs(target), target.User+"@"+target.Host)
	return "ssh " + strings.Join(shellWords(args), " ")
}

func writeEditorInstructions(out io.Writer, editor editorHandoffSpec, target SSHTarget, folder string) {
	fmt.Fprintln(out, editor.displayName)
	fmt.Fprintf(out, editor.connectInstructions, editorSSHCommandLine(target), folder)
}
