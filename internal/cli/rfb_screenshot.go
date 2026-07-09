package cli

import (
	"context"
	"crypto/aes"
	"crypto/des"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/big"
	"math/bits"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	rfbSecurityNone  = 1
	rfbSecurityVNC   = 2
	rfbSecurityARD   = 30
	rfbEncodingRaw   = 0
	rfbKeyEventDelay = 5 * time.Millisecond
)

type rfbCredentials struct {
	Username string
	Password string
}

type rfbClientProtocol struct {
	legacySecurityType      bool
	securityResultAfterNone bool
}

func captureRemoteMacVNCScreenshot(ctx context.Context, cfg Config, target SSHTarget, outputPath string) error {
	tunnel, localPort, err := startVNCForegroundTunnelOnReservedPort(ctx, target, "", "127.0.0.1", managedVNCPort)
	if err != nil {
		return err
	}
	defer stopProcess(tunnel)

	creds, authMode, err := resolveMacOSRFBAuthentication(ctx, cfg, target)
	if err != nil {
		return err
	}
	conn, err := dialVNCForegroundTunnel(ctx, tunnel, localPort)
	if err != nil {
		return fmt.Errorf("connect to verified macOS VNC tunnel: %w", err)
	}
	defer conn.Close()
	img, err := captureRFBFrameFromConn(ctx, conn, creds, authMode)
	if err != nil {
		return exit(5, "capture macOS VNC screenshot: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return exit(2, "create screenshot directory: %v", err)
	}
	file, err := os.Create(outputPath)
	if err != nil {
		return exit(2, "create screenshot %s: %v", outputPath, err)
	}
	ok := false
	defer func() {
		_ = file.Close()
		if !ok {
			_ = os.Remove(outputPath)
		}
	}()
	if err := png.Encode(file, img); err != nil {
		return exit(5, "write screenshot PNG: %v", err)
	}
	ok = true
	return nil
}

func clickRemoteMacVNC(ctx context.Context, cfg Config, target SSHTarget, x, y int) error {
	tunnel, localPort, err := startVNCForegroundTunnelOnReservedPort(ctx, target, "", "127.0.0.1", managedVNCPort)
	if err != nil {
		return err
	}
	defer stopProcess(tunnel)

	creds, authMode, err := resolveMacOSRFBAuthentication(ctx, cfg, target)
	if err != nil {
		return err
	}
	conn, err := dialVNCForegroundTunnel(ctx, tunnel, localPort)
	if err != nil {
		return fmt.Errorf("connect to verified macOS VNC tunnel: %w", err)
	}
	defer conn.Close()
	if err := clickRFBPointerFromConn(ctx, conn, creds, authMode, x, y); err != nil {
		return fmt.Errorf("click macOS VNC pointer: %w", err)
	}
	return nil
}

func typeRemoteMacVNC(ctx context.Context, cfg Config, target SSHTarget, text string) error {
	tunnel, localPort, err := startVNCForegroundTunnelOnReservedPort(ctx, target, "", "127.0.0.1", managedVNCPort)
	if err != nil {
		return err
	}
	defer stopProcess(tunnel)

	creds, authMode, err := resolveMacOSRFBAuthentication(ctx, cfg, target)
	if err != nil {
		return err
	}
	conn, err := dialVNCForegroundTunnel(ctx, tunnel, localPort)
	if err != nil {
		return fmt.Errorf("connect to verified macOS VNC tunnel: %w", err)
	}
	defer conn.Close()
	if err := typeRFBTextFromConn(ctx, conn, creds, authMode, text); err != nil {
		return fmt.Errorf("type macOS VNC text: %w", err)
	}
	return nil
}

func resolveMacOSRFBAuthentication(ctx context.Context, cfg Config, target SSHTarget) (rfbCredentials, localWebVNCAuthenticationMode, error) {
	credentials, authMode, err := resolveMacOSWebVNCCredentials(ctx, cfg, target, runSSHOutput)
	if err != nil {
		return rfbCredentials{}, localWebVNCAuthAuto, err
	}
	if err := requireMacOSWebVNCCredentials(credentials, authMode); err != nil {
		return rfbCredentials{}, localWebVNCAuthAuto, err
	}
	return credentials, authMode, nil
}

func providerDesktopCredentials(cfg Config, target SSHTarget) (rfbCredentials, bool, error) {
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return rfbCredentials{}, false, nil
	}
	return desktopCredentialsFromProvider(provider, cfg, target)
}

