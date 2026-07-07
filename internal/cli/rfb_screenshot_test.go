package cli

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/des"
	"crypto/md5"
	"encoding/binary"
	"flag"
	"image/color"
	"io"
	"math/big"
	"math/bits"
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

func TestClickRFBPointerSendsPressAndRelease(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- serveTestPointerRFB(server, 12, 34)
	}()

	if err := clickRFBPointerFromConn(context.Background(), client, rfbCredentials{}, 12, 34); err != nil {
		t.Fatalf("click RFB pointer: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("fake RFB server: %v", err)
	}
}

func TestWriteRFBPointerEventRejectsProtocolOverflow(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	for _, point := range [][2]int{{-1, 0}, {0, -1}, {1 << 16, 0}, {0, 1 << 16}} {
		if err := writeRFBPointerEvent(client, 0, point[0], point[1]); err == nil {
			t.Errorf("pointer coordinates %d,%d should be rejected", point[0], point[1])
		}
	}
}

func TestTypeRFBTextSendsExactKeyEvents(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	const text = "Aa !\n\té🦀"
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- serveTestTypeRFB(server, []uint32{0x41, 0x61, 0x20, 0x21, 0xff0d, 0xff09, 0xe9, 0x0101f980})
	}()

	if err := typeRFBTextFromConn(context.Background(), client, rfbCredentials{}, text); err != nil {
		t.Fatalf("type RFB text: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("fake RFB server: %v", err)
	}
}

func TestRFBKeysymForRuneRejectsUnsupportedControl(t *testing.T) {
	if _, err := rfbKeysymForRune(0x1b); err == nil {
		t.Fatal("Escape control character should be rejected")
	}
}

func TestRFBPasswordOnlyCredentialsPreferVNCAuth(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	serverErr := make(chan error, 1)
	go func() {
		if _, err := server.Write([]byte{2, rfbSecurityARD, rfbSecurityVNC}); err != nil {
			serverErr <- err
			return
		}
		selected := []byte{0}
		if _, err := io.ReadFull(server, selected); err != nil {
			serverErr <- err
			return
		}
		if selected[0] != rfbSecurityVNC {
			serverErr <- errUnexpectedTestBytes("security type", selected)
			return
		}
		challenge := []byte("0123456789abcdef")
		if _, err := server.Write(challenge); err != nil {
			serverErr <- err
			return
		}
		response := make([]byte, len(challenge))
		if _, err := io.ReadFull(server, response); err != nil {
			serverErr <- err
			return
		}
		key := []byte("password")
		for i := range key {
			key[i] = bits.Reverse8(key[i])
		}
		cipher, err := des.NewCipher(key)
		if err != nil {
			serverErr <- err
			return
		}
		plaintext := make([]byte, len(response))
		for offset := 0; offset < len(response); offset += cipher.BlockSize() {
			cipher.Decrypt(plaintext[offset:offset+cipher.BlockSize()], response[offset:offset+cipher.BlockSize()])
		}
		if !bytes.Equal(plaintext, challenge) {
			serverErr <- errUnexpectedTestBytes("VNC auth response", plaintext)
			return
		}
		serverErr <- nil
	}()

	credentials := rfbCredentials{Password: "password"}
	securityType, err := negotiateRFBSecurityType(client, credentials)
	if err != nil {
		t.Fatal(err)
	}
	if securityType != rfbSecurityVNC {
		t.Fatalf("security type = %d", securityType)
	}
	if err := negotiateRFBVNCAuth(client, credentials); err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func serveTestPointerRFB(conn net.Conn, wantX, wantY int) error {
	if err := serveTestInputRFBInit(conn); err != nil {
		return err
	}

	wantMasks := []byte{0, 1, 0}
	for _, wantMask := range wantMasks {
		event := make([]byte, 6)
		if _, err := io.ReadFull(conn, event); err != nil {
			return err
		}
		if event[0] != 5 || event[1] != wantMask || int(binary.BigEndian.Uint16(event[2:4])) != wantX || int(binary.BigEndian.Uint16(event[4:6])) != wantY {
			return errUnexpectedTestBytes("pointer event", event)
		}
	}
	return nil
}

func serveTestTypeRFB(conn net.Conn, wantKeys []uint32) error {
	if err := serveTestInputRFBInit(conn); err != nil {
		return err
	}
	for _, wantKey := range wantKeys {
		for _, wantDown := range []byte{1, 0} {
			event := make([]byte, 8)
			if _, err := io.ReadFull(conn, event); err != nil {
				return err
			}
			if event[0] != 4 || event[1] != wantDown || !bytes.Equal(event[2:4], []byte{0, 0}) || binary.BigEndian.Uint32(event[4:8]) != wantKey {
				return errUnexpectedTestBytes("key event", event)
			}
		}
	}
	return nil
}

func serveTestInputRFBInit(conn net.Conn) error {
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
	binary.BigEndian.PutUint16(serverInit[0:2], 100)
	binary.BigEndian.PutUint16(serverInit[2:4], 80)
	serverInit[4] = 32
	serverInit[5] = 24
	serverInit[7] = 1
	if _, err := conn.Write(serverInit); err != nil {
		return err
	}
	return nil
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
