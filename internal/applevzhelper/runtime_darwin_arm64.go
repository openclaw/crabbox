//go:build darwin && arm64

package applevzhelper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Code-Hex/vz/v3"
	"github.com/lima-vm/go-qcow2reader"
	"golang.org/x/sys/unix"
)

const helperLogFileName = "helper.log"

type startConfig struct {
	StateRoot    string
	Instance     Instance
	SSHPublicKey string
}

func prepareInstanceAssets(ctx context.Context, cfg startConfig) (Instance, error) {
	inst := cfg.Instance
	if err := os.MkdirAll(InstanceDir(cfg.StateRoot, inst.Name), 0o755); err != nil {
		return Instance{}, fmt.Errorf("create instance directory: %w", err)
	}
	sourcePath, err := resolveSourceImage(ctx, cfg.StateRoot, inst.Image)
	if err != nil {
		return Instance{}, err
	}
	rawPath, err := ensureRawImage(cfg.StateRoot, inst.Image, sourcePath)
	if err != nil {
		return Instance{}, err
	}
	inst.SourceImage = sourcePath
	inst.DiskPath = DiskPath(cfg.StateRoot, inst.Name)
	inst.SeedPath = SeedPath(cfg.StateRoot, inst.Name)
	inst.EFIVariableStorePath = EFIPath(cfg.StateRoot, inst.Name)
	inst.ConsoleLogPath = ConsoleLogPath(cfg.StateRoot, inst.Name)
	if err := cloneOrCopyFile(rawPath, inst.DiskPath); err != nil {
		return Instance{}, fmt.Errorf("clone base disk: %w", err)
	}
	if inst.DiskGiB > 0 {
		targetSize := int64(inst.DiskGiB) * 1024 * 1024 * 1024
		info, err := os.Stat(inst.DiskPath)
		if err != nil {
			return Instance{}, fmt.Errorf("stat cloned disk: %w", err)
		}
		if info.Size() < targetSize {
			if err := os.Truncate(inst.DiskPath, targetSize); err != nil {
				return Instance{}, fmt.Errorf("resize disk: %w", err)
			}
		}
	}
	if err := createSeedImage(ctx, inst.SeedPath, inst.Name, inst.SSHUser, cfg.SSHPublicKey, inst.WorkRoot); err != nil {
		return Instance{}, err
	}
	if err := os.MkdirAll(filepath.Dir(inst.ConsoleLogPath), 0o755); err != nil {
		return Instance{}, fmt.Errorf("create console log directory: %w", err)
	}
	if file, err := os.OpenFile(inst.ConsoleLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		_ = file.Close()
	}
	return inst, nil
}

func resolveSourceImage(ctx context.Context, stateRoot, image string) (string, error) {
	image = strings.TrimSpace(image)
	if image == "" {
		return "", fmt.Errorf("image is required")
	}
	if strings.HasPrefix(image, "http://") || strings.HasPrefix(image, "https://") {
		if err := os.MkdirAll(DownloadsDir(stateRoot), 0o755); err != nil {
			return "", fmt.Errorf("create downloads cache: %w", err)
		}
		sum := sha256.Sum256([]byte(image))
		name := filepath.Base(image)
		if name == "." || name == "/" || name == "" {
			name = "image.img"
		}
		target := filepath.Join(DownloadsDir(stateRoot), hex.EncodeToString(sum[:8])+"-"+name)
		if _, err := os.Stat(target); err == nil {
			return target, nil
		}
		tmp := target + ".tmp"
		_ = os.Remove(tmp)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, image, nil)
		if err != nil {
			return "", fmt.Errorf("build image request: %w", err)
		}
		req.Header.Set("User-Agent", "crabbox-apple-vz-helper")
		resp, err := (&http.Client{Timeout: 2 * time.Hour}).Do(req)
		if err != nil {
			return "", fmt.Errorf("download image: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("download image: http %d", resp.StatusCode)
		}
		file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return "", fmt.Errorf("create image cache file: %w", err)
		}
		if _, err := io.Copy(file, resp.Body); err != nil {
			file.Close()
			_ = os.Remove(tmp)
			return "", fmt.Errorf("write image cache file: %w", err)
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(tmp)
			return "", fmt.Errorf("close image cache file: %w", err)
		}
		if err := os.Rename(tmp, target); err != nil {
			_ = os.Remove(tmp)
			return "", fmt.Errorf("commit image cache file: %w", err)
		}
		return target, nil
	}
	path := image
	if strings.HasPrefix(path, "~"+string(os.PathSeparator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve image path: %w", err)
		}
		path = abs
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("image %q: %w", path, err)
	}
	return path, nil
}