func desktopCredentialsFromProvider(provider Provider, cfg Config, target SSHTarget) (rfbCredentials, bool, error) {
	var credentials DesktopCredentials
	var ok bool
	if resolver, supported := provider.(DesktopCredentialResolver); supported {
		var err error
		credentials, ok, err = resolver.ResolveDesktopCredentials(cfg, target)
		if err != nil {
			return rfbCredentials{}, false, err
		}
	} else if credentialProvider, supported := provider.(DesktopCredentialProvider); supported {
		credentials, ok = credentialProvider.DesktopCredentials(cfg, target)
	} else {
		return rfbCredentials{}, false, nil
	}
	if !ok {
		return rfbCredentials{}, false, nil
	}
	username := strings.TrimSpace(credentials.Username)
	if username == "" {
		username = strings.TrimSpace(target.User)
	}
	return rfbCredentials{
		Username: username,
		Password: credentials.Password,
	}, true, nil
}

func preflightRFBAuthenticationFromConn(ctx context.Context, conn net.Conn, creds rfbCredentials) error {
	return preflightRFBAuthenticationFromConnWithMode(ctx, conn, creds, localWebVNCAuthAuto)
}

func preflightRFBAuthenticationFromConnWithMode(ctx context.Context, conn net.Conn, creds rfbCredentials, authMode localWebVNCAuthenticationMode) error {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	protocol, err := negotiateRFBClientProtocol(conn)
	if err != nil {
		return err
	}
	securityType, err := negotiateRFBClientSecurityType(conn, creds, authMode, protocol)
	if err != nil {
		return err
	}
	return preflightRFBAuthenticationForSecurityType(conn, creds, securityType)
}

func preflightRFBAuthenticationForSecurityType(conn net.Conn, creds rfbCredentials, securityType byte) error {
	switch securityType {
	case rfbSecurityNone:
		return fmt.Errorf("RFB server did not require credential authentication")
	case rfbSecurityVNC:
		if err := negotiateRFBVNCAuth(conn, creds); err != nil {
			return err
		}
	case rfbSecurityARD:
		if err := negotiateRFBARDAuth(conn, creds); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported RFB security type %d", securityType)
	}
	return readRFBSecurityResult(conn)
}

