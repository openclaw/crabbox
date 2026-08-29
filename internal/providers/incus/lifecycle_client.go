package incus

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

// mutate preserves the client's OIDC transaction around the whole async operation.
func (c *sdkClient) mutate(ctx context.Context, start func() (incusclient.Operation, error)) (incusclient.Operation, error) {
	if err := c.beginOperation(); err != nil {
		return nil, err
	}
	defer c.endOperation()
	if err := c.persistResult(nil); err != nil {
		return nil, err
	}
	op, err := start()
	if err == nil {
		err = op.WaitContext(ctx)
		if err == nil && op.Get().StatusCode != api.Success {
			err = fmt.Errorf("Incus operation has not completed; outcome remains uncertain")
		}
	}
	if err != nil {
		return op, c.persistResult(err)
	}
	c.persistCommittedMutation()
	return op, nil
}

func (c *sdkClient) Identity() (connectionIdentity, error) {
	if err := c.beginOperation(); err != nil {
		return connectionIdentity{}, err
	}
	defer c.endOperation()
	server, _, err := c.server.GetServer()
	if err != nil {
		return connectionIdentity{}, c.persistResult(err)
	}
	info, err := c.server.GetConnectionInfo()
	if err != nil {
		return connectionIdentity{}, err
	}
	endpoint := info.URL
	if info.SocketPath != "" {
		endpoint = "unix:" + info.SocketPath
	}
	identity := connectionIdentity{Endpoint: endpoint, Project: info.Project, Certificate: server.Environment.CertificateFingerprint}
	if identity.Endpoint == "" || identity.Project == "" || identity.Certificate == "" {
		return connectionIdentity{}, fmt.Errorf("Incus did not report a complete endpoint/project/certificate identity")
	}
	return identity, c.persistResult(nil)
}

func (c *sdkClient) CreateSnapshot(ctx context.Context, name, snapshot string) error {
	_, err := c.mutate(ctx, func() (incusclient.Operation, error) {
		return c.server.CreateInstanceSnapshot(name, api.InstanceSnapshotsPost{Name: snapshot, Stateful: false})
	})
	return err
}
func (c *sdkClient) GetSnapshot(name, snapshot string) (*api.InstanceSnapshot, error) {
	if err := c.beginOperation(); err != nil {
		return nil, err
	}
	defer c.endOperation()
	snap, _, err := c.server.GetInstanceSnapshot(name, snapshot)
	return snap, c.persistResult(err)
}
func (c *sdkClient) DeleteSnapshot(ctx context.Context, name, snapshot string) error {
	_, err := c.mutate(ctx, func() (incusclient.Operation, error) { return c.server.DeleteInstanceSnapshot(name, snapshot) })
	return err
}
func (c *sdkClient) PublishSnapshot(ctx context.Context, name, snapshot string, properties map[string]string) (string, error) {
	op, err := c.mutate(ctx, func() (incusclient.Operation, error) {
		// Export only overrides inherited metadata expiry for a nonzero Go time.
		// Unix zero is Incus's explicit no-expiry value (v7.1.0 driver_lxc.Export).
		return c.server.CreateImage(api.ImagesPost{ImagePut: api.ImagePut{Public: false, AutoUpdate: false, ExpiresAt: time.Unix(0, 0).UTC(), Properties: properties}, Source: &api.ImagesPostSource{Type: "snapshot", Name: name + "/" + snapshot}}, nil)
	})
	if op != nil {
		if fingerprint, ok := op.Get().Metadata["fingerprint"].(string); ok {
			return fingerprint, err
		}
	}
	if err == nil {
		err = fmt.Errorf("Incus publish returned no image fingerprint")
	}
	return "", err
}
func (c *sdkClient) ListImages() ([]api.Image, error) {
	if err := c.beginOperation(); err != nil {
		return nil, err
	}
	defer c.endOperation()
	images, err := c.server.GetImages()
	return images, c.persistResult(err)
}
func (c *sdkClient) GetImage(fingerprint string) (*api.Image, error) {
	if err := c.beginOperation(); err != nil {
		return nil, err
	}
	defer c.endOperation()
	image, _, err := c.server.GetImage(fingerprint)
	return image, c.persistResult(err)
}
func (c *sdkClient) DeleteImage(ctx context.Context, fingerprint string) error {
	_, err := c.mutate(ctx, func() (incusclient.Operation, error) { return c.server.DeleteImage(fingerprint) })
	return err
}
func (c *sdkClient) ReadFile(name, filePath string) ([]byte, error) {
	if err := c.beginOperation(); err != nil {
		return nil, err
	}
	defer c.endOperation()
	// The HTTP file endpoint uses stat.Size(), which is zero for procfs. SFTP
	// streams mountinfo correctly and follows guest symlinks inside forkfile's root.
	files, err := c.server.GetInstanceFileSFTP(name)
	if err != nil {
		return nil, c.persistResult(err)
	}
	defer files.Close()
	file, err := files.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("Incus file %s exceeds read limit", filePath)
	}
	return data, c.persistResult(nil)
}

func (c *sdkClient) WriteFile(name, path string, data []byte, mode int) error {
	if err := c.beginOperation(); err != nil {
		return err
	}
	defer c.endOperation()
	err := c.server.CreateInstanceFile(name, path, incusclient.InstanceFileArgs{Content: bytes.NewReader(data), UID: 0, GID: 0, Mode: mode, Type: "file", WriteMode: "overwrite"})
	return c.persistResult(err)
}

func (c *sdkClient) Profile(name string) (*api.Profile, error) {
	if err := c.beginOperation(); err != nil {
		return nil, err
	}
	defer c.endOperation()
	profile, _, err := c.server.GetProfile(name)
	return profile, c.persistResult(err)
}
func (c *sdkClient) CanonicalPath(name, filePath string) (string, error) {
	if err := c.beginOperation(); err != nil {
		return "", err
	}
	defer c.endOperation()
	files, err := c.server.GetInstanceFileSFTP(name)
	if err != nil {
		return "", c.persistResult(err)
	}
	defer files.Close()
	// Incus forkfile's SFTP RealPath cleans paths without resolving symlinks.
	// Require an already-normalized directory path and attest every component.
	if !path.IsAbs(filePath) || strings.TrimSuffix(filePath, "/") != path.Clean(filePath) {
		return "", fmt.Errorf("Incus checkpoint workspace must be an absolute normalized directory")
	}
	current := "/"
	for _, component := range strings.Split(strings.TrimPrefix(path.Clean(filePath), "/"), "/") {
		current = path.Join(current, component)
		info, err := files.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("Incus native checkpoint workspace cannot traverse symlink or non-directory %s; use --mode archive", current)
		}
	}
	return path.Clean(filePath), c.persistResult(nil)
}

func (c *sdkClient) ClearTemplates(name string) error {
	if err := c.beginOperation(); err != nil {
		return err
	}
	defer c.endOperation()
	metadata, etag, err := c.server.GetInstanceMetadata(name)
	if err != nil {
		return c.persistResult(err)
	}
	metadata.Templates = nil
	return c.persistResult(c.server.UpdateInstanceMetadata(name, *metadata, etag))
}
