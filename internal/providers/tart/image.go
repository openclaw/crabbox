package tart

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

// The raw registry manifest binds the VM config, NVRAM, and uncompressed disk
// chunks. Tart's cache name alone does not verify the downloaded contents.
//
//go:embed images/sequoia.manifest.json
var defaultTartManifest []byte

//go:embed images/sequoia.config.json
var defaultTartConfig []byte

type tartImageLayer struct {
	MediaType   string            `json:"mediaType"`
	Size        int64             `json:"size"`
	Digest      string            `json:"digest"`
	Annotations map[string]string `json:"annotations"`
}

type tartImageManifest struct {
	Layers      []tartImageLayer  `json:"layers"`
	Annotations map[string]string `json:"annotations"`
}

func verifyDefaultTartImage(ctx context.Context, image, storage, name string) (string, error) {
	if image != core.DefaultTartImage {
		return "", nil // Custom images remain explicit operator trust.
	}
	digest := "sha256:" + fmt.Sprintf("%x", sha256.Sum256(defaultTartManifest))
	if !strings.HasSuffix(core.DefaultTartImage, "@"+digest) {
		return "", fmt.Errorf("built-in Tart image metadata does not match its pinned digest")
	}
	dir, err := tartVMDirectory(storage, name)
	if err == nil {
		err = verifyTartImageFiles(ctx, dir, defaultTartManifest, defaultTartConfig)
	}
	if err != nil {
		return "", fmt.Errorf("verify built-in Tart image before boot: %w", err)
	}
	return digest, nil
}

func verifyTartImageFiles(ctx context.Context, dir string, manifestJSON, configJSON []byte) error {
	if _, err := os.Lstat(filepath.Join(dir, "state.vzvmsave")); !os.IsNotExist(err) {
		return fmt.Errorf("expected an unsuspended base image (state.vzvmsave must be absent)")
	}
	var manifest tartImageManifest
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		return fmt.Errorf("read reviewed manifest: %w", err)
	}
	var config, nvram *tartImageLayer
	var disk []tartImageLayer
	var diskSize int64
	for _, layer := range manifest.Layers {
		switch layer.MediaType {
		case "application/vnd.cirruslabs.tart.config.v1":
			if config != nil {
				return fmt.Errorf("duplicate config in reviewed manifest")
			}
			copy := layer
			config = &copy
		case "application/vnd.cirruslabs.tart.nvram.v1":
			if nvram != nil {
				return fmt.Errorf("duplicate NVRAM in reviewed manifest")
			}
			copy := layer
			nvram = &copy
		case "application/vnd.cirruslabs.tart.disk.v2":
			size, err := strconv.ParseInt(layer.Annotations["org.cirruslabs.tart.uncompressed-size"], 10, 64)
			if err != nil || size <= 0 || size > 1<<30 || diskSize > (1<<63-1)-size {
				return fmt.Errorf("invalid reviewed disk chunk size")
			}
			layer.Size = size
			layer.Digest = layer.Annotations["org.cirruslabs.tart.uncompressed-content-digest"]
			disk = append(disk, layer)
			diskSize += size
		default:
			return fmt.Errorf("unsupported layer type in reviewed manifest: %s", layer.MediaType)
		}
	}
	if config == nil || nvram == nil || len(disk) == 0 || strconv.FormatInt(diskSize, 10) != manifest.Annotations["org.cirruslabs.tart.uncompressed-disk-size"] {
		return fmt.Errorf("incomplete reviewed Tart image metadata")
	}
	if int64(len(configJSON)) != config.Size || "sha256:"+fmt.Sprintf("%x", sha256.Sum256(configJSON)) != config.Digest {
		return fmt.Errorf("reviewed VM config does not match the manifest")
	}
	if err := verifyTartImageFile(ctx, filepath.Join(dir, "config.json"), -1, func(file *os.File) error {
		data, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
		if err != nil {
			return err
		}
		if len(data) > 1<<20 {
			return fmt.Errorf("VM config exceeds size limit")
		}
		return verifyTartImageConfig(data, configJSON)
	}); err != nil {
		return err
	}
	if err := verifyTartImageFile(ctx, filepath.Join(dir, "nvram.bin"), nvram.Size, func(file *os.File) error {
		return verifyTartImageChunk(ctx, file, *nvram)
	}); err != nil {
		return err
	}
	return verifyTartImageFile(ctx, filepath.Join(dir, "disk.img"), diskSize, func(file *os.File) error {
		for i, layer := range disk {
			if err := verifyTartImageChunk(ctx, file, layer); err != nil {
				return fmt.Errorf("disk chunk %d: %w", i, err)
			}
		}
		return nil
	})
}

func verifyTartImageConfig(actual, reviewed []byte) error {
	decode := func(data []byte) (map[string]any, error) {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var cfg map[string]any
		if err := decoder.Decode(&cfg); err != nil {
			return nil, err
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return nil, fmt.Errorf("VM config must contain one JSON object")
		}
		// Swift and Go choose different values for duplicate object keys.
		// Reject them before comparison, after Decode enforces its depth limit.
		scan := json.NewDecoder(bytes.NewReader(data))
		scan.UseNumber()
		duplicate, err := core.JSONHasDuplicateKeys(scan)
		if err != nil {
			return nil, err
		}
		if duplicate {
			return nil, fmt.Errorf("VM config contains duplicate JSON keys")
		}
		return cfg, nil
	}
	a, err := decode(actual)
	if err != nil {
		return err
	}
	b, err := decode(reviewed)
	if err != nil {
		return err
	}
	// Native Tart may regenerate a clone's MAC and reserialize JSON. Every
	// other configuration field must still match the reviewed config.
	mac, ok := a["macAddress"].(string)
	address, err := net.ParseMAC(mac)
	if !ok || err != nil || len(address) != 6 || address[0]&1 != 0 {
		return fmt.Errorf("invalid cloned VM MAC address")
	}
	a["macAddress"] = b["macAddress"]
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("VM config differs from the reviewed image")
	}
	return nil
}

func verifyTartImageFile(ctx context.Context, path string, size int64, check func(*os.File) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || (size >= 0 && before.Size() != size) {
		return fmt.Errorf("%s must be a regular file with the reviewed size", filepath.Base(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return fmt.Errorf("%s changed while opening", filepath.Base(path))
	}
	if err := check(file); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return fmt.Errorf("%s changed during image verification", filepath.Base(path))
	}
	return ctx.Err()
}

func verifyTartImageChunk(ctx context.Context, file *os.File, layer tartImageLayer) error {
	hash := sha256.New()
	buffer := make([]byte, 256<<10)
	remaining := layer.Size
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		next := min(remaining, int64(len(buffer)))
		n, err := io.ReadFull(file, buffer[:next])
		if err != nil {
			return err
		}
		_, _ = hash.Write(buffer[:n])
		remaining -= int64(n)
	}
	if layer.Digest != fmt.Sprintf("sha256:%x", hash.Sum(nil)) {
		return fmt.Errorf("content digest does not match the reviewed image")
	}
	return nil
}
