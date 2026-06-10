package cli

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"encoding/binary"
	"flag"
	"image/color"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

type desktopCredentialTestProvider struct{}

func (desktopCredentialTestProvider) Name() string      { return "desktop-credential-test" }
func (desktopCredentialTestProvider) Aliases() []string { return nil }
func (desktopCredentialTestProvider) Spec() ProviderSpec {
	return ProviderSpec{Name: "desktop-credential-test"}
}
func (desktopCredentialTestProvider) RegisterFlags(*flag.FlagSet, Config) any { return nil }
func (desktopCredentialTestProvider) ApplyFlags(*Config, *flag.FlagSet, any) error {
	return nil
}
func (desktopCredentialTestProvider) Configure(Config, Runtime) (Backend, error) { return nil, nil }
func (desktopCredentialTestProvider) DesktopCredentials(Config, SSHTarget) (DesktopCredentials, bool) {
	return DesktopCredentials{Password: "provider-secret"}, true
}

func TestDesktopCredentialsFromProvider(t *testing.T) {
	got, ok := desktopCredentialsFromProvider(
		desktopCredentialTestProvider{},
		Config{},
		SSHTarget{User: "lease-user"},
	)
	if !ok {
		t.Fatal("provider desktop credentials should be available")
	}
	if got.Username != "lease-user" || got.Password != "provider-secret" {
		t.Fatalf("credentials = %#v", got)
	}
}

func TestCaptureRFBFrameSupportsAppleRemoteDesktopAuth(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- serveTestARDRFB(server, "ec2-user", "example-pass")
	}()

	img, err := captureRFBFrameFromConn(context.Background(), client, rfbCredentials{
		Username: "ec2-user",
		Password: "example-pass",
	})
	if err != nil {
		t.Fatalf("capture RFB frame: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("fake RFB server: %v", err)
	}
	if got := color.RGBAModel.Convert(img.At(0, 0)); got != (color.RGBA{R: 255, A: 255}) {
		t.Fatalf("pixel 0=%v", got)
	}
	if got := color.RGBAModel.Convert(img.At(1, 0)); got != (color.RGBA{G: 255, A: 255}) {
		t.Fatalf("pixel 1=%v", got)
	}
}

func TestCaptureRFBFrameReadsNoneAuthSecurityResult(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- serveTestNoneRFB(server)
	}()

	img, err := captureRFBFrameFromConn(context.Background(), client, rfbCredentials{})
	if err != nil {
		t.Fatalf("capture RFB frame: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("fake RFB server: %v", err)
	}
	if got := color.RGBAModel.Convert(img.At(0, 0)); got != (color.RGBA{B: 255, A: 255}) {
		t.Fatalf("pixel=%v", got)
	}
}

func serveTestARDRFB(conn net.Conn, username, password string) error {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("RFB 003.889\n")); err != nil {
		return err
	}
	clientVersion := make([]byte, 12)
	if _, err := io.ReadFull(conn, clientVersion); err != nil {
		return err
	}
	if !bytes.Equal(clientVersion, []byte("RFB 003.008\n")) {
		return errUnexpectedTestBytes("client version", clientVersion)
	}
	if _, err := conn.Write([]byte{1, rfbSecurityARD}); err != nil {
		return err
	}
	security := []byte{0}
	if _, err := io.ReadFull(conn, security); err != nil {
		return err
	}
	if security[0] != rfbSecurityARD {
		return errUnexpectedTestBytes("security type", security)
	}

	keyLength := 8
	g := big.NewInt(5)
	p := big.NewInt(23)
	serverPrivate := big.NewInt(6)
	serverPublic := new(big.Int).Exp(g, serverPrivate, p)
	params := make([]byte, 4+keyLength*2)
	binary.BigEndian.PutUint16(params[0:2], uint16(g.Uint64()))
	binary.BigEndian.PutUint16(params[2:4], uint16(keyLength))
	copy(params[4:4+keyLength], leftPadBigInt(p, keyLength))
	copy(params[4+keyLength:], leftPadBigInt(serverPublic, keyLength))
	if _, err := conn.Write(params); err != nil {
		return err
	}

	response := make([]byte, 128+keyLength)
	if _, err := io.ReadFull(conn, response); err != nil {
		return err
	}
	clientPublic := new(big.Int).SetBytes(response[128:])
	shared := new(big.Int).Exp(clientPublic, serverPrivate, p)
	key := md5.Sum(leftPadBigInt(shared, keyLength))
	credentials, err := aesECBDecryptForTest(key[:], response[:128])
	if err != nil {
		return err
	}
	if got := string(credentials[:bytes.IndexByte(credentials[:64], 0)]); got != username {
		return errUnexpectedTestString("username", got)
	}
	if got := string(credentials[64 : 64+bytes.IndexByte(credentials[64:], 0)]); got != password {
		return errUnexpectedTestString("password", got)
	}
	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
		return err
	}

	clientInit := []byte{0}
	if _, err := io.ReadFull(conn, clientInit); err != nil {
		return err
	}
	serverInit := make([]byte, 24)
	binary.BigEndian.PutUint16(serverInit[0:2], 2)
	binary.BigEndian.PutUint16(serverInit[2:4], 1)
	serverInit[4] = 32
	serverInit[5] = 24
	serverInit[7] = 1
	binary.BigEndian.PutUint16(serverInit[8:10], 255)
	binary.BigEndian.PutUint16(serverInit[10:12], 255)
	binary.BigEndian.PutUint16(serverInit[12:14], 255)
	serverInit[14] = 16
	serverInit[15] = 8
	if _, err := conn.Write(serverInit); err != nil {
		return err
	}
	if err := readTestRFBPixelFormat(conn); err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, conn, 8); err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, conn, 10); err != nil {
		return err
	}

	update := make([]byte, 4+12+8)
	update[0] = 0
	binary.BigEndian.PutUint16(update[2:4], 1)
	binary.BigEndian.PutUint16(update[8:10], 2)
	binary.BigEndian.PutUint16(update[10:12], 1)
	binary.BigEndian.PutUint32(update[12:16], uint32(rfbEncodingRaw))
	copy(update[16:], []byte{
		0, 0, 255, 0,
		0, 255, 0, 0,
	})
	_, err = conn.Write(update)
	return err
}

