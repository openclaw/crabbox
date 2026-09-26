package cli

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"sync"
)

type syncDigestContextKey struct{}

type syncDigestEntry struct {
	info   os.FileInfo
	change [2]int64
	digest [sha256.Size]byte
}

// The cache belongs to one snapshot attempt sequence, never a lease or process.
// Every use still checks identity, mode, size, mtime and ctime from a fresh stat.
type syncDigestCache struct {
	mu      sync.Mutex
	entries map[string]syncDigestEntry
	bytes   int64
	hashes  int64
}

func withSyncDigests(ctx context.Context) context.Context {
	if syncDigests(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, syncDigestContextKey{}, &syncDigestCache{entries: make(map[string]syncDigestEntry)})
}

func syncDigests(ctx context.Context) *syncDigestCache {
	cache, _ := ctx.Value(syncDigestContextKey{}).(*syncDigestCache)
	return cache
}

func (cache *syncDigestCache) lookup(path string, info os.FileInfo) ([sha256.Size]byte, bool) {
	change, supported := syncDigestChangeTime(info)
	if cache == nil || !supported {
		return [sha256.Size]byte{}, false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[path]
	return entry.digest, ok && entry.change == change && sameSourceSnapshotIdentity(entry.info, info)
}

func (cache *syncDigestCache) put(path string, info os.FileInfo, digest [sha256.Size]byte) {
	change, supported := syncDigestChangeTime(info)
	if cache == nil || !supported {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.entries[path] = syncDigestEntry{info: info, change: change, digest: digest}
}

func (cache *syncDigestCache) recordHash(bytes int64) {
	if cache != nil {
		cache.mu.Lock()
		cache.bytes += bytes
		cache.hashes++
		cache.mu.Unlock()
	}
}

func sourceFileDigest(ctx context.Context, path string, info os.FileInfo) ([sha256.Size]byte, error) {
	cache := syncDigests(ctx)
	if digest, ok := cache.lookup(path, info); ok {
		return digest, ctx.Err()
	}
	h := sha256.New()
	written, err := copyObservedSourceFileBytes(ctx, h, path, info)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	digest := [sha256.Size]byte(h.Sum(nil))
	cache.recordHash(written)
	cache.put(path, info, digest)
	return digest, nil
}

func sameSyncDigestChangeTime(left, right os.FileInfo) bool {
	a, supported := syncDigestChangeTime(left)
	b, otherSupported := syncDigestChangeTime(right)
	return !supported || !otherSupported || a == b
}

func writeSyncFileDigest(ctx context.Context, h interface{ Write([]byte) (int, error) }, path string, info os.FileInfo) error {
	digest, err := sourceFileDigest(ctx, path, info)
	if err == nil {
		_, err = fmt.Fprintf(h, "sha256=%x\n", digest)
	}
	return err
}
