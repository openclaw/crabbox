package cli

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

//go:embed wayvnc_relay.py
var wayVNCRelayScript string

type wayVNCBinding struct {
	Client string `json:"client"`
	Server string `json:"server"`
	Path   string `json:"path,omitempty"`
}

type wayVNCRetirement struct {
	Type      string   `json:"type"`
	Request   string   `json:"request"`
	Server    string   `json:"server"`
	Successor string   `json:"successor"`
	Clients   []string `json:"clients"`
}

type wayVNCRelayConn struct {
	net.Conn
	binding wayVNCBinding
	target  SSHTarget
	cancel  context.CancelFunc
	once    sync.Once
}

func (c *wayVNCRelayConn) Close() error {
	c.once.Do(c.cancel)
	return c.Conn.Close()
}

func dialWayVNCRelay(ctx context.Context, target SSHTarget) (*wayVNCRelayConn, error) {
	processCtx, cancel := context.WithCancel(ctx)
	startup := time.AfterFunc(10*time.Second, cancel)
	cmd := sshCommandContext(processCtx, target, sshArgsWithOptions(target, "python3 -c "+shellQuote(wayVNCRelayScript), "5", "1")...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		startup.Stop()
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		startup.Stop()
		cancel()
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		startup.Stop()
		cancel()
		return nil, err
	}
	reader := bufio.NewReaderSize(stdout, 4096)
	line, err := reader.ReadSlice('\n')
	var binding wayVNCBinding
	if err == nil {
		err = json.Unmarshal(line, &binding)
	}
	if err == nil && (binding.Client == "" || binding.Server == "" || !strings.HasPrefix(binding.Path, "/tmp/crabbox-runtime-") || !strings.HasSuffix(binding.Path, "/control")) {
		err = fmt.Errorf("invalid WayVNC relay binding")
	}
	if !startup.Stop() || err != nil {
		cancel()
		_ = cmd.Wait()
		return nil, fmt.Errorf("WayVNC relay unavailable")
	}
	local, relay := net.Pipe()
	conn := &wayVNCRelayConn{Conn: local, binding: binding, target: target, cancel: cancel}
	go func() { _, _ = io.Copy(stdin, relay); _ = stdin.Close() }()
	go func() {
		_, _ = io.Copy(relay, reader)
		_ = relay.Close()
		cancel()
		_ = cmd.Wait()
	}()
	return conn, nil
}

func (c *wayVNCRelayConn) retire(ctx context.Context, request wayVNCRetirement) bool {
	if request.Server != c.binding.Server || len(request.Clients) > 128 {
		return false
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return false
	}
	// The private relay owns the unchanged WayVNC control connection. A new SSH
	// command only talks to that relay; it never rebinds a process-local client ID.
	script := `import json,socket,sys
s=socket.socket(socket.AF_UNIX)
s.settimeout(6)
s.connect(sys.argv[1])
s.sendall(sys.argv[2].encode()+b"\n")
print(s.makefile("r").readline(8193),end="")
`
	out, err := runSSHOutput(ctx, c.target, "python3 -c "+shellQuote(script)+" "+shellQuote(c.binding.Path)+" "+shellQuote(string(payload)))
	var response struct {
		Request string `json:"request"`
		Retired bool   `json:"retired"`
	}
	return err == nil && json.Unmarshal([]byte(out), &response) == nil && response.Request == request.Request && response.Retired
}

func (b *webVNCBridge) retireWayVNC(ctx context.Context, data []byte) bool {
	var request wayVNCRetirement
	if json.Unmarshal(data, &request) != nil || request.Type != "wayvnc_retire" {
		return false
	}
	b.remoteRetirement.Lock()
	defer b.remoteRetirement.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 7*time.Second)
	defer cancel()
	retired := false
	if relay, ok := b.tcp.(*wayVNCRelayConn); ok && request.Request != "" && len(request.Request) <= 64 {
		retired = relay.retire(ctx, request)
	}
	response, _ := json.Marshal(map[string]any{"type": "wayvnc_retired", "request": request.Request, "retired": retired})
	_ = b.ws.Write(ctx, websocket.MessageText, response)
	return true
}