func ensureRawImage(stateRoot, sourceRef, sourcePath string) (string, error) {
	qcow2, err := isQCOW2(sourcePath)
	if err != nil {
		return "", err
	}
	if !qcow2 {
		return sourcePath, nil
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		return "", fmt.Errorf("stat source image: %w", err)
	}
	if err := os.MkdirAll(ImagesDir(stateRoot), 0o755); err != nil {
		return "", fmt.Errorf("create image cache: %w", err)
	}
	key := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%d", sourceRef, sourcePath, info.Size(), info.ModTime().UnixNano())))
	target := filepath.Join(ImagesDir(stateRoot), hex.EncodeToString(key[:])+".raw")
	if _, err := os.Stat(target); err == nil {
		return target, nil
	}
	tmp := target + ".tmp"
	_ = os.Remove(tmp)
	if err := convertQCOW2ToRaw(sourcePath, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("commit raw image: %w", err)
	}
	return target, nil
}

func isQCOW2(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open image: %w", err)
	}
	defer file.Close()
	header := make([]byte, 4)
	if _, err := io.ReadFull(file, header); err != nil {
		return false, fmt.Errorf("read image header: %w", err)
	}
	return bytes.Equal(header, []byte{'Q', 'F', 'I', 0xfb}), nil
}

func convertQCOW2ToRaw(sourcePath, targetPath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open qcow2 image: %w", err)
	}
	defer source.Close()
	image, err := qcow2reader.Open(source)
	if err != nil {
		return fmt.Errorf("open qcow2 image: %w", err)
	}
	defer image.Close()
	if err := image.Readable(); err != nil {
		return fmt.Errorf("qcow2 image not readable: %w", err)
	}
	size := image.Size()
	if size <= 0 {
		return fmt.Errorf("qcow2 image size is unavailable")
	}
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create raw image: %w", err)
	}
	defer target.Close()
	if err := target.Truncate(size); err != nil {
		return fmt.Errorf("size raw image: %w", err)
	}
	buf := make([]byte, 4*1024*1024)
	for offset := int64(0); offset < size; {
		extent, err := image.Extent(offset, size-offset)
		if err != nil {
			return fmt.Errorf("read qcow2 extent at %d: %w", offset, err)
		}
		end := extent.Start + extent.Length
		if end <= offset {
			return fmt.Errorf("invalid qcow2 extent at %d", offset)
		}
		start := offset
		if extent.Start > start {
			start = extent.Start
		}
		if extent.Allocated && !extent.Zero {
			if err := copyReaderAtRange(target, image, start, end-start, buf); err != nil {
				return fmt.Errorf("copy qcow2 extent at %d: %w", start, err)
			}
		}
		offset = end
	}
	return nil
}

func cloneOrCopyFile(sourcePath, targetPath string) error {
	_ = os.Remove(targetPath)
	if err := unix.Clonefile(sourcePath, targetPath, 0); err == nil {
		return nil
	} else if !errors.Is(err, unix.EXDEV) && !errors.Is(err, unix.ENOTSUP) && !errors.Is(err, unix.EPERM) && !errors.Is(err, syscall.ENOTSUP) {
		return fmt.Errorf("clone file: %w", err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open source file: %w", err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return fmt.Errorf("stat source file: %w", err)
	}
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create target file: %w", err)
	}
	defer target.Close()
	if err := target.Truncate(info.Size()); err != nil {
		return fmt.Errorf("size target file: %w", err)
	}
	buf := make([]byte, 4*1024*1024)
	return copyReaderAtRange(target, source, 0, info.Size(), buf)
}

func copyReaderAtRange(target *os.File, source io.ReaderAt, offset, length int64, buf []byte) error {
	for copied := int64(0); copied < length; {
		chunk := int64(len(buf))
		if remaining := length - copied; remaining < chunk {
			chunk = remaining
		}
		n, err := source.ReadAt(buf[:chunk], offset+copied)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if n == 0 {
			if errors.Is(err, io.EOF) {
				break
			}
			return io.ErrUnexpectedEOF
		}
		part := buf[:n]
		if !allZero(part) {
			if _, err := target.WriteAt(part, offset+copied); err != nil {
				return err
			}
		}
		copied += int64(n)
	}
	return nil
}

