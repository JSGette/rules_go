// Copyright 2024 The Go Authors. All rights reserved.
// Copyright 2026 The Bazel Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the prelink_fips.LICENSE file.

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

const (
	prelinkFIPSMagic = "\xff Go fipsinfo \xff\x00"
	prelinkFIPSSize  = 120 // magic, checksum, self pointer, and four address pairs
)

// finalizePrelinkELF performs the post-host-link FIPS checksum step from
// Go 1.26.7's cmd/link/internal/ld/fips140.go (newFipsObj, addSection, elffips),
// which is unchanged in Go 1.26.10. The caller must enforce a supported SDK
// version and the Linux amd64 target, and run
// build-ID finalization afterward. This private format is not a stable Go API.
// Executables without FIPS metadata need no checksum, as with stock cmd/link.
func finalizePrelinkELF(path string) (err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	ef, err := elf.NewFile(f)
	if err != nil {
		return fmt.Errorf("reading prelink ELF: %w", err)
	}
	if ef.Class != elf.ELFCLASS64 || ef.Data != elf.ELFDATA2LSB || ef.Machine != elf.EM_X86_64 || ef.Type != elf.ET_EXEC {
		return fmt.Errorf("prelink finalization requires a little-endian ELF64 amd64 ET_EXEC executable")
	}
	var section *elf.Section
	for _, candidate := range ef.Sections {
		if candidate.Name == ".go.fipsinfo" {
			if section != nil {
				return fmt.Errorf("multiple .go.fipsinfo sections")
			}
			section = candidate
		}
	}
	if section == nil {
		return nil
	}
	fileSize := uint64(stat.Size())
	withinFile := func(offset, size uint64) bool {
		return offset <= fileSize && size <= fileSize-offset
	}
	if section.Type != elf.SHT_PROGBITS || section.Flags&elf.SHF_COMPRESSED != 0 || section.Size != prelinkFIPSSize || !withinFile(section.Offset, section.Size) {
		return fmt.Errorf("invalid Go 1.26.7 .go.fipsinfo section")
	}
	var info [prelinkFIPSSize]byte
	if _, err := f.ReadAt(info[:], int64(section.Offset)); err != nil {
		return fmt.Errorf("reading .go.fipsinfo: %w", err)
	}
	if string(info[:len(prelinkFIPSMagic)]) != prelinkFIPSMagic {
		return fmt.Errorf("invalid Go 1.26.7 .go.fipsinfo magic")
	}
	if binary.LittleEndian.Uint64(info[48:56]) != section.Addr {
		return fmt.Errorf("invalid .go.fipsinfo self pointer")
	}
	for _, prog := range ef.Progs {
		if prog.Type == elf.PT_LOAD && (!withinFile(prog.Off, prog.Filesz) || prog.Filesz > prog.Memsz || prog.Filesz > ^uint64(0)-prog.Vaddr) {
			return fmt.Errorf("invalid file-backed ELF load segment")
		}
	}
	fileOffset := func(start, end uint64) (uint64, error) {
		if start <= end {
			for _, prog := range ef.Progs {
				if prog.Type != elf.PT_LOAD || start < prog.Vaddr {
					continue
				}
				delta := start - prog.Vaddr
				if delta <= prog.Filesz && end-start <= prog.Filesz-delta {
					return prog.Off + delta, nil
				}
			}
		}
		return 0, fmt.Errorf("FIPS address range [%#x, %#x) is outside file-backed ELF load segments", start, end)
	}
	if section.Addr > ^uint64(0)-section.Size {
		return fmt.Errorf(".go.fipsinfo address overflows")
	}
	metadataOffset, err := fileOffset(section.Addr, section.Addr+section.Size)
	if err != nil || metadataOffset != section.Offset {
		return fmt.Errorf(".go.fipsinfo is not mapped at its section address")
	}

	h := hmac.New(sha256.New, make([]byte, sha256.Size))
	h.Write([]byte("go fips object v1\n"))
	checksumOffset := section.Offset + uint64(len(prelinkFIPSMagic))
	for i := 0; i < 4; i++ {
		start := binary.LittleEndian.Uint64(info[56+16*i:])
		end := binary.LittleEndian.Uint64(info[64+16*i:])
		offset, err := fileOffset(start, end)
		if err != nil {
			return err
		}
		size := end - start
		if size != 0 && offset < checksumOffset+sha256.Size && checksumOffset < offset+size {
			return fmt.Errorf("FIPS address range includes its verification checksum")
		}
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], size)
		h.Write(length[:])
		if _, err := io.CopyN(h, io.NewSectionReader(f, int64(offset), int64(size)), int64(size)); err != nil {
			return fmt.Errorf("reading FIPS address range: %w", err)
		}
	}
	if _, err := f.WriteAt(h.Sum(nil), int64(checksumOffset)); err != nil {
		return fmt.Errorf("writing FIPS verification checksum: %w", err)
	}
	return nil
}
