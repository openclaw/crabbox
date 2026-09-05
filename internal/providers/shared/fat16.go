package shared

import (
	"encoding/binary"
	"fmt"
	"strings"

	core "github.com/openclaw/crabbox/internal/cli"
)

type FATFile struct {
	Name string
	Data []byte
}

func BuildFAT16Image(label string, files []FATFile, shortNameFormat, errorPrefix string) ([]byte, error) {
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
	clusterSize := sectorsPerCluster * bytesPerSector
	dataClusters := (totalSectors - firstDataSector) / sectorsPerCluster
	fatEntries := sectorsPerFAT * bytesPerSector / 2
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
	nextCluster := 2
	rootOffset := 0
	root := image[rootStart : rootStart+rootDirSectors*bytesPerSector]
	labelEntry := root[rootOffset : rootOffset+32]
	copyPadded(labelEntry[0:11], strings.ToUpper(label), ' ')
	labelEntry[11] = 0x08
	rootOffset += 32
	for i, file := range files {
		if strings.TrimSpace(file.Name) == "" {
			return nil, core.Exit(2, "%s file name is required", errorPrefix)
		}
		cluster := nextCluster
		clusterCount := (len(file.Data) + clusterSize - 1) / clusterSize
		if clusterCount == 0 {
			clusterCount = 1
		}
		if cluster-2+clusterCount > dataClusters || cluster+clusterCount > fatEntries {
			return nil, core.Exit(2, "%s payload is too large", errorPrefix)
		}
		short := fmt.Sprintf(shortNameFormat, i+1)
		checksum := fatShortChecksum([]byte(short))
		lfnEntries := fatLongNameEntries(file.Name, checksum)
		if rootOffset+(len(lfnEntries)+1)*32 > len(root) {
			return nil, core.Exit(2, "%s directory is too large", errorPrefix)
		}
		for c := 0; c < clusterCount; c++ {
			entry := (cluster + c) * 2
			next := uint16(0xffff)
			if c+1 < clusterCount {
				next = uint16(cluster + c + 1)
			}
			binary.LittleEndian.PutUint16(fat[entry:entry+2], next)
		}
		dataOffset := dataStart + (cluster-2)*clusterSize
		copy(image[dataOffset:dataOffset+len(file.Data)], file.Data)
		for _, entry := range lfnEntries {
			copy(root[rootOffset:rootOffset+32], entry[:])
			rootOffset += 32
		}
		entry := root[rootOffset : rootOffset+32]
		copy(entry[0:11], []byte(short))
		entry[11] = 0x20
		binary.LittleEndian.PutUint16(entry[26:28], uint16(cluster))
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