func serveTestNoneRFB(conn net.Conn) error {
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("RFB 003.008\n")); err != nil {
		return err
	}
	clientVersion := make([]byte, 12)
	if _, err := io.ReadFull(conn, clientVersion); err != nil {
		return err
	}
	if !bytes.Equal(clientVersion, []byte("RFB 003.008\n")) {
		return errUnexpectedTestBytes("client version", clientVersion)
	}
	if _, err := conn.Write([]byte{1, rfbSecurityNone}); err != nil {
		return err
	}
	security := []byte{0}
	if _, err := io.ReadFull(conn, security); err != nil {
		return err
	}
	if security[0] != rfbSecurityNone {
		return errUnexpectedTestBytes("security type", security)
	}
	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
		return err
	}
	clientInit := []byte{0}
	if _, err := io.ReadFull(conn, clientInit); err != nil {
		return err
	}
	serverInit := make([]byte, 24)
	binary.BigEndian.PutUint16(serverInit[0:2], 1)
	binary.BigEndian.PutUint16(serverInit[2:4], 1)
	serverInit[4] = 32
	serverInit[5] = 24
	serverInit[7] = 1
	binary.BigEndian.PutUint16(serverInit[8:10], 255)
	binary.BigEndian.PutUint16(serverInit[10:12], 255)
	binary.BigEndian.PutUint16(serverInit[12:14], 255)
	serverInit[14] = 16
	serverInit[15] = 8
	if _, err := conn.Write(serverInit); err != nil {
		return err
	}
	if err := readTestRFBPixelFormat(conn); err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, conn, 8); err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, conn, 10); err != nil {
		return err
	}
	update := make([]byte, 4+12+4)
	update[0] = 0
	binary.BigEndian.PutUint16(update[2:4], 1)
	binary.BigEndian.PutUint16(update[8:10], 1)
	binary.BigEndian.PutUint16(update[10:12], 1)
	binary.BigEndian.PutUint32(update[12:16], uint32(rfbEncodingRaw))
	copy(update[16:], []byte{255, 0, 0, 0})
	_, err := conn.Write(update)
	return err
}

func aesECBDecryptForTest(key, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(ciphertext))
	for offset := 0; offset < len(ciphertext); offset += aes.BlockSize {
		block.Decrypt(out[offset:offset+aes.BlockSize], ciphertext[offset:offset+aes.BlockSize])
	}
	return out, nil
}

func readTestRFBPixelFormat(conn net.Conn) error {
	msg := make([]byte, 20)
	if _, err := io.ReadFull(conn, msg); err != nil {
		return err
	}
	if msg[0] != 0 || msg[4] != 32 || msg[5] != 24 || msg[7] != 1 {
		return errUnexpectedTestBytes("pixel format", msg)
	}
	return nil
}

type testError string

func (e testError) Error() string { return string(e) }

func errUnexpectedTestBytes(label string, got []byte) error {
	return testError(label + " mismatch: " + string(got))
}

func errUnexpectedTestString(label, got string) error {
	return testError(label + " mismatch: " + got)
}