func allZero(buf []byte) bool {
	for _, b := range buf {
		if b != 0 {
			return false
		}
	}
	return true
}

func createSeedImage(ctx context.Context, path, hostName, user, publicKey, workRoot string) error {
	tmpDir, err := os.MkdirTemp("", "crabbox-apple-vz-seed-*")
	if err != nil {
		return fmt.Errorf("create seed temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	if err := os.WriteFile(filepath.Join(tmpDir, "meta-data"), []byte(fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", hostName, hostName)), 0o644); err != nil {
		return fmt.Errorf("write seed meta-data: %w", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "user-data"), []byte(seedUserData(user, publicKey, workRoot)), 0o644); err != nil {
		return fmt.Errorf("write seed user-data: %w", err)
	}
	_ = os.Remove(path)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create seed image: %w", err)
	}
	if err := file.Truncate(8 * 1024 * 1024); err != nil {
		file.Close()
		return fmt.Errorf("size seed image: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("create seed image: %w", err)
	}
	attachOut, err := exec.CommandContext(ctx, "hdiutil", "attach", "-imagekey", "diskimage-class=CRawDiskImage", "-nomount", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("attach seed image: %w: %s", err, strings.TrimSpace(string(attachOut)))
	}
	device := firstFieldLine(string(attachOut))
	if device == "" {
		return fmt.Errorf("attach seed image: missing device name")
	}
	detached := false
	defer func() {
		if !detached {
			_, _ = exec.Command("hdiutil", "detach", device).CombinedOutput()
		}
	}()
	if out, err := exec.CommandContext(ctx, "newfs_msdos", "-F", "16", "-v", "cidata", device).CombinedOutput(); err != nil {
		return fmt.Errorf("format seed image: %w: %s", err, strings.TrimSpace(string(out)))
	}
	mountDir := filepath.Join(tmpDir, "mnt")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		return fmt.Errorf("create seed mount dir: %w", err)
	}
	if out, err := exec.CommandContext(ctx, "mount", "-t", "msdos", device, mountDir).CombinedOutput(); err != nil {
		return fmt.Errorf("mount seed image: %w: %s", err, strings.TrimSpace(string(out)))
	}
	mounted := true
	defer func() {
		if mounted {
			_, _ = exec.Command("umount", mountDir).CombinedOutput()
		}
	}()
	for _, name := range []string{"meta-data", "user-data"} {
		if err := copyPlainFile(filepath.Join(tmpDir, name), filepath.Join(mountDir, name), 0o644); err != nil {
			return fmt.Errorf("populate seed image: %w", err)
		}
	}
	if out, err := exec.CommandContext(ctx, "umount", mountDir).CombinedOutput(); err != nil {
		return fmt.Errorf("unmount seed image: %w: %s", err, strings.TrimSpace(string(out)))
	}
	mounted = false
	if out, err := exec.CommandContext(ctx, "hdiutil", "detach", device).CombinedOutput(); err != nil {
		return fmt.Errorf("detach seed image: %w: %s", err, strings.TrimSpace(string(out)))
	}
	detached = true
	return nil
}

func seedUserData(user, publicKey, workRoot string) string {
	return strings.TrimSpace(fmt.Sprintf(`
#cloud-config
users:
  - default
  - name: %s
    gecos: Crabbox
    shell: /bin/bash
    lock_passwd: true
    sudo: ALL=(ALL) NOPASSWD:ALL
    groups: [adm, sudo]
    ssh_authorized_keys:
      - %s
write_files:
  - path: /usr/local/bin/crabbox-vsock-ssh-proxy.py
    owner: root:root
    permissions: "0755"
    content: |
%s
  - path: /etc/systemd/system/crabbox-vsock-ssh-proxy.service
    owner: root:root
    permissions: "0644"
    content: |
%s
  - path: /usr/local/bin/crabbox-ready
    owner: root:root
    permissions: "0755"
    content: |
%s
runcmd:
  - [mkdir, -p, %q]
  - [chown, %q, %q]
  - [systemctl, daemon-reload]
  - [systemctl, enable, --now, crabbox-vsock-ssh-proxy.service]
`, user, publicKey, indentBlock(vsockProxyPython), indentBlock(vsockProxyService), indentBlock(readyScript(workRoot)), workRoot, user+":"+user, workRoot)) + "\n"
}

func indentBlock(content string) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	for i, line := range lines {
		if line == "" {
			lines[i] = "      "
			continue
		}
		lines[i] = "      " + line
	}
	return strings.Join(lines, "\n")
}

