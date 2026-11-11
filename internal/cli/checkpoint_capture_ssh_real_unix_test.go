//go:build darwin || linux

package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
)

type checkpointPreparationSSHServer struct {
	listener    net.Listener
	user        string
	key         ssh.PublicKey
	mu          sync.Mutex
	connections []net.Conn
	wg          sync.WaitGroup
	accepted    atomic.Int32
}

func newCheckpointPreparationSSHServer(t *testing.T, user string, key ssh.PublicKey) *checkpointPreparationSSHServer {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	hostKey, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	server := &checkpointPreparationSSHServer{listener: listener, user: user, key: key}
	config := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, candidate ssh.PublicKey) (*ssh.Permissions, error) {
		if meta.User() != server.user || !bytes.Equal(candidate.Marshal(), server.key.Marshal()) {
			return nil, fmt.Errorf("checkpoint fixture rejected SSH identity")
		}
		return nil, nil
	}}
	config.AddHostKey(hostKey)
	server.wg.Add(1)
	go func() {
		defer server.wg.Done()
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			server.accepted.Add(1)
			server.mu.Lock()
			server.connections = append(server.connections, connection)
			server.mu.Unlock()
			server.wg.Add(1)
			go server.serve(connection, config)
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		server.mu.Lock()
		for _, connection := range server.connections {
			connection.Close()
		}
		server.mu.Unlock()
		server.wg.Wait()
	})
	return server
}

func (s *checkpointPreparationSSHServer) serve(connection net.Conn, config *ssh.ServerConfig) {
	defer s.wg.Done()
	defer connection.Close()
	transport, channels, requests, err := ssh.NewServerConn(connection, config)
	if err != nil {
		return
	}
	defer transport.Close()
	go ssh.DiscardRequests(requests)
	for incoming := range channels {
		if incoming.ChannelType() != "session" {
			_ = incoming.Reject(ssh.UnknownChannelType, "session required")
			continue
		}
		channel, channelRequests, err := incoming.Accept()
		if err != nil {
			continue
		}
		for request := range channelRequests {
			if request.Type != "exec" {
				_ = request.Reply(false, nil)
				continue
			}
			_ = request.Reply(true, nil)
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
			channel.Close()
			break
		}
	}
}

func (s *checkpointPreparationSSHServer) port() int {
	return s.listener.Addr().(*net.TCPAddr).Port
}

