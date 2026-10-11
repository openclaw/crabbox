package cli

import "testing"

func TestDelegatedShellScriptAdmissionIsExplicit(t *testing.T) {
	req := RunRequest{NoSync: true, ScriptRequested: true, Script: &RunScriptSpec{Source: "stdin", Data: []byte("echo ready")}, Command: []string{"argument"}}
	ordinary := ProviderSpec{Name: "ordinary", Kind: ProviderKindDelegatedRun}
	if err := RejectDelegatedSyncOptionsForSpec(ordinary, req); err == nil {
		t.Fatal("ordinary delegated provider admitted scripts")
	}
	shell := ordinary
	shell.Features = FeatureSet{FeatureShellScriptRun}
	if err := RejectDelegatedSyncOptionsForSpec(shell, req); err != nil {
		t.Fatal(err)
	}
	module := ordinary
	module.Features = FeatureSet{FeatureModuleRun}
	if err := RejectDelegatedSyncOptionsForSpec(module, req); err == nil {
		t.Fatal("module provider accepted shell arguments")
	}
}
