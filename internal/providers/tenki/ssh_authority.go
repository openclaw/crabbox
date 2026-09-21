package tenki

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/shared"
	tenkiproto "github.com/openclaw/crabbox/internal/providers/tenki/proto"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

const tenkiProductionAPI = "https://api.tenki.cloud"
const tenkiSSHAuthorityService = "/tenki.sandbox.v1.SSHGatewayClientService/"

var tenkiGatewayIdentity = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

type tenkiCredentialContext struct {
	key      string
	endpoint *url.URL
}

// Read the native CLI's credential context as a pair. In particular, SDK
// environment defaults differ from the CLI, and must not redirect this key.
func tenkiNativeCredentialContext(cfg core.Config) (tenkiCredentialContext, error) {
	var stored struct {
		Token    string `yaml:"auth_token"`
		Endpoint string `yaml:"api_endpoint"`
	}
	name := os.Getenv("TENKI_CONFIG_FILE")
	if name == "" {
		name = "config.yaml"
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\\r\n\x00") {
		return tenkiCredentialContext{}, core.Exit(2, "TENKI_CONFIG_FILE must be a filename inside the Tenki config directory")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return tenkiCredentialContext{}, fmt.Errorf("resolve Tenki config directory: %w", err)
	}
	file, err := os.Open(filepath.Join(home, ".config", "tenki", name))
	if err == nil {
		defer file.Close()
		data, readErr := io.ReadAll(io.LimitReader(file, 1<<20+1))
		if readErr != nil || len(data) > 1<<20 || yaml.Unmarshal(data, &stored) != nil {
			return tenkiCredentialContext{}, core.Exit(2, "cannot decode Tenki credential config")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return tenkiCredentialContext{}, core.Exit(2, "cannot read Tenki credential config")
	}

	key := stored.Token
	auth, authSet := os.LookupEnv("TENKI_AUTH_TOKEN")
	if authSet {
		key = auth
	}
	if !authSet || auth == "" {
		if apiKey := strings.TrimSpace(os.Getenv("TENKI_API_KEY")); apiKey != "" {
			key = apiKey
		}
	}
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, "tk_") || strings.ContainsAny(key, " \t\r\n") {
		return tenkiCredentialContext{}, core.Exit(3, "Tenki SSH authority requires a workspace API key; run tenki onboard or set TENKI_API_KEY")
	}
	endpoint := stored.Endpoint
	if value, set := os.LookupEnv("TENKI_API_ENDPOINT"); set {
		endpoint = value
	}
	if cfg.Tenki.Endpoint != "" {
		endpoint = cfg.Tenki.Endpoint
	}
	if endpoint == "" {
		endpoint = tenkiProductionAPI
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return tenkiCredentialContext{}, core.Exit(2, "Tenki SSH authority requires an HTTPS API endpoint without userinfo, query, or fragment")
	}
	return tenkiCredentialContext{key: key, endpoint: parsed}, nil
}

func tenkiCommandGateway(output tenkiSSHCommandOutput) (*url.URL, error) {
	words, err := splitTenkiShellWords(output.ProxyCommand)
	if err != nil {
		return nil, core.Exit(5, "cannot parse Tenki SSH gateway command")
	}
	var gateway, session string
	seen := map[string]bool{}
	for i := 0; i < len(words); i++ {
		name, value, equal := strings.Cut(words[i], "=")
		if name != "--gateway" && name != "--session" {
			continue
		}
		if seen[name] {
			return nil, core.Exit(5, "Tenki SSH gateway command repeats an identity option")
		}
		seen[name] = true
		if !equal {
			i++
			if i == len(words) {
				return nil, core.Exit(5, "Tenki SSH gateway command is missing an identity value")
			}
			value = words[i]
		}
		if name == "--gateway" {
			gateway = value
		} else {
			session = value
		}
	}
	parsed, err := url.Parse(gateway)
	if session != output.SessionID || err != nil || parsed.Scheme != "wss" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, core.Exit(5, "Tenki CLI must expose the exact session and a secure SSH gateway; update the Tenki CLI")
	}
	return parsed, nil
}

func tenkiGatewayAuthority(gateway *url.URL, sessionID string, records []*tenkiproto.ActiveSSHGateway) (string, []string, error) {
	var selected string
	identities := map[string]bool{}
	for _, record := range records {
		if !record.GetHealthy() {
			continue
		}
		id := record.GetGatewayId()
		if !tenkiGatewayIdentity.MatchString(id) {
			return "", nil, core.Exit(5, "Tenki returned an invalid SSH gateway identity")
		}
		endpoint, err := url.Parse(record.GetWsBridgeEndpoint())
		if err != nil {
			continue
		}
		if endpoint.Scheme == "https" || endpoint.Scheme == "" {
			endpoint.Scheme = "wss"
		}
		if endpoint.Scheme != "wss" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			continue
		}
		identities[id] = true
		// The published SDK appends this exact session path to the advertised
		// bridge. A matching hostname alone cannot bind a different route.
		endpoint.Path = path.Join(strings.TrimRight(endpoint.Path, "/"), "v1", "ssh", sessionID)
		endpoint.RawPath = ""
		if endpoint.String() != gateway.String() {
			continue
		}
		if selected != "" {
			return "", nil, core.Exit(5, "Tenki SSH gateway identity is ambiguous")
		}
		selected = id
	}
	if selected == "" {
		return "", nil, core.Exit(5, "Tenki SSH gateway is not bound by authenticated discovery")
	}
	hosts := make([]string, 0, len(identities))
	for id := range identities {
		hosts = append(hosts, id)
	}
	sort.Strings(hosts)
	return selected, hosts, nil
}