func runCheckpointAWSSSHAccessContract(t *testing.T, repo, binary string) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal("real OpenSSH is required:", err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("python3 is required for the SSM stream fixture:", err)
	}
	for _, transport := range []string{AWSSSHTransportDirect, AWSSSHTransportSSM} {
		t.Run(transport, func(t *testing.T) {
			f := newCheckpointCaptureFixture(t, repo, binary)
			const instanceID = "i-0123456789abcdef0"
			const imageID = "ami-0123456789abcdef0"
			const accountID = "123456789012"
			const user = "ubuntu"

			storedKey := filepath.Join(f.root, "state", "crabbox", "testboxes", captureFixtureLease, "id_ed25519")
			if err := os.MkdirAll(filepath.Dir(storedKey), 0o700); err != nil {
				t.Fatal(err)
			}
			keygen := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", storedKey)
			if output, err := keygen.CombinedOutput(); err != nil {
				t.Fatalf("generate stored checkpoint source key: %v: %s", err, output)
			}
			publicData, err := os.ReadFile(storedKey + ".pub")
			if err != nil {
				t.Fatal(err)
			}
			publicKey, _, _, _, err := ssh.ParseAuthorizedKey(publicData)
			if err != nil {
				t.Fatal(err)
			}
			server := newCheckpointPreparationSSHServer(t, user, publicKey)
			port := strconv.Itoa(server.port())

			if err := os.WriteFile(filepath.Join(f.root, "bin", "ssh"), []byte("#!/bin/sh\nexec "+shellQuote(sshPath)+" \"$@\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			proxyLog := filepath.Join(f.root, "ssm-proxy.log")
			proxyScript := fmt.Sprintf(`#!/usr/bin/env python3
import os
import select
import socket
import sys
with open(%s, "a", encoding="utf-8") as log:
    log.write(" ".join(sys.argv[1:]) + "\n")
stream = socket.create_connection(("127.0.0.1", %d), timeout=5)
stdin_open = True
while True:
    readers = [stream]
    if stdin_open:
        readers.append(sys.stdin.buffer)
    ready, _, _ = select.select(readers, [], [])
    if stream in ready:
        data = stream.recv(65536)
        if not data:
            break
        sys.stdout.buffer.write(data)
        sys.stdout.buffer.flush()
    if stdin_open and sys.stdin.buffer in ready:
        data = os.read(sys.stdin.fileno(), 65536)
        if data:
            stream.sendall(data)
        else:
            stdin_open = False
            stream.shutdown(socket.SHUT_WR)
`, strconv.Quote(proxyLog), server.port())
			if err := os.WriteFile(filepath.Join(f.root, "bin", "aws"), []byte(proxyScript), 0o700); err != nil {
				t.Fatal(err)
			}

			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if err := request.ParseForm(); err != nil {
					t.Error(err)
					http.Error(w, "bad fixture request", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "text/xml")
				switch action := request.Form.Get("Action"); action {
				case "GetCallerIdentity":
					fmt.Fprintf(w, `<GetCallerIdentityResponse><GetCallerIdentityResult><Account>%s</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`, accountID)
				case "DescribeInstances":
					fmt.Fprintf(w, `<DescribeInstancesResponse><reservationSet><item><instancesSet><item><instanceId>%s</instanceId><instanceType>t3.medium</instanceType><ipAddress>127.0.0.1</ipAddress><instanceState><name>running</name></instanceState><tagSet>`, instanceID)
					tags := map[string]string{"Name": "aws-ssh-access-proof", "crabbox": "true", "created_by": "crabbox", "provider": "aws", "lease": captureFixtureLease, "slug": "aws-ssh-access-proof", "provider_key": ProviderKeyForLease(captureFixtureLease), "aws_region": "us-east-1", "target": targetLinux}
					if transport == AWSSSHTransportSSM {
						tags["aws_ssh_transport"] = AWSSSHTransportSSM
					}
					for key, value := range tags {
						fmt.Fprintf(w, "<item><key>%s</key><value>%s</value></item>", key, value)
					}
					fmt.Fprint(w, `</tagSet></item></instancesSet></item></reservationSet></DescribeInstancesResponse>`)
				case "CreateImage":
					fmt.Fprintf(w, `<CreateImageResponse><imageId>%s</imageId></CreateImageResponse>`, imageID)
				case "DescribeImages":
					fmt.Fprintf(w, `<DescribeImagesResponse><imagesSet><item><imageId>%s</imageId><imageState>pending</imageState><architecture>x86_64</architecture></item></imagesSet></DescribeImagesResponse>`, imageID)
				default:
					t.Errorf("unexpected AWS request: %s", action)
					http.Error(w, "unsupported fixture action", http.StatusBadRequest)
				}
			}))
			t.Cleanup(endpoint.Close)

			config := fmt.Sprintf("provider: aws\nnetwork: public\naws:\n  region: us-east-1\ntargetOS: linux\nssh:\n  user: %s\n  port: %q\n  key: %s\n  fallbackPorts: []\n", user, port, filepath.Join(f.root, "wrong-key"))
			if err := os.WriteFile(filepath.Join(f.root, "config.yaml"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			f.env = append(f.env, "AWS_ENDPOINT_URL="+endpoint.URL, "AWS_ACCESS_KEY_ID=fixture", "AWS_SECRET_ACCESS_KEY=fixture", "AWS_EC2_METADATA_DISABLED=true", "AWS_REGION=us-east-1")
			claim := f.claim()
			claim.Provider, claim.CloudID, claim.ProviderScope, claim.Slug = "aws", instanceID, "account:"+accountID, "aws-ssh-access-proof"
			claim.FixedCreateIntent = nil
			claim.Labels = map[string]string{"aws_region": "us-east-1", "aws_account_id": accountID, "provider_key": ProviderKeyForLease(captureFixtureLease)}
			if transport == AWSSSHTransportSSM {
				claim.Labels["aws_ssh_transport"] = AWSSSHTransportSSM
			}
			f.writeJSON(filepath.Join(f.root, "state", "crabbox", "claims", captureFixtureLease+".json"), claim)

			result := f.run("checkpoint", "create", "--provider", "aws", "--id", captureFixtureLease, "--checkpoint-id", captureFixtureCheckpoint, "--retire-source", "--mode", "native", "--strategy", "image", "--wait=false", "--json")
			if result.err != nil {
				t.Fatalf("real SSH checkpoint preparation failed: %v\n%s", result.err, result.stderr)
			}
			record := f.record(captureFixtureCheckpoint)
			if record.Capture == nil || record.Capture.Phase != "pending" || record.Native.ImageID != imageID {
				t.Fatalf("checkpoint did not retain pending image: %+v", record)
			}
			proxyData, proxyErr := os.ReadFile(proxyLog)
			if transport == AWSSSHTransportSSM {
				want := "ssm start-session --target " + instanceID + " --document-name AWS-StartSSHSession --parameters portNumber=" + port + " --region us-east-1\n"
				if proxyErr != nil || string(proxyData) != want {
					t.Fatalf("SSM ProxyCommand route=%q err=%v want=%q", proxyData, proxyErr, want)
				}
			} else if !os.IsNotExist(proxyErr) {
				t.Fatalf("direct SSH unexpectedly invoked ProxyCommand: route=%q err=%v", proxyData, proxyErr)
			}
			if got := server.accepted.Load(); got != 1 {
				t.Fatalf("checkpoint preparation made %d SSH connections, want one exact route with no fallback", got)
			}
		})
	}
}
