package runtimeartifact

import (
	"context"
	"debug/buildinfo"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"io"
	"runtime/debug"
)

type metadataReader struct {
	ctx       context.Context
	r         io.ReaderAt
	remaining int
}

func (r *metadataReader) ReadAt(p []byte, off int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > r.remaining {
		return 0, fmt.Errorf("executable metadata exceeds read limit")
	}
	r.remaining -= len(p)
	return r.r.ReadAt(p, off)
}

// inspectExecutable reads format and Go metadata without executing the artifact.
// Inspection support is independent of capability admission in the manifest.
func inspectExecutable(ctx context.Context, r io.ReaderAt, target Target) error {
	return inspectPackage(ctx, r, target, runtimePackage)
}

func inspectPackage(ctx context.Context, r io.ReaderAt, target Target, packagePath string) error {
	bounded := &metadataReader{ctx, r, maxMetadataRead}
	if err := inspectFormat(bounded, target); err != nil {
		return err
	}
	info, err := buildinfo.Read(bounded)
	if err != nil {
		return fmt.Errorf("read Go build metadata: %w", err)
	}
	return validatePackageBuildInfo(info, target, packagePath)
}

func validateELF(f *elf.File, target Target) error {
	machine := elf.EM_X86_64
	if target.Arch == "arm64" {
		machine = elf.EM_AARCH64
	}
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB || f.Machine != machine {
		return fmt.Errorf("ELF architecture does not match %s", target.Arch)
	}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("ELF is not an executable")
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("ELF requires an external interpreter")
		}
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		return fmt.Errorf("read ELF dependencies: %w", err)
	}
	if len(libs) != 0 {
		return fmt.Errorf("ELF requires shared libraries")
	}
	return nil
}

func validateBuildInfo(info *debug.BuildInfo, target Target) error {
	return validatePackageBuildInfo(info, target, runtimePackage)
}

func validatePackageBuildInfo(info *debug.BuildInfo, target Target, packagePath string) error {
	if info.Path != packagePath {
		return fmt.Errorf("Go package mismatch: got %q, want %q", info.Path, packagePath)
	}
	settings := make(map[string]string, len(info.Settings))
	for _, s := range info.Settings {
		if _, exists := settings[s.Key]; exists {
			return fmt.Errorf("duplicate Go build setting %q", s.Key)
		}
		settings[s.Key] = s.Value
	}
	for key, want := range map[string]string{"GOOS": target.OS, "GOARCH": target.Arch, "CGO_ENABLED": "0"} {
		if settings[key] != want {
			return fmt.Errorf("Go build setting %s must be %q (got %q)", key, want, settings[key])
		}
	}
	return nil
}

// inspectFormat preserves Linux's standalone-linking policy. Darwin and Windows
// normally import system runtimes even with CGO disabled; static inspection here
// does not impose a static-linking or system-library trust policy on those formats.
func inspectFormat(r io.ReaderAt, target Target) error {
	if target.Arch != "amd64" && target.Arch != "arm64" {
		return fmt.Errorf("unsupported executable architecture %s", target.Arch)
	}
	switch target.OS {
	case "linux":
		f, err := elf.NewFile(r)
		if err != nil {
			return fmt.Errorf("read ELF: %w", err)
		}
		defer f.Close()
		return validateELF(f, target)
	case "darwin":
		f, err := macho.NewFile(r)
		if err != nil {
			return fmt.Errorf("read Mach-O: %w", err)
		}
		defer f.Close()
		cpu := macho.CpuAmd64
		if target.Arch == "arm64" {
			cpu = macho.CpuArm64
		}
		if f.Magic != macho.Magic64 || f.ByteOrder != binary.LittleEndian || f.Cpu != cpu {
			return fmt.Errorf("Mach-O architecture does not match %s", target.Arch)
		}
		if f.Type != macho.TypeExec {
			return fmt.Errorf("Mach-O is not an executable")
		}
		return nil
	case "windows":
		f, err := pe.NewFile(r)
		if err != nil {
			return fmt.Errorf("read PE: %w", err)
		}
		defer f.Close()
		machine := uint16(pe.IMAGE_FILE_MACHINE_AMD64)
		if target.Arch == "arm64" {
			machine = pe.IMAGE_FILE_MACHINE_ARM64
		}
		if _, ok := f.OptionalHeader.(*pe.OptionalHeader64); !ok || f.Machine != machine {
			return fmt.Errorf("PE architecture does not match %s", target.Arch)
		}
		if f.Characteristics&pe.IMAGE_FILE_EXECUTABLE_IMAGE == 0 || f.Characteristics&pe.IMAGE_FILE_DLL != 0 {
			return fmt.Errorf("PE is not an executable")
		}
		return nil
	default:
		return fmt.Errorf("unsupported executable OS %s", target.OS)
	}
}
