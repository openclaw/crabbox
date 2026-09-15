// Command verify-native-binary checks executable format and architecture without
// executing the input. It does not establish source provenance or OS compatibility.
package main

import (
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/json"
	"fmt"
	"os"
)

type binaryIdentity struct {
	Format       string `json:"format"`
	Architecture string `json:"architecture"`
	Bits         int    `json:"bits"`
}

func verify(file, platform, arch string) (binaryIdentity, error) {
	identity := binaryIdentity{Architecture: arch, Bits: 64}
	if arch != "amd64" && arch != "arm64" {
		return binaryIdentity{}, fmt.Errorf("unsupported native architecture %q", arch)
	}
	switch platform {
	case "darwin":
		f, err := macho.Open(file)
		if err != nil {
			return binaryIdentity{}, err
		}
		defer f.Close()
		cpu := macho.CpuAmd64
		if arch == "arm64" {
			cpu = macho.CpuArm64
		}
		if f.Cpu != cpu || f.Magic != macho.Magic64 || f.Type != macho.TypeExec {
			return binaryIdentity{}, fmt.Errorf("expected thin 64-bit %s Mach-O executable", arch)
		}
		identity.Format = "mach-o"
	case "linux":
		f, err := elf.Open(file)
		if err != nil {
			return binaryIdentity{}, err
		}
		defer f.Close()
		machine := elf.EM_X86_64
		if arch == "arm64" {
			machine = elf.EM_AARCH64
		}
		if f.Machine != machine || f.Class != elf.ELFCLASS64 || f.Entry == 0 || (f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN) {
			return binaryIdentity{}, fmt.Errorf("expected 64-bit %s ELF executable or PIE", arch)
		}
		identity.Format = "elf"
	case "windows":
		f, err := pe.Open(file)
		if err != nil {
			return binaryIdentity{}, err
		}
		defer f.Close()
		machine := uint16(pe.IMAGE_FILE_MACHINE_AMD64)
		if arch == "arm64" {
			machine = pe.IMAGE_FILE_MACHINE_ARM64
		}
		_, optional64 := f.OptionalHeader.(*pe.OptionalHeader64)
		if f.Machine != machine || !optional64 || f.Characteristics&pe.IMAGE_FILE_EXECUTABLE_IMAGE == 0 || f.Characteristics&pe.IMAGE_FILE_DLL != 0 {
			return binaryIdentity{}, fmt.Errorf("expected 64-bit %s PE executable", arch)
		}
		identity.Format = "pe"
	default:
		return binaryIdentity{}, fmt.Errorf("unsupported native platform %q", platform)
	}
	return identity, nil
}

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: verify-native-binary <binary> <darwin|linux|windows> <amd64|arm64>")
		os.Exit(2)
	}
	identity, err := verify(os.Args[1], os.Args[2], os.Args[3])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(identity); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
