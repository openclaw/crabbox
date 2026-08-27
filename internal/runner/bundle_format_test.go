package runner

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureBundle(t *testing.T) ([]byte, bundleManifest, []byte) {
	t.Helper()
	manifest := bundleManifest{Version: 1, BuildID: strings.Repeat("a", 40)}
	var payload bytes.Buffer
	for _, osName := range []string{"darwin", "linux", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			data := []byte(osName + "-" + arch)
			var packed bytes.Buffer
			gz := gzip.NewWriter(&packed)
			if _, err := gz.Write(data); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			rawHash, packedHash := sha256.Sum256(data), sha256.Sum256(packed.Bytes())
			manifest.Entries = append(manifest.Entries, bundleEntry{OS: osName, Arch: arch, SHA256: hex.EncodeToString(rawHash[:]), Size: int64(len(data)), PackedSHA256: hex.EncodeToString(packedHash[:]), PackedSize: int64(packed.Len()), Offset: int64(payload.Len())})
			payload.Write(packed.Bytes())
		}
	}
	return encodeFixtureBundle(t, manifest, payload.Bytes()), manifest, payload.Bytes()
}

func encodeFixtureBundle(t *testing.T, manifest bundleManifest, payload []byte) []byte {
	t.Helper()
	header, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	prefix := make([]byte, 12)
	copy(prefix, bundleMagic)
	binary.BigEndian.PutUint32(prefix[8:], uint32(len(header)))
	return append(append(prefix, header...), payload...)
}

func TestRunnerBundleSelectsExactTarget(t *testing.T) {
	data, manifest, _ := fixtureBundle(t)
	for _, entry := range manifest.Entries {
		artifact, err := artifactFromBundle(data, manifest.BuildID, Target{OS: entry.OS, Arch: entry.Arch})
		if err != nil || string(artifact.Data) != entry.OS+"-"+entry.Arch || artifact.Identity.BuildID != manifest.BuildID {
			t.Fatalf("artifact=%v err=%v", artifact, err)
		}
	}
	if _, err := artifactFromBundle(data, "wrong", Target{OS: "linux", Arch: "amd64"}); err == nil {
		t.Fatal("wrong source identity accepted")
	}
}

func TestRunnerBundleRejectsCorruptionAndAmbiguousInventory(t *testing.T) {
	for _, mutate := range []func(*bundleManifest){
		func(m *bundleManifest) { m.Entries = m.Entries[:5] },
		func(m *bundleManifest) { m.Entries[1].OS = m.Entries[0].OS; m.Entries[1].Arch = m.Entries[0].Arch },
		func(m *bundleManifest) { m.Entries[0].Offset = 1 },
		func(m *bundleManifest) { m.Entries[0].Size = maxRunnerBytes + 1 },
		func(m *bundleManifest) { m.Entries[0].Size++ },
		func(m *bundleManifest) { m.Entries[0].PackedSize++ },
		func(m *bundleManifest) { m.Entries[0].SHA256 = strings.Repeat("0", 64) },
	} {
		_, manifest, payload := fixtureBundle(t)
		mutate(&manifest)
		if _, err := artifactFromBundle(encodeFixtureBundle(t, manifest, payload), manifest.BuildID, Target{OS: "darwin", Arch: "amd64"}); err == nil {
			t.Fatalf("malformed manifest accepted: %+v", manifest)
		}
	}
	data, manifest, _ := fixtureBundle(t)
	for _, malformed := range [][]byte{data[:11], data[:len(data)-1], append(append([]byte(nil), data...), 0)} {
		if _, err := artifactFromBundle(malformed, manifest.BuildID, Target{OS: "darwin", Arch: "amd64"}); err == nil {
			t.Fatal("invalid bundle length accepted")
		}
	}
}

