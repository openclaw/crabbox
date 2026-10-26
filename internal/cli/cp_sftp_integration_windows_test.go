//go:build windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const sftpProbeSource = `package main
import ("encoding/binary";"encoding/json";"errors";"fmt";"io";"net";"os";"os/exec";"strings";"sync";"time")
func packet(r io.Reader)([]byte,error){
 h:=make([]byte,4); if _,e:=io.ReadFull(r,h);e!=nil{return nil,e}
 n:=binary.BigEndian.Uint32(h); if n<1||n>16<<20{return nil,errors.New("invalid SFTP packet size")}
 b:=make([]byte,4+int(n));copy(b,h);_,e:=io.ReadFull(r,b[4:]);return b,e
}
func run()error{
 first,e:=packet(os.Stdin);if e!=nil{return e}
 if len(first)<9||first[4]!=1{return errors.New("client did not send SSH_FXP_INIT")}
 fmt.Fprintln(os.Stderr,"sftp-probe: client INIT received")
 record:=map[string]any{"clientType":first[4],"clientVersion":binary.BigEndian.Uint32(first[5:9])}
 save:=func()error{b,e:=json.Marshal(record);if e!=nil{return e};return os.WriteFile(os.Getenv("CBX_SFTP_REPORT"),b,0600)}
 if e=save();e!=nil{return e}
 if os.Getenv("CBX_SFTP_MODE")=="noise"{_,e=os.Stdout.Write([]byte("invalid protocol output\n"));return e}
 server:=exec.Command(os.Getenv("CBX_SFTP_SERVER"),"-e","-l","ERROR","-d",os.Getenv("CBX_SFTP_ROOT"))
 for _,entry:=range os.Environ(){
  name:=strings.SplitN(entry,"=",2)[0]
  if strings.EqualFold(name,"c28fc6f98a2c44abbbd89d6a3037d0d9_POSIX_FD_STATE")||strings.EqualFold(name,"OPENSSH_STDIO_MODE"){continue}
  server.Env=append(server.Env,entry)
 }
 server.Env=append(server.Env,"OPENSSH_STDIO_MODE=nonsock_sync")
 server.Stderr=os.Stderr
 input,e:=server.StdinPipe();if e!=nil{return e}
 output,e:=server.StdoutPipe();if e!=nil{return e}
 if e=server.Start();e!=nil{return e}
 fmt.Fprintln(os.Stderr,"sftp-probe: native server started")
 defer func(){_ = server.Process.Kill();_ = server.Wait()}()
 if _,e=input.Write(first);e!=nil{return e}
 fmt.Fprintln(os.Stderr,"sftp-probe: INIT forwarded")
 var writes sync.Map
 inputDone:=make(chan error,1)
 go func(){defer input.Close();for{
  p,e:=packet(os.Stdin);if e!=nil{if e==io.EOF{e=nil};inputDone<-e;return}
  if len(p)>=9&&p[4]==6{writes.Store(binary.BigEndian.Uint32(p[5:9]),true)}
  if _,e=input.Write(p);e!=nil{inputDone<-e;return}
 }}()
 gated:=false
 for{
  p,e:=packet(output);if e!=nil{if e==io.EOF{break};return e}
  if len(p)>=9&&p[4]==2{record["serverType"]=p[4];record["serverVersion"]=binary.BigEndian.Uint32(p[5:9]);if e=save();e!=nil{return e};fmt.Fprintln(os.Stderr,"sftp-probe: server VERSION received")}
  if len(p)>=13&&p[4]==101{
   _,write:=writes.LoadAndDelete(binary.BigEndian.Uint32(p[5:9]))
   if write&&binary.BigEndian.Uint32(p[9:13])==0&&!gated&&os.Getenv("CBX_SFTP_MODE")=="gate"{
    gated=true
    conn,e:=net.DialTimeout("tcp",os.Getenv("CBX_SFTP_SIGNAL"),5*time.Second);if e!=nil{return e}
    if _,e=conn.Write([]byte{1});e!=nil{return e}
    var permit [1]byte;_,e=io.ReadFull(conn,permit[:]);conn.Close();if e!=nil{return e}
   }
  }
  if _,e=os.Stdout.Write(p);e!=nil{return e}
 }
 e=server.Wait()
 select {case inputErr:=<-inputDone: e=errors.Join(e,inputErr);default:}
 return e
}
func main(){if e:=run();e!=nil{fmt.Fprintln(os.Stderr,e);os.Exit(1)}}
`

type windowsSFTPFixture struct {
	t                  *testing.T
	root, tools, proxy string
	mode, signal       string
	timeout            time.Duration
	mu                 sync.Mutex
	reports            []string
	current            string
	beforeCopy         func(string, string, bool) error
}

func sftpFixtureTools(t *testing.T) string {
	t.Helper()
	root := os.Getenv("CRABBOX_TEST_OPENSSH_DIR")
	if root == "" {
		if os.Getenv("CRABBOX_TEST_REQUIRE_SFTP") == "1" {
			t.Fatal("required SFTP lane needs CRABBOX_TEST_OPENSSH_DIR pointing to a verified portable OpenSSH bundle")
		}
		t.Skip("opt-in native SFTP integration lane: set CRABBOX_TEST_OPENSSH_DIR and CRABBOX_TEST_REQUIRE_SFTP=1")
	}
	for _, name := range []string{"ssh.exe", "scp.exe", "sftp-server.exe"} {
		if _, err := windowsSFTPRegularFile(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	ssh, err := windowsSFTPFileVersion(filepath.Join(root, "ssh.exe"))
	if err != nil {
		t.Fatal(err)
	}
	scp, err := windowsSFTPFileVersion(filepath.Join(root, "scp.exe"))
	if err != nil || ssh != scp || ssh[0] < 9 {
		t.Fatalf("unqualified SFTP fixture client pair: ssh=%v scp=%v err=%v", ssh, scp, err)
	}
	return root
}

func buildSFTPProbe(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	source, binary := filepath.Join(root, "probe.go"), filepath.Join(root, "sftp-probe.exe")
	if err := os.WriteFile(source, []byte(sftpProbeSource), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binary, source)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOTOOLCHAIN=local")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build isolated protocol probe: %v\n%s", err, out)
	}
	return binary
}

func (f *windowsSFTPFixture) guest(ctx context.Context, script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	command := strings.Fields(windowsSFTPGuestCommand(script))
	return runWindowsSFTPProcess(ctx, SSHTarget{}, "powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", command[len(command)-1])
}

func (f *windowsSFTPFixture) copy(ctx context.Context, local, remote string, upload bool) error {
	budget := f.timeout
	if budget == 0 {
		budget = time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if f.beforeCopy != nil {
		if err := f.beforeCopy(local, remote, upload); err != nil {
			return err
		}
	}
	f.mu.Lock()
	report := filepath.Join(f.t.TempDir(), "protocol.json")
	f.reports = append(f.reports, report)
	f.current = strings.ReplaceAll(strings.TrimPrefix(remote, "/"), "/", "\\")
	f.mu.Unlock()
	config := filepath.Join(f.t.TempDir(), "ssh_config")
	if err := os.WriteFile(config, []byte("Host fixture\n  HostName invalid.invalid\n  BatchMode yes\n"), 0o600); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	session := &sshTransportSession{configPath: config, destination: "fixture"}
	// Even a broken -D invocation cannot contact SSH: -S points to the test
	// executable, not a network client. The proxy accepts only SFTP INIT.
	args := append([]string{"-D", f.proxy}, windowsSFTPArgs(session, executable, local, remote, upload)...)
	target := SSHTarget{ChildEnv: map[string]string{
		"CBX_SFTP_SERVER": filepath.Join(f.tools, "sftp-server.exe"), "CBX_SFTP_ROOT": f.root,
		"CBX_SFTP_REPORT": report, "CBX_SFTP_MODE": f.mode, "CBX_SFTP_SIGNAL": f.signal,
	}}
	_, err = runWindowsSFTPProcess(ctx, target, filepath.Join(f.tools, "scp.exe"), args...)
	return err
}

func (f *windowsSFTPFixture) assertProtocol(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	reports := append([]string(nil), f.reports...)
	f.mu.Unlock()
	if len(reports) == 0 {
		t.Fatal("no real SFTP exchange occurred")
	}
	for _, report := range reports {
		data, err := os.ReadFile(report)
		if err != nil {
			t.Fatal(err)
		}
		var observed struct{ ClientType, ClientVersion, ServerType, ServerVersion int }
		if err := json.Unmarshal(data, &observed); err != nil {
			t.Fatal(err)
		}
		if observed.ClientType != 1 || observed.ServerType != 2 || observed.ClientVersion != 3 || observed.ServerVersion != 3 {
			t.Fatalf("not a real SFTP v3 exchange: %+v", observed)
		}
	}
}

func runFixtureWindowsCopy(ctx context.Context, local, remote string, upload bool, fixture *windowsSFTPFixture) error {
	source, destination := local, "SANDBOX:"+remote
	if !upload {
		source, destination = destination, source
	}
	req, err := parseWindowsSFTPRequest(source, destination, false)
	if err != nil {
		return err
	}
	return copyWindowsSFTPWithTransport(ctx, req, fixture, strings.Repeat("a", 32))
}

func assertNoWindowsCopyStage(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".crabbox-cp-") {
			t.Fatalf("owned staging remains: %s", entry.Name())
		}
	}
}

func TestWindowsSFTPBehavior(t *testing.T) {
	tools, proxy := sftpFixtureTools(t), buildSFTPProbe(t)
	t.Run("bytes-spaces-overwrite", func(t *testing.T) {
		root := t.TempDir()
		f := &windowsSFTPFixture{t: t, root: root, tools: tools, proxy: proxy, timeout: windowsSFTPTimeout}
		source, remote, output := filepath.Join(root, "input.bin"), filepath.Join(root, "o'hara evidence.zip"), filepath.Join(root, "output.bin")
		data := bytes.Repeat([]byte{0, 10, 13, 26, 127, 128, 255}, 32*1024)
		for name, value := range map[string][]byte{source: data, remote: []byte("old remote"), output: []byte("old local")} {
			if err := os.WriteFile(name, value, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := runFixtureWindowsCopy(ctx, source, remote, true, f); err != nil {
			t.Fatal(err)
		}
		if err := runFixtureWindowsCopy(ctx, output, remote, false, f); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(output)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("binary bytes changed: %v", err)
		}
		f.assertProtocol(t)
		assertNoWindowsCopyStage(t, root)
	})
	t.Run("missing-export", func(t *testing.T) {
		root := t.TempDir()
		f := &windowsSFTPFixture{t: t, root: root, tools: tools, proxy: proxy}
		output := filepath.Join(root, "existing")
		if err := os.WriteFile(output, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runFixtureWindowsCopy(context.Background(), output, filepath.Join(root, "missing.zip"), false, f); err == nil {
			t.Fatal("missing export reported success")
		}
		got, _ := os.ReadFile(output)
		if string(got) != "keep" {
			t.Fatal("failed export replaced prior output")
		}
		assertNoWindowsCopyStage(t, root)
	})
	t.Run("publish-denied", func(t *testing.T) {
		root := t.TempDir()
		f := &windowsSFTPFixture{t: t, root: root, tools: tools, proxy: proxy}
		source, remote := filepath.Join(root, "input"), filepath.Join(root, "existing")
		if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(remote, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		path, err := windows.UTF16PtrFromString(remote)
		if err != nil {
			t.Fatal(err)
		}
		handle, err := windows.CreateFile(path, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer windows.CloseHandle(handle)
		if err := runFixtureWindowsCopy(context.Background(), source, remote, true, f); err == nil {
			t.Fatal("denied publication reported success")
		}
		got, _ := os.ReadFile(remote)
		if string(got) != "keep" {
			t.Fatal("publication failure corrupted prior destination")
		}
		f.assertProtocol(t)
		assertNoWindowsCopyStage(t, root)
	})
	for _, reason := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(reason.Error(), func(t *testing.T) {
			root := t.TempDir()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			f := &windowsSFTPFixture{t: t, root: root, tools: tools, proxy: proxy, mode: "gate", signal: listener.Addr().String()}
			source, remote := filepath.Join(root, "input"), filepath.Join(root, "existing")
			if err := os.WriteFile(source, bytes.Repeat([]byte{42}, 32*1024*1024), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(remote, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			done := make(chan error, 1)
			go func() { done <- runFixtureWindowsCopy(ctx, source, remote, true, f) }()
			if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			conn, err := listener.Accept()
			if err != nil {
				t.Fatalf("no acknowledged SFTP write: %v", err)
			}
			defer conn.Close()
			var ack [1]byte
			if _, err := io.ReadFull(conn, ack[:]); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			partial := f.current
			f.mu.Unlock()
			info, err := os.Stat(partial)
			if err != nil || info.Size() == 0 {
				t.Fatalf("cancellation did not interrupt a real written payload: %v", err)
			}
			cancel(reason)
			if err := <-done; !errors.Is(err, reason) {
				t.Fatalf("cancellation/deadline cause lost: %v", err)
			}
			got, _ := os.ReadFile(remote)
			if string(got) != "keep" {
				t.Fatal("interrupted copy published partial data")
			}
			f.assertProtocol(t)
			assertNoWindowsCopyStage(t, root)
		})
	}
	t.Run("protocol-stdout-corruption", func(t *testing.T) {
		root := t.TempDir()
		f := &windowsSFTPFixture{t: t, root: root, tools: tools, proxy: proxy, mode: "noise"}
		source, output := filepath.Join(root, "input"), filepath.Join(root, "output")
		if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(output, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runFixtureWindowsCopy(context.Background(), output, source, false, f); err == nil {
			t.Fatal("corrupted protocol stdout reported a successful export")
		}
		got, _ := os.ReadFile(output)
		if string(got) != "keep" {
			t.Fatal("protocol noise became published evidence")
		}
		assertNoWindowsCopyStage(t, root)
	})
}

func TestWindowsSFTPStreamedLargeFile(t *testing.T) {
	tools, proxy := sftpFixtureTools(t), buildSFTPProbe(t)
	root := t.TempDir()
	free, err := availableRunDownloadBytes(root)
	if err != nil || free < 8<<30 {
		t.Fatalf("large SFTP lane requires 8 GiB free fixture space: free=%d err=%v", free, err)
	}
	source, remote, output := filepath.Join(root, "large.bin"), filepath.Join(root, "large remote.bin"), filepath.Join(root, "download.bin")
	file, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	block := make([]byte, 1024*1024)
	for i := range block {
		block[i] = byte(i)
	}
	for i := 0; i < 2048; i++ {
		if _, err := file.Write(block); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	if _, err := file.Write(block[:17]); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	f := &windowsSFTPFixture{t: t, root: root, tools: tools, proxy: proxy}
	expected, err := windowsSFTPHash(ctx, source)
	if err != nil || expected.Size != 2<<30+17 {
		t.Fatalf("large fixture invalid: %+v err=%v", expected, err)
	}
	f.timeout = windowsSFTPTimeout
	if err := runFixtureWindowsCopy(ctx, source, remote, true, f); err != nil {
		t.Fatal(err)
	}
	if err := runFixtureWindowsCopy(ctx, output, remote, false, f); err != nil {
		t.Fatal(err)
	}
	actual, err := windowsSFTPHash(ctx, output)
	if err != nil || actual != expected {
		t.Fatalf("multi-GB bytes changed: got=%+v want=%+v err=%v", actual, expected, err)
	}
	f.assertProtocol(t)
	assertNoWindowsCopyStage(t, root)
}
