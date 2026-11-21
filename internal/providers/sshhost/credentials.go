package sshhost

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/flock"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/privatefile"
	"golang.org/x/crypto/ssh"
)

type hostDescriptor struct {
	Version      int      `json:"version"`
	Coordinator  string   `json:"coordinator"`
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Target       string   `json:"target"`
	Architecture string   `json:"architecture"`
	Runtime      string   `json:"runtime"`
	Template     string   `json:"template"`
	Capabilities []string `json:"capabilities"`
	HostKey      string   `json:"hostKey"`
}
type hostCleanupBinding struct {
	LeaseID          string `json:"leaseID"`
	Generation       int    `json:"generation"`
	AccessGeneration int    `json:"accessGeneration"`
}

func (b *hostCleanupBinding) matches(a *hostAssignment) bool {
	return b != nil && a != nil && b.LeaseID == a.LeaseID && b.Generation == a.Generation && b.AccessGeneration == a.AccessGeneration
}

type hostCredentialIdentity struct {
	Purpose    string              `json:"purpose"`
	Cleanup    *hostCleanupBinding `json:"cleanup,omitempty"`
	ID         string              `json:"id"`
	Owner      string              `json:"owner"`
	Org        string              `json:"org"`
	HostKey    string              `json:"hostKey"`
	Generation int                 `json:"generation"`
	ExpiresAt  string              `json:"expiresAt"`
}
type hostCredentialEnvelope struct {
	Purpose    string                  `json:"purpose"`
	Cleanup    *hostCleanupBinding     `json:"cleanup,omitempty"`
	Version    int                     `json:"version"`
	Descriptor hostDescriptor          `json:"descriptor"`
	Token      string                  `json:"token"`
	Generation int                     `json:"generation"`
	ExpiresAt  string                  `json:"expiresAt"`
	Identity   *hostCredentialIdentity `json:"identity,omitempty"`
}
type hostCredentialOptions struct {
	cleanup                                                                        *hostCleanupBinding
	action, dir, id, name, template, capabilities, descriptor, output, coordinator string
	stdin                                                                          bool
	ttl                                                                            time.Duration
	expectedGeneration                                                             int
}

var hostLeasePattern = regexp.MustCompile(`^cbx_[a-f0-9]{12}$`)

var hostUUIDPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$`)

func hostCoordinator(cfg core.Config, state hostState) (*core.CoordinatorClient, error) {
	if state.AuthMode != "" || state.Descriptor != nil || state.Credential != nil {
		if state.AuthMode != "host" {
			return nil, fmt.Errorf("invalid scoped host authentication mode")
		}
		if state.Credential == nil || !validCredentialEnvelope(*state.Credential) || state.Credential.Descriptor.Coordinator != state.Coordinator || state.Credential.Descriptor.ID != state.ID {
			return nil, fmt.Errorf("scoped host credential missing or invalid; install a current credential")
		}
		key, err := publicFrontDoorKey(state)
		if err != nil || key != state.Credential.Descriptor.HostKey {
			return nil, fmt.Errorf("host credential key mismatch")
		}
		// Never inherit a user's bearer, token command, Access credentials, or login destination.
		return core.NewCoordinatorClient(core.Config{Coordinator: state.Coordinator, CoordToken: state.Credential.Token})
	}
	return core.NewCoordinatorClient(cfg)
}

func publicFrontDoorKey(state hostState) (string, error) {
	signer, err := ssh.ParsePrivateKey([]byte(state.FrontDoorKey))
	if err != nil {
		return "", fmt.Errorf("invalid host front-door key")
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), nil
}

func validateDescriptor(d hostDescriptor) error {
	if d.Version != 1 || !hostUUIDPattern.MatchString(d.ID) || d.Name == "" || d.Template == "" ||
		(d.Target != "linux" && d.Target != "windows") || (d.Architecture != "amd64" && d.Architecture != "arm64") ||
		(d.Target == "windows" && d.Runtime != "mxc-isolation-session") || (d.Target == "linux" && d.Runtime != "native-user") {
		return fmt.Errorf("invalid prepared host descriptor")
	}
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(d.HostKey))
	if err != nil || len(rest) != 0 || key.Type() != ssh.KeyAlgoED25519 {
		return fmt.Errorf("invalid descriptor host key")
	}
	c, err := core.NewCoordinatorClient(core.Config{Coordinator: d.Coordinator})
	if err != nil || c.BaseURL != d.Coordinator {
		return fmt.Errorf("invalid descriptor coordinator URL")
	}
	return nil
}

func validCredentialEnvelope(c hostCredentialEnvelope) bool {
	if (c.Purpose != "agent" && c.Purpose != "cleanup") || (c.Purpose == "agent" && c.Cleanup != nil) || (c.Purpose == "cleanup" && (c.Cleanup == nil || !hostLeasePattern.MatchString(c.Cleanup.LeaseID) || c.Cleanup.Generation < 1 || c.Cleanup.AccessGeneration < 0)) || c.Version != 1 || c.Generation < 1 || c.Generation > 999999999 || validateDescriptor(c.Descriptor) != nil {
		return false
	}
	prefix := fmt.Sprintf("cbxh_%s_%d_", c.Descriptor.ID, c.Generation)
	if !strings.HasPrefix(c.Token, prefix) {
		return false
	}
	secret, err := hex.DecodeString(strings.TrimPrefix(c.Token, prefix))
	if err != nil || len(secret) != 32 {
		return false
	}
	expires, err := time.Parse(time.RFC3339, c.ExpiresAt)
	return err == nil && !expires.IsZero()
}

func credentialUnexpired(c hostCredentialEnvelope) bool {
	expires, err := time.Parse(time.RFC3339, c.ExpiresAt)
	return err == nil && expires.After(time.Now())
}

func credentialIdentityMatches(c hostCredentialEnvelope, identity *hostCredentialIdentity) bool {
	return identity != nil && identity.Purpose == c.Purpose && reflect.DeepEqual(identity.Cleanup, c.Cleanup) && identity.ID == c.Descriptor.ID && identity.HostKey == c.Descriptor.HostKey &&
		identity.Owner != "" && identity.Owner != "unknown" && identity.Org != "" && identity.Generation == c.Generation && identity.ExpiresAt == c.ExpiresAt
}

func runCredentialCommand(ctx context.Context, cfg core.Config, rt core.Runtime, o hostCredentialOptions, input io.Reader) error {
	if o.action == "enroll" || o.action == "reissue" || o.action == "recover" {
		return enrollHostDescriptor(ctx, cfg, rt, o)
	}
	if o.action == "revoke" {
		if !hostUUIDPattern.MatchString(o.id) {
			return fmt.Errorf("--id must be the host UUID")
		}
		c, err := core.NewAdminCoordinatorClient(cfg)
		if err != nil {
			return err
		}
		var out json.RawMessage
		if err = c.AgentRequest(ctx, "POST", "/v1/ssh-hosts/"+o.id+"/credential/revoke", map[string]any{}, &out); err != nil {
			return err
		}
		_, err = fmt.Fprintln(rt.Stdout, string(out))
		return err
	}
	if !filepath.IsAbs(o.dir) {
		return fmt.Errorf("host state directory must be absolute")
	}
	if err := secureStateDir(o.dir); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(o.dir, "agent.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("host agent already owns this state")
	}
	defer lock.Close()
	path := filepath.Join(o.dir, "state.json")
	var state hostState
	data, err := readPrivateHostFile(path)
	if err == nil {
		if err = json.Unmarshal(data, &state); err != nil {
			return fmt.Errorf("invalid private host state")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if o.action == "prepare" {
		if state.Assignment != nil || state.Runtime != nil {
			return fmt.Errorf("host custody must be settled before preparation")
		}
		if state.ID != "" {
			if state.Descriptor == nil {
				return fmt.Errorf("legacy host already enrolled; retain its existing state")
			}
			if o.coordinator != "" && o.coordinator != state.Coordinator {
				return fmt.Errorf("host state is bound to a different coordinator")
			}
			return json.NewEncoder(rt.Stdout).Encode(state.Descriptor)
		}
		if o.name == "" || o.template == "" {
			return fmt.Errorf("--name and --template are required")
		}
		url := o.coordinator
		if url == "" {
			url = cfg.Coordinator
		}
		c, err := core.NewCoordinatorClient(core.Config{Coordinator: url})
		if err != nil {
			return err
		}
		var uuid [16]byte
		if _, err = rand.Read(uuid[:]); err != nil {
			return err
		}
		uuid[6] = (uuid[6] & 15) | 64
		uuid[8] = (uuid[8] & 63) | 128
		h := hex.EncodeToString(uuid[:])
		state.ID = h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
		state.Coordinator = c.BaseURL
		state.AuthMode = "host"
		state.FrontDoorKey, _, err = generateHostKey()
		if err != nil {
			return err
		}
		key, err := publicFrontDoorKey(state)
		if err != nil {
			return err
		}
		kind := "native-user"
		if runtime.GOOS == "windows" {
			kind = "mxc-isolation-session"
		}
		caps := []string{}
		for _, cap := range strings.Split(o.capabilities, ",") {
			if cap = strings.TrimSpace(cap); cap != "" {
				caps = append(caps, cap)
			}
		}
		state.Descriptor = &hostDescriptor{Version: 1, Coordinator: c.BaseURL, ID: state.ID, Name: o.name, Target: runtime.GOOS, Architecture: runtime.GOARCH, Runtime: kind, Template: o.template, Capabilities: caps, HostKey: key}
		if err = validateDescriptor(*state.Descriptor); err != nil {
			return err
		}
		if err = writePrivateJSON(path, state); err != nil {
			return err
		}
		return json.NewEncoder(rt.Stdout).Encode(state.Descriptor)
	}
	if !o.stdin {
		return fmt.Errorf("install requires --credential-stdin")
	}
	if state.Descriptor == nil || state.AuthMode != "host" {
		return fmt.Errorf("prepare this host first")
	}
	var credential hostCredentialEnvelope
	if err = decodeHostJSON(input, &credential); err != nil || !validCredentialEnvelope(credential) || !credentialUnexpired(credential) {
		return fmt.Errorf("invalid host credential envelope")
	}
	key, err := publicFrontDoorKey(state)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(credential.Descriptor, *state.Descriptor) || key != credential.Descriptor.HostKey || state.ID != credential.Descriptor.ID || state.Coordinator != credential.Descriptor.Coordinator {
		return fmt.Errorf("credential does not match this prepared host")
	}
	if credential.Purpose == "cleanup" {
		if !credential.Cleanup.matches(state.Assignment) {
			return fmt.Errorf("cleanup credential does not match retained host custody")
		}
	} else if state.Assignment != nil || state.Runtime != nil {
		return fmt.Errorf("host custody must be settled before installing a credential")
	}
	if !credentialIdentityMatches(credential, credential.Identity) {
		return fmt.Errorf("credential has no matching enrollment receipt; retry admin enrollment")
	}
	if state.Credential != nil && credential.Generation < state.Credential.Generation {
		return fmt.Errorf("credential generation cannot go backwards")
	}
	candidate := state
	candidate.Credential = &credential
	c, err := hostCoordinator(core.Config{}, candidate)
	if err != nil {
		return err
	}
	var response struct {
		CleanupComplete bool                    `json:"cleanupComplete"`
		Identity        *hostCredentialIdentity `json:"identity"`
		Assignment      *hostAssignment         `json:"assignment"`
	}
	if err = c.AgentRequest(ctx, "POST", "/v1/ssh-hosts/"+state.ID+"/heartbeat", map[string]any{"hostKey": key}, &response); err != nil {
		return fmt.Errorf("host credential verification failed")
	}
	if !credentialIdentityMatches(credential, response.Identity) || !reflect.DeepEqual(credential.Identity, response.Identity) {
		return fmt.Errorf("coordinator enrollment identity mismatch")
	}
	if credential.Purpose == "cleanup" {
		if (response.Assignment == nil && !response.CleanupComplete) || (response.Assignment != nil && (!credential.Cleanup.matches(response.Assignment) || response.Assignment.Operation != "clean")) {
			return fmt.Errorf("coordinator cleanup custody mismatch")
		}
	} else if response.Assignment != nil {
		return fmt.Errorf("coordinator still holds host custody; retain existing state")
	}
	if err = writePrivateJSON(path, candidate); err != nil {
		return err
	}
	_, err = fmt.Fprintln(rt.Stdout, "host credential installed")
	return err
}

func enrollHostDescriptor(ctx context.Context, cfg core.Config, rt core.Runtime, o hostCredentialOptions) error {
	purpose := "agent"
	if o.action == "recover" {
		purpose = "cleanup"
	}
	if purpose == "cleanup" && (o.cleanup == nil || !hostLeasePattern.MatchString(o.cleanup.LeaseID) || o.cleanup.Generation < 1 || o.cleanup.AccessGeneration < 0) {
		return fmt.Errorf("invalid cleanup custody binding")
	}

	if o.descriptor == "" || o.output == "" {
		return fmt.Errorf("--descriptor and --credential-out are required")
	}
	if o.action == "recover" && o.ttl > 24*time.Hour {
		return fmt.Errorf("cleanup credential lifetime cannot exceed 24 hours")
	}
	if o.ttl <= 0 || o.ttl > 365*24*time.Hour {
		return fmt.Errorf("credential lifetime must be between zero and 365 days")
	}
	data, err := os.ReadFile(o.descriptor)
	if err != nil {
		return err
	}
	var descriptor hostDescriptor
	if json.Unmarshal(data, &descriptor) != nil || validateDescriptor(descriptor) != nil {
		return fmt.Errorf("invalid prepared host descriptor")
	}
	c, err := core.NewAdminCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if c.BaseURL != descriptor.Coordinator {
		return fmt.Errorf("descriptor belongs to a different coordinator")
	}
	generation := 1
	if o.action == "reissue" || o.action == "recover" {
		if o.expectedGeneration < 1 || o.expectedGeneration >= 999999999 {
			return fmt.Errorf("--expected-generation is required for reissue")
		}
		generation = o.expectedGeneration + 1
	}
	var credential hostCredentialEnvelope
	data, err = readPrivateHostFile(o.output)
	if err == nil {
		if json.Unmarshal(data, &credential) != nil || !validCredentialEnvelope(credential) || !credentialUnexpired(credential) || !reflect.DeepEqual(credential.Descriptor, descriptor) || credential.Generation != generation || !reflect.DeepEqual(credential.Cleanup, o.cleanup) || credential.Purpose != purpose {
			return fmt.Errorf("credential output already exists with different or expired enrollment; choose a new private output path")
		}
	} else if os.IsNotExist(err) {
		var secret [32]byte
		if _, err = rand.Read(secret[:]); err != nil {
			return err
		}
		credential = hostCredentialEnvelope{Purpose: purpose, Cleanup: o.cleanup, Version: 1, Descriptor: descriptor, Generation: generation, Token: fmt.Sprintf("cbxh_%s_%d_%s", descriptor.ID, generation, hex.EncodeToString(secret[:])), ExpiresAt: time.Now().UTC().Add(o.ttl).Truncate(time.Second).Format(time.RFC3339)}
		if err = createPrivateHostJSON(o.output, credential); err != nil {
			return err
		}
	} else {
		return err
	}
	digest := sha256.Sum256([]byte(credential.Token))
	credentialInput := struct {
		Purpose    string              `json:"purpose"`
		Cleanup    *hostCleanupBinding `json:"cleanup,omitempty"`
		Version    int                 `json:"version"`
		Generation int                 `json:"generation"`
		TokenHash  string              `json:"tokenHash"`
		ExpiresAt  string              `json:"expiresAt"`
	}{credential.Purpose, credential.Cleanup, 1, generation, hex.EncodeToString(digest[:]), credential.ExpiresAt}
	var body map[string]any
	raw, _ := json.Marshal(descriptor)
	_ = json.Unmarshal(raw, &body)
	body["credential"] = credentialInput
	route := "/v1/ssh-hosts"
	if o.action == "reissue" || o.action == "recover" {
		body["expectedGeneration"] = o.expectedGeneration
		route += "/" + descriptor.ID + "/credential/" + o.action
	}
	var response struct {
		Identity *hostCredentialIdentity `json:"identity"`
	}
	if err = c.AgentRequest(ctx, "POST", route, body, &response); err != nil {
		return err
	}
	if !credentialIdentityMatches(credential, response.Identity) {
		return fmt.Errorf("coordinator returned an invalid enrollment receipt; private output retained for retry")
	}
	credential.Identity = response.Identity
	if err = writePrivateJSON(o.output, credential); err != nil {
		return err
	}
	_, err = fmt.Fprintf(rt.Stdout, "host %s credential generation %d saved; inventory remains disabled\n", descriptor.ID, generation)
	return err
}

func decodeHostJSON(input io.Reader, out any) error {
	data, err := io.ReadAll(io.LimitReader(input, 65537))
	if err != nil {
		return err
	}
	if len(data) > 65536 {
		return fmt.Errorf("host credential input too large")
	}
	return json.Unmarshal(data, out)
}

func readPrivateHostFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("host credential/state must be a regular private file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, actual) {
		return nil, fmt.Errorf("host credential/state changed while opening")
	}
	if err = verifyPrivateHostFile(f); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if len(data) > 65536 {
		return nil, fmt.Errorf("host credential/state too large")
	}
	return data, err
}

func createPrivateHostJSON(path string, value any) error {
	f, err := privatefile.CreateTemp(filepath.Dir(path), ".host-credential-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(value)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// Linking fails if the destination exists; no concurrent enrollment can overwrite its token.
	return os.Link(f.Name(), path)
}