func typeRFBTextFromConn(ctx context.Context, conn net.Conn, creds rfbCredentials, authMode localWebVNCAuthenticationMode, text string) error {
	if !utf8.ValidString(text) {
		return fmt.Errorf("RFB text is not valid UTF-8")
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	if _, _, err := initializeRFBConnection(conn, creds, authMode); err != nil {
		return err
	}
	for _, r := range text {
		key, err := rfbKeysymForRune(r)
		if err != nil {
			return err
		}
		if err := writeRFBKeyEvent(conn, true, key); err != nil {
			return err
		}
		time.Sleep(rfbKeyEventDelay)
		if err := writeRFBKeyEvent(conn, false, key); err != nil {
			return err
		}
		time.Sleep(rfbKeyEventDelay)
	}
	time.Sleep(50 * time.Millisecond)
	return nil
}

func rfbKeysymForRune(r rune) (uint32, error) {
	switch r {
	case '\n', '\r':
		return 0xff0d, nil // XK_Return
	case '\t':
		return 0xff09, nil // XK_Tab
	case '\b':
		return 0xff08, nil // XK_BackSpace
	}
	if r < 0x20 || r == 0x7f {
		return 0, fmt.Errorf("RFB typing does not support control character U+%04X", r)
	}
	if r <= 0xff {
		return uint32(r), nil
	}
	return 0x01000000 | uint32(r), nil
}

func clickRFBPointerFromConn(ctx context.Context, conn net.Conn, creds rfbCredentials, authMode localWebVNCAuthenticationMode, x, y int) error {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	width, height, err := initializeRFBConnection(conn, creds, authMode)
	if err != nil {
		return err
	}
	if x < 0 || y < 0 || x >= int(width) || y >= int(height) {
		return fmt.Errorf("pointer coordinates %d,%d exceed framebuffer %dx%d", x, y, width, height)
	}
	if err := writeRFBPointerEvent(conn, 0, x, y); err != nil {
		return err
	}
	if err := writeRFBPointerEvent(conn, 1, x, y); err != nil {
		return err
	}
	time.Sleep(80 * time.Millisecond)
	return writeRFBPointerEvent(conn, 0, x, y)
}

func captureRFBFrameFromConn(ctx context.Context, conn net.Conn, creds rfbCredentials, authMode localWebVNCAuthenticationMode) (image.Image, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	width, height, err := initializeRFBConnection(conn, creds, authMode)
	if err != nil {
		return nil, err
	}

	if err := writeRFBPixelFormat(conn); err != nil {
		return nil, err
	}
	if err := writeRFBSetEncodings(conn); err != nil {
		return nil, err
	}
	if err := writeRFBFramebufferUpdateRequest(conn, width, height); err != nil {
		return nil, err
	}
	return readRFBFramebufferUpdate(conn, int(width), int(height))
}

func initializeRFBConnection(conn net.Conn, creds rfbCredentials, authMode localWebVNCAuthenticationMode) (uint16, uint16, error) {
	protocol, err := negotiateRFBClientProtocol(conn)
	if err != nil {
		return 0, 0, err
	}
	securityType, err := negotiateRFBClientSecurityType(conn, creds, authMode, protocol)
	if err != nil {
		return 0, 0, err
	}
	switch securityType {
	case rfbSecurityNone:
	case rfbSecurityVNC:
		if err := negotiateRFBVNCAuth(conn, creds); err != nil {
			return 0, 0, err
		}
	case rfbSecurityARD:
		if err := negotiateRFBARDAuth(conn, creds); err != nil {
			return 0, 0, err
		}
	default:
		return 0, 0, fmt.Errorf("unsupported RFB security type %d", securityType)
	}
	if securityType != rfbSecurityNone || protocol.securityResultAfterNone {
		if err := readRFBSecurityResult(conn); err != nil {
			return 0, 0, err
		}
	}

	if _, err := conn.Write([]byte{1}); err != nil {
		return 0, 0, fmt.Errorf("write RFB client init: %w", err)
	}
	width, height, err := readRFBServerInit(conn)
	if err != nil {
		return 0, 0, err
	}
	if width == 0 || height == 0 {
		return 0, 0, fmt.Errorf("server reported empty framebuffer %dx%d", width, height)
	}
	if int(width)*int(height) > 16_000_000 {
		return 0, 0, fmt.Errorf("framebuffer %dx%d is too large", width, height)
	}
	return width, height, nil
}

func negotiateRFBClientProtocol(conn net.Conn) (rfbClientProtocol, error) {
	version := make([]byte, 12)
	if _, err := io.ReadFull(conn, version); err != nil {
		return rfbClientProtocol{}, fmt.Errorf("read RFB version: %w", err)
	}
	clientVersion, err := rfbClientVersionForServer(version)
	if err != nil {
		return rfbClientProtocol{}, fmt.Errorf("unexpected RFB version %q", string(version))
	}

	protocol := rfbClientProtocol{securityResultAfterNone: true}
	switch string(clientVersion) {
	case "RFB 003.003\n":
		protocol.legacySecurityType = true
		protocol.securityResultAfterNone = false
	case "RFB 003.007\n":
		protocol.securityResultAfterNone = false
	}
	if _, err := conn.Write(clientVersion); err != nil {
		return rfbClientProtocol{}, fmt.Errorf("write RFB version: %w", err)
	}
	return protocol, nil
}

func negotiateRFBClientSecurityType(conn net.Conn, creds rfbCredentials, authMode localWebVNCAuthenticationMode, protocol rfbClientProtocol) (byte, error) {
	if !protocol.legacySecurityType {
		return negotiateRFBSecurityTypeForMode(conn, creds, authMode)
	}

	security := make([]byte, 4)
	if _, err := io.ReadFull(conn, security); err != nil {
		return 0, fmt.Errorf("read RFB 3.3 security type: %w", err)
	}
	selected := binary.BigEndian.Uint32(security)
	if selected == 0 {
		reason, err := readRFBReason(conn)
		if err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("RFB server rejected security negotiation: %s", reason)
	}
	if selected > 255 {
		return 0, fmt.Errorf("unsupported RFB 3.3 security type %d", selected)
	}
	securityType := byte(selected)
	if expected := rfbSecurityTypeForAuthenticationMode(authMode); expected != 0 && securityType != expected {
		return 0, fmt.Errorf("RFB server selected security type %d, want %d", securityType, expected)
	}
	return securityType, nil
}

func writeRFBPointerEvent(conn net.Conn, buttonMask byte, x, y int) error {
	if x < 0 || y < 0 || x > 0xffff || y > 0xffff {
		return fmt.Errorf("RFB pointer coordinates %d,%d are outside the uint16 protocol range", x, y)
	}
	message := []byte{5, buttonMask, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(message[2:4], uint16(x))
	binary.BigEndian.PutUint16(message[4:6], uint16(y))
	if _, err := conn.Write(message); err != nil {
		return fmt.Errorf("write RFB pointer event: %w", err)
	}
	return nil
}

func writeRFBKeyEvent(conn net.Conn, down bool, key uint32) error {
	message := make([]byte, 8)
	message[0] = 4
	if down {
		message[1] = 1
	}
	binary.BigEndian.PutUint32(message[4:8], key)
	if _, err := conn.Write(message); err != nil {
		return fmt.Errorf("write RFB key event: %w", err)
	}
	return nil
}

func negotiateRFBSecurityType(conn net.Conn, creds rfbCredentials) (byte, error) {
	return negotiateRFBSecurityTypeForMode(conn, creds, localWebVNCAuthAuto)
}

func negotiateRFBSecurityTypeForMode(conn net.Conn, creds rfbCredentials, authMode localWebVNCAuthenticationMode) (byte, error) {
	count := []byte{0}
	if _, err := io.ReadFull(conn, count); err != nil {
		return 0, fmt.Errorf("read RFB security type count: %w", err)
	}
	if count[0] == 0 {
		reason, err := readRFBReason(conn)
		if err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("RFB server rejected security negotiation: %s", reason)
	}
	types := make([]byte, count[0])
	if _, err := io.ReadFull(conn, types); err != nil {
		return 0, fmt.Errorf("read RFB security types: %w", err)
	}
	preferences := []byte{rfbSecurityARD, rfbSecurityVNC, rfbSecurityNone}
	if creds.Username == "" {
		preferences = []byte{rfbSecurityVNC, rfbSecurityARD, rfbSecurityNone}
	}
	if creds.Password == "" {
		preferences = []byte{rfbSecurityNone, rfbSecurityARD, rfbSecurityVNC}
	}
	if required := rfbSecurityTypeForAuthenticationMode(authMode); required != 0 {
		preferences = []byte{required}
	}
	for _, preferred := range preferences {
		for _, offered := range types {
			if offered != preferred {
				continue
			}
			if _, err := conn.Write([]byte{offered}); err != nil {
				return 0, fmt.Errorf("write RFB security type: %w", err)
			}
			return offered, nil
		}
	}
	return 0, fmt.Errorf("unsupported RFB security types %v", types)
}

func rfbSecurityTypeForAuthenticationMode(authMode localWebVNCAuthenticationMode) byte {
	switch authMode {
	case localWebVNCAuthVNC:
		return rfbSecurityVNC
	case localWebVNCAuthARD:
		return rfbSecurityARD
	default:
		return 0
	}
}

func negotiateRFBVNCAuth(conn net.Conn, creds rfbCredentials) error {
	if creds.Password == "" {
		return fmt.Errorf("VNC password is required")
	}
	challenge := make([]byte, 16)
	if _, err := io.ReadFull(conn, challenge); err != nil {
		return fmt.Errorf("read VNC auth challenge: %w", err)
	}
	var key [8]byte
	copy(key[:], []byte(creds.Password))
	for i := range key {
		key[i] = bits.Reverse8(key[i])
	}
	cipher, err := des.NewCipher(key[:])
	if err != nil {
		return fmt.Errorf("create VNC auth cipher: %w", err)
	}
	response := make([]byte, len(challenge))
	for offset := 0; offset < len(challenge); offset += cipher.BlockSize() {
		// RFB VNC Authentication mandates DES for protocol compatibility; it is
		// not used here to protect application data or stored credentials.
		cipher.Encrypt(response[offset:offset+cipher.BlockSize()], challenge[offset:offset+cipher.BlockSize()]) // lgtm[go/weak-cryptographic-algorithm]
	}
	if _, err := conn.Write(response); err != nil {
		return fmt.Errorf("write VNC auth response: %w", err)
	}
	return nil
}

func negotiateRFBARDAuth(conn net.Conn, creds rfbCredentials) error {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("read ARD auth header: %w", err)
	}
	keyLength := int(binary.BigEndian.Uint16(header[2:4]))
	if keyLength <= 0 || keyLength > 1024 {
		return fmt.Errorf("invalid ARD key length %d", keyLength)
	}
	params := make([]byte, keyLength*2)
	if _, err := io.ReadFull(conn, params); err != nil {
		return fmt.Errorf("read ARD auth parameters: %w", err)
	}
	g := new(big.Int).SetBytes(header[:2])
	p := new(big.Int).SetBytes(params[:keyLength])
	serverPublic := new(big.Int).SetBytes(params[keyLength:])
	if g.Sign() == 0 || p.Sign() == 0 || serverPublic.Sign() == 0 {
		return fmt.Errorf("invalid ARD Diffie-Hellman parameters")
	}
	privateBytes := make([]byte, keyLength)
	if _, err := rand.Read(privateBytes); err != nil {
		return fmt.Errorf("generate ARD private key: %w", err)
	}
	private := new(big.Int).SetBytes(privateBytes)
	clientPublic := new(big.Int).Exp(g, private, p)
	shared := new(big.Int).Exp(serverPublic, private, p)
	sharedBytes := leftPadBigInt(shared, keyLength)
	key := md5.Sum(sharedBytes)
	credentials, err := ardCredentialsBlock(creds)
	if err != nil {
		return err
	}
	encrypted, err := aesECBEncrypt(key[:], credentials)
	if err != nil {
		return err
	}
	out := make([]byte, 0, len(encrypted)+keyLength)
	out = append(out, encrypted...)
	out = append(out, leftPadBigInt(clientPublic, keyLength)...)
	if _, err := conn.Write(out); err != nil {
		return fmt.Errorf("write ARD auth response: %w", err)
	}
	return nil
}

func readRFBSecurityResult(conn net.Conn) error {
	statusBytes := make([]byte, 4)
	if _, err := io.ReadFull(conn, statusBytes); err != nil {
		return fmt.Errorf("read RFB security result: %w", err)
	}
	status := binary.BigEndian.Uint32(statusBytes)
	if status == 0 {
		return nil
	}
	reason, _ := readRFBReason(conn)
	if reason != "" {
		return fmt.Errorf("RFB authentication failed: %s", reason)
	}
	return fmt.Errorf("RFB authentication failed with status %d", status)
}

func readRFBReason(conn net.Conn) (string, error) {
	lengthBytes := make([]byte, 4)
	if _, err := io.ReadFull(conn, lengthBytes); err != nil {
		return "", fmt.Errorf("read RFB failure reason length: %w", err)
	}
	length := binary.BigEndian.Uint32(lengthBytes)
	if length == 0 {
		return "", nil
	}
	if length > 64*1024 {
		return "", fmt.Errorf("RFB failure reason is too large")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return "", fmt.Errorf("read RFB failure reason: %w", err)
	}
	return string(buf), nil
}

func readRFBServerInit(conn net.Conn) (uint16, uint16, error) {
	header := make([]byte, 24)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, 0, fmt.Errorf("read RFB server init: %w", err)
	}
	width := binary.BigEndian.Uint16(header[0:2])
	height := binary.BigEndian.Uint16(header[2:4])
	nameLength := binary.BigEndian.Uint32(header[20:24])
	if nameLength > 64*1024 {
		return 0, 0, fmt.Errorf("RFB desktop name is too large")
	}
	if nameLength > 0 {
		if _, err := io.CopyN(io.Discard, conn, int64(nameLength)); err != nil {
			return 0, 0, fmt.Errorf("read RFB desktop name: %w", err)
		}
	}
	return width, height, nil
}

