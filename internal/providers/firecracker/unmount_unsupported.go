//go:build !linux

package firecracker

func detachUnmount(string) error {
	return nil
}
