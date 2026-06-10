package cli

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	rfbSecurityNone = 1
	rfbSecurityARD  = 30
	rfbEncodingRaw  = 0
)

type rfbCredentials struct {
	Username string
	Password string
}

func captureRemoteMacVNCScreenshot(ctx context.Context, cfg Config, target SSHTarget, outputPath string) error {
	localPort := availableLocalVNCPort()
	tunnel, err := startVNCForegroundTunnel(ctx, target, localPort, "127.0.0.1", managedVNCPort)
	if err != nil {
		return err
	}
	defer stopProcess(tunnel)

	creds, credentialsAvailable := providerDesktopCredentials(cfg, target)
	if !credentialsAvailable {
		password := ""
		if out, err := runSSHOutput(ctx, target, vncPasswordCommand(target)); err == nil {
			password = strings.TrimSpace(out)
		}
		creds = rfbCredentials{
			Username: strings.TrimSpace(target.User),
			Password: password,
		}
	}
	img, err := captureRFBFrame(ctx, "127.0.0.1:"+localPort, creds)
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

func providerDesktopCredentials(cfg Config, target SSHTarget) (rfbCredentials, bool) {
	provider, err := ProviderFor(cfg.Provider)
	if err != nil {
		return rfbCredentials{}, false
	}
	return desktopCredentialsFromProvider(provider, cfg, target)
}

func desktopCredentialsFromProvider(provider Provider, cfg Config, target SSHTarget) (rfbCredentials, bool) {
	credentialProvider, ok := provider.(DesktopCredentialProvider)
	if !ok {
		return rfbCredentials{}, false
	}
	credentials, ok := credentialProvider.DesktopCredentials(cfg, target)
	if !ok {
		return rfbCredentials{}, false
	}
	username := strings.TrimSpace(credentials.Username)
	if username == "" {
		username = strings.TrimSpace(target.User)
	}
	return rfbCredentials{
		Username: username,
		Password: credentials.Password,
	}, true
}

func captureRFBFrame(ctx context.Context, address string, creds rfbCredentials) (image.Image, error) {
	dialer := net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return captureRFBFrameFromConn(ctx, conn, creds)
}

func captureRFBFrameFromConn(ctx context.Context, conn net.Conn, creds rfbCredentials) (image.Image, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	version := make([]byte, 12)
	if _, err := io.ReadFull(conn, version); err != nil {
		return nil, fmt.Errorf("read RFB version: %w", err)
	}
	if !bytes.HasPrefix(version, []byte("RFB ")) {
		return nil, fmt.Errorf("unexpected RFB version %q", string(version))
	}
	if _, err := conn.Write([]byte("RFB 003.008\n")); err != nil {
		return nil, fmt.Errorf("write RFB version: %w", err)
	}

	securityType, err := negotiateRFBSecurityType(conn)
	if err != nil {
		return nil, err
	}
	switch securityType {
	case rfbSecurityNone:
	case rfbSecurityARD:
		if err := negotiateRFBARDAuth(conn, creds); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported RFB security type %d", securityType)
	}
	if err := readRFBSecurityResult(conn); err != nil {
		return nil, err
	}

	if _, err := conn.Write([]byte{1}); err != nil {
		return nil, fmt.Errorf("write RFB client init: %w", err)
	}
	width, height, err := readRFBServerInit(conn)
	if err != nil {
		return nil, err
	}
	if width == 0 || height == 0 {
		return nil, fmt.Errorf("server reported empty framebuffer %dx%d", width, height)
	}
	if int(width)*int(height) > 16_000_000 {
		return nil, fmt.Errorf("framebuffer %dx%d is too large", width, height)
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

func negotiateRFBSecurityType(conn net.Conn) (byte, error) {
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
	for _, preferred := range types {
		switch preferred {
		case rfbSecurityARD, rfbSecurityNone:
			if _, err := conn.Write([]byte{preferred}); err != nil {
				return 0, fmt.Errorf("write RFB security type: %w", err)
			}
			return preferred, nil
		}
	}
	return 0, fmt.Errorf("unsupported RFB security types %v", types)
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