func readyScript(workRoot string) string {
	return fmt.Sprintf(`#!/bin/sh
set -eu
test -d %q
systemctl is-active --quiet crabbox-vsock-ssh-proxy.service
`, workRoot)
}

const vsockProxyService = `[Unit]
Description=Crabbox VSOCK to SSH proxy
After=network.target ssh.service
Wants=ssh.service

[Service]
Type=simple
ExecStart=/usr/local/bin/crabbox-vsock-ssh-proxy.py
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
`

const vsockProxyPython = `#!/usr/bin/env python3
import socket
import threading

LISTEN_PORT = 2222
TARGET = ("127.0.0.1", 22)


def pump(src, dst):
    try:
        while True:
            data = src.recv(32768)
            if not data:
                break
            dst.sendall(data)
    except OSError:
        pass
    finally:
        try:
            dst.shutdown(socket.SHUT_WR)
        except OSError:
            pass


def handle(conn):
    upstream = socket.create_connection(TARGET)
    t1 = threading.Thread(target=pump, args=(conn, upstream), daemon=True)
    t2 = threading.Thread(target=pump, args=(upstream, conn), daemon=True)
    t1.start()
    t2.start()
    t1.join()
    t2.join()
    try:
        conn.close()
    finally:
        upstream.close()


sock = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind((socket.VMADDR_CID_ANY, LISTEN_PORT))
sock.listen(16)

while True:
    conn, _ = sock.accept()
    threading.Thread(target=handle, args=(conn,), daemon=True).start()
`

func runServe(stateRoot, name string, stdout, stderr io.Writer) error {
	inst, err := readMetadata(MetadataPath(stateRoot, name))
	if err != nil {
		return err
	}
	inst.PID = os.Getpid()
	inst.Status = StatusStarting
	inst.UpdatedAt = time.Now().UTC()
	if err := writeMetadata(MetadataPath(stateRoot, name), inst); err != nil {
		return err
	}
	vmConfig, closers, err := buildVMConfig(inst)
	if err != nil {
		inst.Status = StatusError
		inst.Error = err.Error()
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), inst)
		return err
	}
	defer closeFiles(closers)
	if ok, err := vmConfig.Validate(); err != nil {
		inst.Status = StatusError
		inst.Error = err.Error()
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), inst)
		return fmt.Errorf("validate vm config: %w", err)
	} else if !ok {
		inst.Status = StatusError
		inst.Error = "invalid vm configuration"
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), inst)
		return fmt.Errorf("invalid vm configuration")
	}
	vm, err := vz.NewVirtualMachine(vmConfig)
	if err != nil {
		inst.Status = StatusError
		inst.Error = err.Error()
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), inst)
		return fmt.Errorf("create virtual machine: %w", err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		inst.Status = StatusError
		inst.Error = err.Error()
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), inst)
		return fmt.Errorf("open local ssh proxy: %w", err)
	}
	defer listener.Close()
	tcpAddr, _ := listener.Addr().(*net.TCPAddr)
	inst.SSHHost = "127.0.0.1"
	inst.SSHPort = tcpAddr.Port
	inst.UpdatedAt = time.Now().UTC()
	if err := writeMetadata(MetadataPath(stateRoot, name), inst); err != nil {
		return err
	}
	proxyErr := make(chan error, 1)
	go func() {
		proxyErr <- serveLocalSSHProxy(listener, vm)
	}()
	if err := vm.Start(); err != nil {
		inst.Status = StatusError
		inst.Error = err.Error()
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), inst)
		return fmt.Errorf("start virtual machine: %w", err)
	}
	inst.Status = StatusRunning
	inst.Error = ""
	inst.UpdatedAt = time.Now().UTC()
	if err := writeMetadata(MetadataPath(stateRoot, name), inst); err != nil {
		return err
	}
	sigCh := make(chan os.Signal, 2)
	signalNotify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signalStop(sigCh)
	stateCh := vm.StateChangedNotify()
	for {
		select {
		case err := <-proxyErr:
			if err == nil || errors.Is(err, net.ErrClosed) {
				continue
			}
			inst.Status = StatusError
			inst.Error = err.Error()
			inst.UpdatedAt = time.Now().UTC()
			_ = writeMetadata(MetadataPath(stateRoot, name), inst)
			if stopErr := requestStop(vm); stopErr != nil {
				fmt.Fprintf(stderr, "apple-vz helper stop request failed for %s after proxy error: %v\n", name, stopErr)
			}
			return err
		case sig := <-sigCh:
			fmt.Fprintf(stderr, "apple-vz helper received %s for %s\n", sig.String(), name)
			inst.Status = StatusStopping
			inst.UpdatedAt = time.Now().UTC()
			_ = writeMetadata(MetadataPath(stateRoot, name), inst)
			if err := requestStop(vm); err != nil {
				fmt.Fprintf(stderr, "apple-vz helper stop request failed for %s: %v\n", name, err)
			}
		case state := <-stateCh:
			result := handleVMState(state, &inst, stateRoot, name, stdout)
			if result.requestStop {
				if err := requestStop(vm); err != nil {
					fmt.Fprintf(stderr, "apple-vz helper stop request failed for %s: %v\n", name, err)
				}
			}
			if result.done {
				return result.err
			}
		}
	}
}