func writeRFBPixelFormat(conn net.Conn) error {
	msg := make([]byte, 20)
	msg[0] = 0
	msg[4] = 32
	msg[5] = 24
	msg[6] = 0
	msg[7] = 1
	binary.BigEndian.PutUint16(msg[8:10], 255)
	binary.BigEndian.PutUint16(msg[10:12], 255)
	binary.BigEndian.PutUint16(msg[12:14], 255)
	msg[14] = 16
	msg[15] = 8
	msg[16] = 0
	if _, err := conn.Write(msg); err != nil {
		return fmt.Errorf("write RFB pixel format: %w", err)
	}
	return nil
}

func writeRFBSetEncodings(conn net.Conn) error {
	msg := make([]byte, 8)
	msg[0] = 2
	binary.BigEndian.PutUint16(msg[2:4], 1)
	binary.BigEndian.PutUint32(msg[4:8], uint32(rfbEncodingRaw))
	if _, err := conn.Write(msg); err != nil {
		return fmt.Errorf("write RFB encodings: %w", err)
	}
	return nil
}

func writeRFBFramebufferUpdateRequest(conn net.Conn, width, height uint16) error {
	msg := make([]byte, 10)
	msg[0] = 3
	msg[1] = 0
	binary.BigEndian.PutUint16(msg[6:8], width)
	binary.BigEndian.PutUint16(msg[8:10], height)
	if _, err := conn.Write(msg); err != nil {
		return fmt.Errorf("write RFB framebuffer update request: %w", err)
	}
	return nil
}

