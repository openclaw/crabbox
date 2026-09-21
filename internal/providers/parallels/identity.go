package parallels

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// parallelsConnectionIdentity is the Parallels service a fixed lease's VM lives
// in, as that service reports itself. It is deliberately built only from
// attested values: a fleet entry's display name, its configured address, and
// the configured account name are how the host was addressed, not which
// host answered. Keeping `name` while repointing `host` or `user` at another
// machine therefore changes the identity and is refused, while reaching the
// same machine/account through a new address still resolves its own leases
// — which matters, because otherwise a routine address change would strand
// running VMs with no CLI route to stop them.
// AccountID binds inventory visibility to the same native UID, independently
// of the SSH login spelling.
//
// This is the Parallels analogue of the Incus daemon certificate: the part of
// the Incus scope that the client cannot choose.
type parallelsConnectionIdentity struct {
	ServerID   string `json:"serverId"`
	HardwareID string `json:"hardwareId"`
	AccountID  string `json:"accountId"`
}

// scope hashes the identity, so claims, labels, and operator-visible errors
// carry no host identifiers.
func (i parallelsConnectionIdentity) scope() string {
	data, _ := json.Marshal(i)
	return fmt.Sprintf("prlsrv1:%x", sha256.Sum256(data))
}

func parallelsScopeFromIdentity(identity core.ParallelsServerIdentity) string {
	return parallelsConnectionIdentity{ServerID: identity.ServerID, HardwareID: identity.HardwareID, AccountID: identity.AccountID}.scope()
}

func parallelsConnectionScope(ctx context.Context, client *core.ParallelsClient, _ core.Config) (string, error) {
	identity, err := client.ServerIdentity(ctx)
	if err != nil {
		return "", err
	}
	return parallelsScopeFromIdentity(identity), nil
}

// shortParallelsScope keeps errors diagnosable without printing the digest that
// a scope comparison depends on.
func shortParallelsScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return "<none>"
	}
	if digest := strings.TrimPrefix(scope, "prlsrv1:"); len(digest) > 12 {
		return digest[:12]
	}
	return scope
}

// parallelsFixedHostConfig pins a replay or release to the Parallels connection
// recorded in the durable intent. A fixed lease never re-runs fleet selection:
// its VM lives behind exactly one connection, and silently accepting another
// one would clone a second VM or delete a stranger's.
//
// An unreachable candidate is not proof that the recorded connection left the
// fleet, so an incomplete sweep retains custody instead of rebinding.
func parallelsFixedHostConfig(ctx context.Context, exec core.CommandRunner, base core.Config, leaseID, scope string) (core.Config, *core.ParallelsClient, error) {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return core.Config{}, nil, core.Exit(4, "lease_id_conflict: Parallels lease %s has no recorded host connection identity", leaseID)
	}
	var probeErrs []error
	for _, candidate := range core.ParallelsCandidateConfigs(base) {
		client := core.NewParallelsClient(candidate, exec)
		candidateScope, err := parallelsConnectionScope(ctx, client, candidate)
		if err != nil {
			probeErrs = append(probeErrs, parallelsHostError(candidate, "attest host connection", err))
			continue
		}
		if candidateScope == scope {
			return candidate, client, nil
		}
	}
	if len(probeErrs) > 0 {
		return core.Config{}, nil, fmt.Errorf("Parallels host connection identity incomplete for lease=%s connection=%s (claim and key retained): %w",
			leaseID, shortParallelsScope(scope), errors.Join(probeErrs...))
	}
	return core.Config{}, nil, core.Exit(4, "lease_id_conflict: Parallels lease %s is bound to host connection %s, which no configured fleet host attests", leaseID, shortParallelsScope(scope))
}