type vmStateResult struct {
	done        bool
	requestStop bool
	err         error
}

func handleVMState(state vz.VirtualMachineState, inst *Instance, stateRoot, name string, stdout io.Writer) vmStateResult {
	switch state {
	case vz.VirtualMachineStateRunning:
		inst.Status = StatusRunning
		inst.Error = ""
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), *inst)
	case vz.VirtualMachineStateStopping:
		inst.Status = StatusStopping
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), *inst)
	case vz.VirtualMachineStateStopped:
		inst.Status = StatusStopped
		inst.Error = ""
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), *inst)
		return vmStateResult{done: true}
	case vz.VirtualMachineStateError:
		inst.Status = StatusError
		inst.Error = "vm entered VirtualMachineStateError"
		inst.UpdatedAt = time.Now().UTC()
		_ = writeMetadata(MetadataPath(stateRoot, name), *inst)
		return vmStateResult{
			done:        true,
			requestStop: true,
			err:         fmt.Errorf("vm entered error state"),
		}
	default:
		fmt.Fprintf(stdout, "apple-vz helper state=%s name=%s\n", state.String(), name)
	}
	return vmStateResult{}
}

func buildVMConfig(inst Instance) (*vz.VirtualMachineConfiguration, []*os.File, error) {
	var closers []*os.File
	efiStore, err := vz.NewEFIVariableStore(inst.EFIVariableStorePath, vz.WithCreatingEFIVariableStore())
	if err != nil {
		return nil, closers, fmt.Errorf("create EFI variable store: %w", err)
	}
	bootLoader, err := vz.NewEFIBootLoader(vz.WithEFIVariableStore(efiStore))
	if err != nil {
		return nil, closers, fmt.Errorf("create EFI boot loader: %w", err)
	}
	config, err := vz.NewVirtualMachineConfiguration(bootLoader, uint(inst.CPUs), uint64(inst.MemoryMiB)*1024*1024)
	if err != nil {
		return nil, closers, fmt.Errorf("create vm config: %w", err)
	}
	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, closers, fmt.Errorf("create entropy device: %w", err)
	}
	config.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})
	nat, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return nil, closers, fmt.Errorf("create nat attachment: %w", err)
	}
	netDevice, err := vz.NewVirtioNetworkDeviceConfiguration(nat)
	if err != nil {
		return nil, closers, fmt.Errorf("create network device: %w", err)
	}
	config.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netDevice})
	socketDevice, err := vz.NewVirtioSocketDeviceConfiguration()
	if err != nil {
		return nil, closers, fmt.Errorf("create socket device: %w", err)
	}
	config.SetSocketDevicesVirtualMachineConfiguration([]vz.SocketDeviceConfiguration{socketDevice})
	rootAttachment, err := vz.NewDiskImageStorageDeviceAttachment(inst.DiskPath, false)
	if err != nil {
		return nil, closers, fmt.Errorf("open root disk: %w", err)
	}
	rootDevice, err := vz.NewVirtioBlockDeviceConfiguration(rootAttachment)
	if err != nil {
		return nil, closers, fmt.Errorf("configure root disk: %w", err)
	}
	seedAttachment, err := vz.NewDiskImageStorageDeviceAttachment(inst.SeedPath, true)
	if err != nil {
		return nil, closers, fmt.Errorf("open seed disk: %w", err)
	}
	seedDevice, err := vz.NewVirtioBlockDeviceConfiguration(seedAttachment)
	if err != nil {
		return nil, closers, fmt.Errorf("configure seed disk: %w", err)
	}
	config.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{rootDevice, seedDevice})
	if inst.ConsoleLogPath != "" {
		readFile, err := os.Open("/dev/null")
		if err != nil {
			return nil, closers, fmt.Errorf("open /dev/null: %w", err)
		}
		writeFile, err := os.OpenFile(inst.ConsoleLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			readFile.Close()
			return nil, closers, fmt.Errorf("open console log: %w", err)
		}
		closers = append(closers, readFile, writeFile)
		consoleAttachment, err := vz.NewFileHandleSerialPortAttachment(readFile, writeFile)
		if err != nil {
			return nil, closers, fmt.Errorf("create serial console: %w", err)
		}
		consolePort, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(consoleAttachment)
		if err != nil {
			return nil, closers, fmt.Errorf("configure serial console: %w", err)
		}
		config.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{consolePort})
	}
	return config, closers, nil
}