func readRFBFramebufferUpdate(conn net.Conn, width, height int) (image.Image, error) {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for {
		messageType := []byte{0}
		if _, err := io.ReadFull(conn, messageType); err != nil {
			return nil, fmt.Errorf("read RFB message type: %w", err)
		}
		switch messageType[0] {
		case 0:
			return readRFBFramebufferRectangles(conn, img)
		case 2:
			continue
		case 3:
			if err := discardRFBServerCutText(conn); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported RFB server message type %d", messageType[0])
		}
	}
}

func readRFBFramebufferRectangles(conn net.Conn, img *image.RGBA) (image.Image, error) {
	header := make([]byte, 3)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, fmt.Errorf("read RFB framebuffer update header: %w", err)
	}
	rectangles := binary.BigEndian.Uint16(header[1:3])
	for i := 0; i < int(rectangles); i++ {
		rectHeader := make([]byte, 12)
		if _, err := io.ReadFull(conn, rectHeader); err != nil {
			return nil, fmt.Errorf("read RFB rectangle header: %w", err)
		}
		x := int(binary.BigEndian.Uint16(rectHeader[0:2]))
		y := int(binary.BigEndian.Uint16(rectHeader[2:4]))
		w := int(binary.BigEndian.Uint16(rectHeader[4:6]))
		h := int(binary.BigEndian.Uint16(rectHeader[6:8]))
		encoding := int32(binary.BigEndian.Uint32(rectHeader[8:12]))
		if encoding != rfbEncodingRaw {
			return nil, fmt.Errorf("unsupported RFB rectangle encoding %d", encoding)
		}
		if x < 0 || y < 0 || w < 0 || h < 0 || x+w > img.Bounds().Dx() || y+h > img.Bounds().Dy() {
			return nil, fmt.Errorf("RFB rectangle outside framebuffer: x=%d y=%d w=%d h=%d", x, y, w, h)
		}
		raw := make([]byte, w*h*4)
		if _, err := io.ReadFull(conn, raw); err != nil {
			return nil, fmt.Errorf("read RFB raw rectangle: %w", err)
		}
		for row := 0; row < h; row++ {
			for col := 0; col < w; col++ {
				offset := (row*w + col) * 4
				img.SetRGBA(x+col, y+row, color.RGBA{
					R: raw[offset+2],
					G: raw[offset+1],
					B: raw[offset],
					A: 255,
				})
			}
		}
	}
	return img, nil
}

