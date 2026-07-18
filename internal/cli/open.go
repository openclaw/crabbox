package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
)

const editorHandoffSchema = "crabbox/editor-handoff/v1"

type editorTargetCapabilities struct {
	supportsWindows       bool
	supportsTokenUsername bool
}

type editorHandoffSpec struct {
	displayName         string
	connectInstructions string
	targets             editorTargetCapabilities
}

type editorHandoffOutput struct {
	Schema            string `json:"schema"`
	Editor            string `json:"editor"`
	DisplayName       string `json:"displayName"`
	LeaseID           string `json:"leaseId,omitempty"`
	SSHCommand        string `json:"sshCommand"`
	RemoteFolder      string `json:"remoteFolder"`
	HydratedByActions bool   `json:"hydratedByActions"`
	LeaseActivity     string `json:"leaseActivity"`
	HardTTLApplies    bool   `json:"hardTTLApplies"`
	ReleaseCommand    string `json:"releaseCommand,omitempty"`
}

var editorHandoffSpecs = map[string]editorHandoffSpec{
	"zed": {
		displayName: "Zed Remote Projects",
		connectInstructions: "1. Open Remote Projects and select Connect New Server.\n" +
			"2. Paste the SSH command shown above.\n" +
			"3. Open the remote folder shown above.\n",
		targets: editorTargetCapabilities{
			supportsWindows:       false,
			supportsTokenUsername: false,
		},
	},
}

func (a App) open(ctx context.Context, args []string) error {
	var editorName string
	var editor editorHandoffSpec
	var jsonOut bool
	resolved, err := a.resolveSSHCommandTargetWithOptions(ctx, "open", args, false, sshCommandResolveOptions{
		registerFlags: func(fs *flag.FlagSet) {
			fs.StringVar(&editorName, "editor", "", "editor: "+strings.Join(editorHandoffNames(), ", "))
			fs.BoolVar(&jsonOut, "json", false, "print a machine-readable handoff")
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
	return a.runEditorHandoff(ctx, editorName, editor, resolved, jsonOut)
}

func editorHandoffNames() []string {
	names := make([]string, 0, len(editorHandoffSpecs))
	for name := range editorHandoffSpecs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (a App) runEditorHandoff(ctx context.Context, editorName string, editor editorHandoffSpec, resolved resolvedSSHCommandTarget, jsonOut bool) error {
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
	handoff := newEditorHandoffOutput(
		editorName,
		editor,
		resolved.Config,
		resolved.Lease,
		folder,
		hydratedByActions,
	)
	if jsonOut {
		if err := json.NewEncoder(a.Stdout).Encode(handoff); err != nil {
			return err
		}
	} else {
		writeEditorInstructions(a.Stdout, editor, handoff)
	}

	<-ctx.Done()
	return nil
}

func newEditorHandoffOutput(editorName string, editor editorHandoffSpec, cfg Config, lease LeaseTarget, folder string, hydratedByActions bool) editorHandoffOutput {
	leaseID := strings.TrimSpace(lease.LeaseID)
	releaseCommand := ""
	if leaseID != "" {
		releaseCommand = runStopCommand(cfg, leaseID)
	}
	return editorHandoffOutput{
		Schema:            editorHandoffSchema,
		Editor:            editorName,
		DisplayName:       editor.displayName,
		LeaseID:           leaseID,
		SSHCommand:        editorSSHCommandLine(lease.SSH),
		RemoteFolder:      folder,
		HydratedByActions: hydratedByActions,
		LeaseActivity:     "foreground",
		HardTTLApplies:    editorHandoffHardTTLApplies(cfg, lease),
		ReleaseCommand:    releaseCommand,
	}
}

func editorHandoffHardTTLApplies(cfg Config, lease LeaseTarget) bool {
	provider := firstNonBlank(lease.Server.Provider, cfg.Provider)
	if isStaticProvider(provider) {
		return false
	}
	_, ok := parseLeaseLabelTime(lease.Server.Labels["expires_at"])
	return ok
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

func writeEditorInstructions(out io.Writer, editor editorHandoffSpec, handoff editorHandoffOutput) {
	fmt.Fprintf(out, "%s is ready.\n\n", editor.displayName)
	fmt.Fprintln(out, "SSH command:")
	fmt.Fprintf(out, "  %s\n\n", handoff.SSHCommand)
	fmt.Fprintln(out, "Remote folder:")
	fmt.Fprintf(out, "  %s\n\n", handoff.RemoteFolder)
	fmt.Fprintln(out, "Next:")
	fmt.Fprint(out, editor.connectInstructions)
	fmt.Fprintln(out)
	if handoff.HardTTLApplies {
		fmt.Fprintln(out, "Lease activity: active while this process runs; the hard TTL still applies.")
	} else {
		fmt.Fprintln(out, "Lease activity: active while this process runs.")
	}
	fmt.Fprintln(out, "Press Ctrl-C when the editor session is finished.")
	if handoff.ReleaseCommand != "" {
		fmt.Fprintf(out, "Release command: %s\n", handoff.ReleaseCommand)
	} else {
		fmt.Fprintln(out, "Stopping this process does not release the lease.")
	}
}