func closeFiles(files []*os.File) {
	for _, file := range files {
		if file != nil {
			_ = file.Close()
		}
	}
}

func serveLocalSSHProxy(listener net.Listener, vm *vz.VirtualMachine) error {
	socketDevices := vm.SocketDevices()
	if len(socketDevices) == 0 {
		return fmt.Errorf("vm socket device unavailable")
	}
	socketDevice := socketDevices[0]
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		wg.Add(1)
		go func(localConn net.Conn) {
			defer wg.Done()
			defer localConn.Close()
			guestConn, err := socketDevice.Connect(GuestVSOCKSSHPort)
			if err != nil {
				return
			}
			defer guestConn.Close()
			bridgeConnections(localConn, guestConn)
		}(conn)
	}
}

func bridgeConnections(left, right net.Conn) {
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		_ = dst.SetDeadline(time.Now())
		done <- struct{}{}
	}
	go pipe(left, right)
	go pipe(right, left)
	<-done
	<-done
}

func requestStop(vm *vz.VirtualMachine) error {
	if vm.CanRequestStop() {
		ok, err := vm.RequestStop()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	if vm.CanStop() {
		return vm.Stop()
	}
	return nil
}

func validateRuntimeConfig(stateRoot, image string) (map[string]string, error) {
	if _, err := exec.LookPath("hdiutil"); err != nil {
		return nil, fmt.Errorf("hdiutil is required")
	}
	if _, err := exec.LookPath("newfs_msdos"); err != nil {
		return nil, fmt.Errorf("newfs_msdos is required")
	}
	tmpDir, err := os.MkdirTemp("", "crabbox-apple-vz-doctor-*")
	if err != nil {
		return nil, fmt.Errorf("create doctor temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	rootDisk := filepath.Join(tmpDir, "disk.raw")
	seedDisk := filepath.Join(tmpDir, "seed.raw")
	efiPath := filepath.Join(tmpDir, "efi.bin")
	if err := vz.CreateDiskImage(rootDisk, 64*1024*1024); err != nil {
		return nil, fmt.Errorf("create doctor root disk: %w", err)
	}
	if err := vz.CreateDiskImage(seedDisk, 8*1024*1024); err != nil {
		return nil, fmt.Errorf("create doctor seed disk: %w", err)
	}
	inst := Instance{
		Name:                 "doctor",
		Image:                image,
		SSHUser:              "crabbox",
		WorkRoot:             "/work/crabbox",
		CPUs:                 2,
		MemoryMiB:            2048,
		DiskPath:             rootDisk,
		SeedPath:             seedDisk,
		EFIVariableStorePath: efiPath,
		ConsoleLogPath:       filepath.Join(tmpDir, "console.log"),
	}
	config, closers, err := buildVMConfig(inst)
	if err != nil {
		return nil, err
	}
	defer closeFiles(closers)
	if ok, err := config.Validate(); err != nil {
		return nil, fmt.Errorf("validate runtime config: %w", err)
	} else if !ok {
		return nil, fmt.Errorf("validate runtime config: invalid configuration")
	}
	if _, err := vz.NewVirtualMachine(config); err != nil {
		return nil, fmt.Errorf("create runtime VM: %w", err)
	}
	return map[string]string{
		"state_root": stateRoot,
		"image":      image,
		"runtime":    "virtualization.framework",
		"host":       "darwin/arm64",
	}, nil
}

func firstFieldLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) > 0 {
			return fields[0]
		}
	}
	return ""
}

func copyPlainFile(sourcePath, targetPath string, mode os.FileMode) error {
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	return os.WriteFile(targetPath, data, mode)
}

func runCommand(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
