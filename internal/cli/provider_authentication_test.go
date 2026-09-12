package cli

import "testing"

func TestDirectProviderAuthenticationCopiesMethods(t *testing.T) {
	methods := []ProviderAuthenticationMethod{ProviderAuthenticationAPIKey, ProviderAuthenticationCLI}
	got := DirectProviderAuthentication(methods...)
	methods[0] = ProviderAuthenticationNone
	if len(got) != 1 || got[0].Route != "direct" || len(got[0].Methods) != 2 || got[0].Methods[0] != ProviderAuthenticationAPIKey || got[0].Methods[1] != ProviderAuthenticationCLI {
		t.Fatalf("declaration did not preserve its method snapshot: %#v", got)
	}
	got[0].Methods[1] = ProviderAuthenticationSSH
	if methods[1] != ProviderAuthenticationCLI {
		t.Fatal("metadata mutation changed caller-owned methods")
	}
}