func (b *tenkiBackend) prepareSSHAuthority(ctx context.Context, cfg core.Config, output tenkiSSHCommandOutput) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if reported := strings.TrimSpace(output.KnownHostsFile); reported != "" {
		return reported, "", nil
	}
	credential, err := tenkiNativeCredentialContext(cfg)
	if err != nil {
		return "", "", err
	}
	gateway, err := tenkiCommandGateway(output)
	if err != nil {
		return "", "", err
	}
	certFile, err := os.Open(output.CertificateFile)
	if err != nil {
		return "", "", core.Exit(5, "cannot read the Tenki-issued client certificate")
	}
	certData, readErr := io.ReadAll(io.LimitReader(certFile, 1<<20+1))
	closeErr := certFile.Close()
	if readErr != nil || closeErr != nil || len(certData) > 1<<20 {
		return "", "", core.Exit(5, "cannot read the Tenki-issued client certificate")
	}
	key, _, _, rest, err := ssh.ParseAuthorizedKey(certData)
	cert, ok := key.(*ssh.Certificate)
	if err != nil || !ok || cert.CertType != ssh.UserCert || len(strings.TrimSpace(string(rest))) != 0 {
		return "", "", core.Exit(5, "Tenki CLI did not supply a single SSH user certificate")
	}
	client := b.rt.HTTP
	if client == nil {
		client = &http.Client{}
	}
	client = shared.SecureHTTPClient(client, credential.endpoint, func(*url.URL) error {
		return errors.New("Tenki SSH authority redirect left its credential origin")
	})
	base := strings.TrimRight(credential.endpoint.String(), "/") + tenkiSSHAuthorityService
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	issuer := connect.NewClient[tenkiproto.IssueSandboxSSHCertRequest, tenkiproto.IssueSandboxSSHCertResponse](client, base+"IssueSandboxSSHCert", connect.WithGRPC(), connect.WithReadMaxBytes(1<<20))
	request := connect.NewRequest(&tenkiproto.IssueSandboxSSHCertRequest{SessionId: output.SessionID, PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(cert.Key)))})
	request.Header().Set("Authorization", "Bearer "+credential.key)
	issued, err := issuer.CallUnary(ctx, request)
	if err != nil {
		return "", "", tenkiAuthorityRequestError(ctx, "certificate authority", err)
	}
	ca, _, options, trailing, err := ssh.ParseAuthorizedKey([]byte(issued.Msg.GetCaPub()))
	_, certificateCA := ca.(*ssh.Certificate)
	if err != nil || certificateCA || len(options) != 0 || len(strings.TrimSpace(string(trailing))) != 0 {
		return "", "", core.Exit(5, "Tenki returned an invalid SSH certificate authority")
	}
	discovery := connect.NewClient[tenkiproto.ListActiveSSHGatewaysRequest, tenkiproto.ListActiveSSHGatewaysResponse](client, base+"ListActiveSSHGateways", connect.WithGRPC(), connect.WithReadMaxBytes(1<<20))
	query := connect.NewRequest(&tenkiproto.ListActiveSSHGatewaysRequest{SessionId: output.SessionID})
	query.Header().Set("Authorization", "Bearer "+credential.key)
	listed, err := discovery.CallUnary(ctx, query)
	if err != nil {
		return "", "", tenkiAuthorityRequestError(ctx, "gateway identity", err)
	}
	alias, identities, err := tenkiGatewayAuthority(gateway, output.SessionID, listed.Msg.GetGateways())
	if err != nil {
		return "", "", err
	}
	trustFile, err := writeTenkiSSHAuthority(output, identities, ca)
	return trustFile, alias, err
}

func tenkiAuthorityRequestError(ctx context.Context, operation string, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	// Remote error messages may echo credentials; retain the actionable status
	// without rendering arbitrary server-provided details.
	return core.Exit(5, "Tenki SSH %s request failed (%s)", operation, connect.CodeOf(err))
}

func writeTenkiSSHAuthority(output tenkiSSHCommandOutput, identities []string, ca ssh.PublicKey) (string, error) {
	if strings.ContainsAny(output.SessionID, "\r\n\x00") {
		return "", core.Exit(5, "Tenki session identity contains a control character")
	}
	session := core.NormalizeLeaseSlug(output.SessionID)
	if session == "" {
		return "", core.Exit(5, "Tenki session identity is empty")
	}
	dir := filepath.Dir(output.CertificateFile)
	name := filepath.Join(dir, "crabbox_known_hosts_"+session)
	marker := "# Crabbox Tenki SSH authority for " + output.SessionID + "\n"
	if info, err := os.Lstat(name); err == nil {
		if !info.Mode().IsRegular() {
			return "", core.Exit(5, "Tenki SSH authority destination is not a regular file")
		}
		previous, err := os.Open(name)
		if err != nil {
			return "", core.Exit(5, "cannot read Tenki SSH authority destination")
		}
		prefix, readErr := io.ReadAll(io.LimitReader(previous, int64(len(marker))))
		closeErr := previous.Close()
		if readErr != nil || closeErr != nil || string(prefix) != marker {
			return "", core.Exit(5, "refusing to replace an unowned Tenki SSH authority file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", core.Exit(5, "cannot inspect Tenki SSH authority destination")
	}
	file, err := os.CreateTemp(dir, ".crabbox-tenki-authority-*")
	if err != nil {
		return "", fmt.Errorf("create Tenki SSH authority file: %w", err)
	}
	defer os.Remove(file.Name())
	data := marker + "@cert-authority " + strings.Join(identities, ",") + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(ca))) + "\n"
	_, writeErr := io.WriteString(file, data)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return "", fmt.Errorf("write Tenki SSH authority file: %w", err)
	}
	if err := os.Rename(file.Name(), name); err != nil {
		return "", fmt.Errorf("replace Tenki SSH authority file: %w", err)
	}
	return name, nil
}
