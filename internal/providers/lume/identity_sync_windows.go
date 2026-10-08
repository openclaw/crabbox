//go:build windows

package lume

func syncLumeStorageDirectory(string) error {
	// Windows does not support flushing an opened directory with File.Sync.
	// The identity file itself is synced before the atomic link is published.
	return nil
}
