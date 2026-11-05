package main

import (
	"compress/gzip"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func checkExecutable(path, system, arch string) error {
	var err error
	compatible := false
	switch system {
	case "linux":
		var program *elf.File
		program, err = elf.Open(path)
		if err == nil {
			defer program.Close()
			machine := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[arch]
			compatible = machine != elf.EM_NONE && program.Machine == machine && program.Class == elf.ELFCLASS64 &&
				program.Data == elf.ELFDATA2LSB && (program.Type == elf.ET_EXEC || program.Type == elf.ET_DYN)
		}
	case "darwin":
		var program *macho.File
		program, err = macho.Open(path)
		if err == nil {
			defer program.Close()
			cpu := map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[arch]
			compatible = cpu != 0 && program.Cpu == cpu && program.Type == macho.TypeExec
		}
	case "windows":
		var program *pe.File
		program, err = pe.Open(path)
		if err == nil {
			defer program.Close()
			compatible = arch == "amd64" && program.Machine == pe.IMAGE_FILE_MACHINE_AMD64 &&
				program.Characteristics&pe.IMAGE_FILE_EXECUTABLE_IMAGE != 0
		}
	}
	if compatible {
		return nil
	}
	if err == nil {
		err = errors.New("format or architecture mismatch")
	}
	return fmt.Errorf("Expected a %s/%s executable: %w", system, arch, err)
}

func (i *installer) checkProgram(path, system, arch string) error {
	if err := checkExecutable(path, system, arch); err != nil {
		cause := errors.Unwrap(err)
		if cause.Error() == "format or architecture mismatch" {
			cause = errors.New(i.msg("format or architecture mismatch"))
		}
		return fmt.Errorf(i.msg("Expected a %s/%s executable: %w"), system, arch, cause)
	}
	return nil
}

func extractPayload(system, arch string) (string, func(), error) {
	directory, err := os.MkdirTemp("", "sub2api-plugins-payload-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(directory) }
	name := "sub2api"
	if system == "windows" {
		name += ".exe"
	}
	path := filepath.Join(directory, name)
	err = func() error {
		compressed, err := assets.Open("payloads/" + system + "-" + arch + ".gz")
		if err != nil {
			return err
		}
		defer compressed.Close()
		reader, err := gzip.NewReader(compressed)
		if err != nil {
			return err
		}
		defer reader.Close()
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(f, reader)
		return errors.Join(copyErr, f.Close())
	}()
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}
