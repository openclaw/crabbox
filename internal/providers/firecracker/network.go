package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/containernetworking/cni/libcni"
)

func cleanupFirecrackerNetwork(ctx context.Context, record leaseStateRecord) error {
	if strings.TrimSpace(record.CNINetwork) == "" {
		return nil
	}

	var cleanupErr error
	cni := libcni.NewCNIConfigWithCacheDir([]string{record.CNIBinDir}, record.CNICacheDir, nil)
	networkConf, err := libcni.LoadConfList(record.CNIConfDir, record.CNINetwork)
	if err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("load firecracker CNI config %q: %w", record.CNINetwork, err))
	} else {
		runtimeConf := &libcni.RuntimeConf{
			ContainerID: record.VMID,
			NetNS:       record.NetNSPath,
			IfName:      firecrackerHostInterface,
		}
		if err := cni.DelNetworkList(ctx, networkConf, runtimeConf); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete firecracker CNI network %q: %w", record.CNINetwork, err))
		}
	}

	if path := strings.TrimSpace(record.NetNSPath); path != "" {
		if err := detachUnmount(path); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("unmount firecracker netns %s: %w", path, err))
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove firecracker netns %s: %w", path, err))
		}
	}

	if cacheDir := strings.TrimSpace(record.CNICacheDir); cacheDir != "" {
		if err := os.RemoveAll(cacheDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove firecracker CNI cache %s: %w", cacheDir, err))
		}
	}

	return cleanupErr
}