func discardRFBServerCutText(conn net.Conn) error {
	header := make([]byte, 7)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("read RFB cut text header: %w", err)
	}
	length := binary.BigEndian.Uint32(header[3:7])
	if length > 1024*1024 {
		return fmt.Errorf("RFB cut text is too large")
	}
	if _, err := io.CopyN(io.Discard, conn, int64(length)); err != nil {
		return fmt.Errorf("read RFB cut text: %w", err)
	}
	return nil
}

func ardCredentialsBlock(creds rfbCredentials) ([]byte, error) {
	username := []byte(creds.Username)
	password := []byte(creds.Password)
	if len(username) > 63 {
		username = username[:63]
	}
	if len(password) > 63 {
		password = password[:63]
	}
	block := make([]byte, 128)
	if _, err := rand.Read(block); err != nil {
		return nil, fmt.Errorf("generate ARD credentials padding: %w", err)
	}
	copy(block, username)
	block[len(username)] = 0
	copy(block[64:], password)
	block[64+len(password)] = 0
	return block, nil
}

func aesECBEncrypt(key, plaintext []byte) ([]byte, error) {
	if len(plaintext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("AES-ECB plaintext must be block aligned")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	out := make([]byte, len(plaintext))
	for offset := 0; offset < len(plaintext); offset += aes.BlockSize {
		block.Encrypt(out[offset:offset+aes.BlockSize], plaintext[offset:offset+aes.BlockSize])
	}
	return out, nil
}

func leftPadBigInt(value *big.Int, length int) []byte {
	out := make([]byte, length)
	bytes := value.Bytes()
	if len(bytes) > length {
		bytes = bytes[len(bytes)-length:]
	}
	copy(out[length-len(bytes):], bytes)
	return out
}
