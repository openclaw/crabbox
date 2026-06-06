package xcpng

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type xcpNgCloudInitPayload struct {
	UserData string
	MetaData string
}

func newCloudInitPayload(cfg Config, leaseID, slug, publicKey string) (xcpNgCloudInitPayload, error) {
	user := strings.TrimSpace(core.Blank(cfg.XCPNg.User, cfg.SSHUser))
	if user == "" {
		return xcpNgCloudInitPayload{}, exit(2, "xcp-ng cloud-init user is required")
	}
	publicKey = strings.TrimSpace(publicKey)
	if publicKey == "" {
		return xcpNgCloudInitPayload{}, exit(2, "xcp-ng cloud-init public key is required")
	}
	workRoot := core.Blank(cfg.XCPNg.WorkRoot, cfg.WorkRoot)
	var userData bytes.Buffer
	fmt.Fprintf(&userData, "#cloud-config\n")
	fmt.Fprintf(&userData, "users:\n")
	fmt.Fprintf(&userData, "  - name: %s\n", shellSafeCloudInitScalar(user))
	fmt.Fprintf(&userData, "    groups: [sudo]\n")
	fmt.Fprintf(&userData, "    shell: /bin/bash\n")
	fmt.Fprintf(&userData, "    sudo: ['ALL=(ALL) NOPASSWD:ALL']\n")
	fmt.Fprintf(&userData, "    ssh_authorized_keys:\n")
	fmt.Fprintf(&userData, "      - %s\n", shellSafeCloudInitScalar(publicKey))
	fmt.Fprintf(&userData, "package_update: true\n")
	fmt.Fprintf(&userData, "packages:\n")
	fmt.Fprintf(&userData, "  - git\n")
	fmt.Fprintf(&userData, "  - curl\n")
	fmt.Fprintf(&userData, "  - rsync\n")
	fmt.Fprintf(&userData, "  - jq\n")
	fmt.Fprintf(&userData, "  - openssh-server\n")
	fmt.Fprintf(&userData, "write_files:\n")
	fmt.Fprintf(&userData, "  - path: /usr/local/bin/crabbox-ready\n")
	fmt.Fprintf(&userData, "    permissions: '0755'\n")
	fmt.Fprintf(&userData, "    content: |\n")
	fmt.Fprintf(&userData, "      #!/usr/bin/env bash\n")
	fmt.Fprintf(&userData, "      set -euo pipefail\n")
	fmt.Fprintf(&userData, "      git --version >/dev/null\n")
	fmt.Fprintf(&userData, "      rsync --version >/dev/null\n")
	fmt.Fprintf(&userData, "      curl --version >/dev/null\n")
	fmt.Fprintf(&userData, "      jq --version >/dev/null\n")
	fmt.Fprintf(&userData, "      test -f /var/lib/crabbox/bootstrapped\n")
	fmt.Fprintf(&userData, "      test -w %s\n", shellQuote(workRoot))
	fmt.Fprintf(&userData, "runcmd:\n")
	fmt.Fprintf(&userData, "  - [mkdir, -p, %s, /var/cache/crabbox/pnpm, /var/cache/crabbox/npm, /var/lib/crabbox]\n", yamlSingleQuotedScalar(workRoot))
	fmt.Fprintf(&userData, "  - [chown, -R, %s:%s, %s, /var/cache/crabbox]\n", yamlSingleQuotedScalar(user), yamlSingleQuotedScalar(user), yamlSingleQuotedScalar(workRoot))
	fmt.Fprintf(&userData, "  - [systemctl, enable, --now, ssh]\n")
	fmt.Fprintf(&userData, "  - [touch, /var/lib/crabbox/bootstrapped]\n")
	fmt.Fprintf(&userData, "  - [/usr/local/bin/crabbox-ready]\n")
	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: crabbox-%s\n", shellSafeCloudInitScalar(leaseID), shellSafeCloudInitScalar(slug))
	return xcpNgCloudInitPayload{UserData: userData.String(), MetaData: metaData}, nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func shellSafeCloudInitScalar(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}

func yamlSingleQuotedScalar(value string) string {
	value = shellSafeCloudInitScalar(value)
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func configDriveLabels(base map[string]string) map[string]string {
	labels := make(map[string]string, len(base)+2)
	for key, value := range base {
		labels[key] = value
	}
	labels["resource"] = "config-drive"
	labels["cleanup_with_vm"] = "true"
	return labels
}

func vmDiskLabels(base map[string]string) map[string]string {
	labels := make(map[string]string, len(base)+2)
	for key, value := range base {
		labels[key] = value
	}
	labels["resource"] = "vm-disk"
	labels["cleanup_with_vm"] = "true"
	return labels
}

func buildConfigDriveImage(payload xcpNgCloudInitPayload) ([]byte, error) {
	files := []fatFile{
		{Name: "user-data", Data: []byte(payload.UserData)},
		{Name: "meta-data", Data: []byte(payload.MetaData)},
	}
	return buildFAT16Image("cidata", files)
}

type fatFile struct {
	Name string
	Data []byte
}

func buildFAT16Image(label string, files []fatFile) ([]byte, error) {
	const (
		bytesPerSector    = 512
		sectorsPerCluster = 4
		reservedSectors   = 1
		fatCount          = 2
		rootEntries       = 512
		totalSectors      = 20480
		sectorsPerFAT     = 40
	)
	rootDirSectors := rootEntries * 32 / bytesPerSector
	firstDataSector := reservedSectors + fatCount*sectorsPerFAT + rootDirSectors
	image := make([]byte, totalSectors*bytesPerSector)
	boot := image[:bytesPerSector]
	boot[0] = 0xeb
	boot[1] = 0x3c
	boot[2] = 0x90
	copy(boot[3:11], []byte("CRABBOX "))
	binary.LittleEndian.PutUint16(boot[11:13], bytesPerSector)
	boot[13] = sectorsPerCluster
	binary.LittleEndian.PutUint16(boot[14:16], reservedSectors)
	boot[16] = fatCount
	binary.LittleEndian.PutUint16(boot[17:19], rootEntries)
	binary.LittleEndian.PutUint16(boot[19:21], totalSectors)
	boot[21] = 0xf8
	binary.LittleEndian.PutUint16(boot[22:24], sectorsPerFAT)
	binary.LittleEndian.PutUint16(boot[24:26], 63)
	binary.LittleEndian.PutUint16(boot[26:28], 255)
	boot[36] = 0x80
	boot[38] = 0x29
	binary.LittleEndian.PutUint32(boot[39:43], 0x43525842)
	copyPadded(boot[43:54], strings.ToUpper(label), ' ')
	copy(boot[54:62], []byte("FAT16   "))
	boot[510] = 0x55
	boot[511] = 0xaa

	fatStart := reservedSectors * bytesPerSector
	rootStart := (reservedSectors + fatCount*sectorsPerFAT) * bytesPerSector
	dataStart := firstDataSector * bytesPerSector
	fat := image[fatStart : fatStart+sectorsPerFAT*bytesPerSector]
	binary.LittleEndian.PutUint16(fat[0:2], 0xfff8)
	binary.LittleEndian.PutUint16(fat[2:4], 0xffff)
	nextCluster := uint16(2)
	rootOffset := 0
	root := image[rootStart : rootStart+rootDirSectors*bytesPerSector]
	labelEntry := root[rootOffset : rootOffset+32]
	copyPadded(labelEntry[0:11], strings.ToUpper(label), ' ')
	labelEntry[11] = 0x08
	rootOffset += 32
	for i, file := range files {
		if strings.TrimSpace(file.Name) == "" {
			return nil, exit(2, "config-drive file name is required")
		}
		cluster := nextCluster
		clusterCount := uint16((len(file.Data) + sectorsPerCluster*bytesPerSector - 1) / (sectorsPerCluster * bytesPerSector))
		if clusterCount == 0 {
			clusterCount = 1
		}
		for c := uint16(0); c < clusterCount; c++ {
			entry := (cluster + c) * 2
			next := uint16(0xffff)
			if c+1 < clusterCount {
				next = cluster + c + 1
			}
			binary.LittleEndian.PutUint16(fat[entry:entry+2], next)
		}
		copy(image[dataStart+int(cluster-2)*sectorsPerCluster*bytesPerSector:], file.Data)
		short := fmt.Sprintf("CRAB%04dTXT", i+1)
		checksum := fatShortChecksum([]byte(short))
		lfnEntries := fatLongNameEntries(file.Name, checksum)
		for _, entry := range lfnEntries {
			copy(root[rootOffset:rootOffset+32], entry[:])
			rootOffset += 32
		}
		entry := root[rootOffset : rootOffset+32]
		copy(entry[0:11], []byte(short))
		entry[11] = 0x20
		binary.LittleEndian.PutUint16(entry[26:28], cluster)
		binary.LittleEndian.PutUint32(entry[28:32], uint32(len(file.Data)))
		rootOffset += 32
		nextCluster += clusterCount
	}
	copy(image[fatStart+sectorsPerFAT*bytesPerSector:fatStart+2*sectorsPerFAT*bytesPerSector], fat)
	return image, nil
}

func copyPadded(dst []byte, value string, pad byte) {
	for i := range dst {
		dst[i] = pad
	}
	copy(dst, []byte(value))
}

func fatShortChecksum(short []byte) byte {
	var sum byte
	for _, b := range short {
		sum = ((sum & 1) << 7) + (sum >> 1) + b
	}
	return sum
}

func fatLongNameEntries(name string, checksum byte) [][32]byte {
	runes := []rune(name)
	chunks := (len(runes) + 12) / 13
	entries := make([][32]byte, 0, chunks)
	for i := chunks - 1; i >= 0; i-- {
		var entry [32]byte
		sequence := byte(i + 1)
		if i == chunks-1 {
			sequence |= 0x40
		}
		entry[0] = sequence
		entry[11] = 0x0f
		entry[13] = checksum
		start := i * 13
		end := start + 13
		if end > len(runes) {
			end = len(runes)
		}
		writeLongNameRunes(entry[:], runes[start:end])
		entries = append(entries, entry)
	}
	return entries
}

func writeLongNameRunes(entry []byte, runes []rune) {
	positions := []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30}
	for i, pos := range positions {
		value := uint16(0xffff)
		if i < len(runes) {
			value = uint16(runes[i])
		} else if i == len(runes) {
			value = 0x0000
		}
		binary.LittleEndian.PutUint16(entry[pos:pos+2], value)
	}
}