func TestProtectedRunnerPackerMatchesRuntimeFormat(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node unavailable")
	}
	directory := t.TempDir()
	for _, osName := range []string{"darwin", "linux", "windows"} {
		for _, arch := range []string{"amd64", "arm64"} {
			name := "crabbox-runner-" + osName + "-" + arch
			if osName == "windows" {
				name += ".exe"
			}
			if err := os.WriteFile(filepath.Join(directory, name), []byte(osName+"-"+arch), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, width := range []int{40, 64} {
		output := filepath.Join(t.TempDir(), "bundle.bin")
		id := strings.Repeat("a", width)
		command := exec.CommandContext(t.Context(), "node", "../../scripts/pack-release-runners.mjs", directory, id, output)
		if diagnostic, err := command.CombinedOutput(); err != nil {
			t.Fatalf("pack: %v: %s", err, diagnostic)
		}
		data, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		for _, osName := range []string{"darwin", "linux", "windows"} {
			for _, arch := range []string{"amd64", "arm64"} {
				artifact, err := artifactFromBundle(data, id, Target{OS: osName, Arch: arch})
				if err != nil || string(artifact.Data) != osName+"-"+arch || artifact.Identity.BuildID != id {
					t.Fatalf("build width=%d target=%s/%s err=%v", width, osName, arch, err)
				}
			}
		}
	}
}

func TestRunnerBundleRejectsOtherBuildIdentityWidths(t *testing.T) {
	_, manifest, payload := fixtureBundle(t)
	for _, id := range []string{"", strings.Repeat("a", 39), strings.Repeat("a", 42), strings.Repeat("a", 62), strings.Repeat("a", 66), strings.Repeat("A", 64), strings.Repeat("z", 64)} {
		manifest.BuildID = id
		if _, err := artifactFromBundle(encodeFixtureBundle(t, manifest, payload), id, Target{OS: "linux", Arch: "amd64"}); err == nil {
			t.Fatalf("unsupported build identity accepted: %q", id)
		}
	}
}

func TestBundleParsersRejectSameMalformedBytes(t *testing.T) {
	node, nodeErr := exec.LookPath("node")
	module, err := filepath.Abs("../../scripts/runner-release-bundle.mjs")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"duplicate", "case-alias", "key-order", "target-order", "unselected-gzip", "unselected-digest", "gzip-trailer"} {
		t.Run(name, func(t *testing.T) {
			_, manifest, payload := fixtureBundle(t)
			header, _ := json.Marshal(manifest)
			switch name {
			case "duplicate":
				header = bytes.Replace(header, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1)
			case "case-alias":
				header = bytes.Replace(header, []byte(`"version"`), []byte(`"Version"`), 1)
			case "key-order":
				header = []byte(`{"buildId":"` + manifest.BuildID + `","version":1,"entries":` + strings.SplitN(string(header), `"entries":`, 2)[1])
			case "target-order":
				manifest.Entries[0].Arch, manifest.Entries[1].Arch = manifest.Entries[1].Arch, manifest.Entries[0].Arch
				header, _ = json.Marshal(manifest)
			case "unselected-gzip", "unselected-digest", "gzip-trailer":
				last := &manifest.Entries[5]
				if name == "unselected-digest" {
					last.SHA256 = strings.Repeat("0", 64)
				} else {
					if name == "unselected-gzip" {
						payload[last.Offset] = 0
					} else {
						var extra bytes.Buffer
						gz := gzip.NewWriter(&extra)
						if err := gz.Close(); err != nil {
							t.Fatal(err)
						}
						payload = append(payload, extra.Bytes()...)
						last.PackedSize += int64(extra.Len())
					}
					digest := sha256.Sum256(payload[last.Offset:])
					last.PackedSHA256 = hex.EncodeToString(digest[:])
				}
				header, _ = json.Marshal(manifest)
			}
			prefix := make([]byte, 12)
			copy(prefix, bundleMagic)
			binary.BigEndian.PutUint32(prefix[8:], uint32(len(header)))
			data := append(append(prefix, header...), payload...)
			if _, err := artifactFromBundle(data, manifest.BuildID, Target{OS: "darwin", Arch: "amd64"}); err == nil {
				t.Fatal("Go parser accepted malformed bundle")
			}
			if nodeErr != nil {
				t.Log("JavaScript parity check unavailable: Node not installed")
				return
			}
			fixture := filepath.Join(t.TempDir(), "bundle.bin")
			if err := os.WriteFile(fixture, data, 0o600); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), node, "--input-type=module", "-e", `
import fs from 'node:fs'; import {pathToFileURL} from 'node:url';
const {unpackRunnerBundle} = await import(pathToFileURL(process.argv[1]));
try { unpackRunnerBundle(fs.readFileSync(process.argv[2]), process.argv[3]); process.exit(1); }
catch { process.stdout.write('rejected'); }
`, module, fixture, manifest.BuildID)
			output, err := command.CombinedOutput()
			if err != nil || string(output) != "rejected" {
				t.Fatalf("JavaScript parity: %v: %s", err, output)
			}
		})
	}
}
