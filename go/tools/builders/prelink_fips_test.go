// Copyright 2026 The Bazel Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

const (
	fipsTestBase    = 0x400000
	fipsTestPayload = 0x100
	fipsTestInfo    = 0x180
	fipsTestNames   = 0x200
	fipsTestShdr    = 0x280
	fipsTestInfoHdr = fipsTestShdr + 2*64
)

// fipsTestELF constructs a non-executable fixture with a valid ELF header,
// one file-backed load segment, and the pinned FIPS metadata layout. It has
// four differently sized ranges, including an empty range.
func fipsTestELF(t *testing.T) []byte {
	t.Helper()
	data := make([]byte, fipsTestShdr+3*64)
	put := func(offset int, value interface{}) {
		t.Helper()
		var out bytes.Buffer
		if err := binary.Write(&out, binary.LittleEndian, value); err != nil {
			t.Fatal(err)
		}
		copy(data[offset:], out.Bytes())
	}
	put(0, elf.Header64{
		Ident:     [elf.EI_NIDENT]byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)},
		Type:      uint16(elf.ET_EXEC),
		Machine:   uint16(elf.EM_X86_64),
		Version:   uint32(elf.EV_CURRENT),
		Phoff:     64,
		Shoff:     fipsTestShdr,
		Ehsize:    64,
		Phentsize: 56,
		Phnum:     1,
		Shentsize: 64,
		Shnum:     3,
		Shstrndx:  1,
	})
	put(64, elf.Prog64{
		Type: uint32(elf.PT_LOAD), Flags: uint32(elf.PF_R),
		Vaddr: fipsTestBase, Paddr: fipsTestBase,
		Filesz: uint64(len(data)), Memsz: uint64(len(data)), Align: 0x1000,
	})
	names := "\x00.shstrtab\x00.go.fipsinfo\x00"
	copy(data[fipsTestNames:], names)
	put(fipsTestShdr+64, elf.Section64{
		Name: 1, Type: uint32(elf.SHT_STRTAB), Off: fipsTestNames,
		Size: uint64(len(names)), Addralign: 1,
	})
	put(fipsTestInfoHdr, elf.Section64{
		Name: 11, Type: uint32(elf.SHT_PROGBITS), Flags: uint64(elf.SHF_ALLOC),
		Addr: fipsTestBase + fipsTestInfo, Off: fipsTestInfo, Size: 120, Addralign: 8,
	})
	copy(data[fipsTestInfo:], "\xff Go fipsinfo \xff\x00")
	put(fipsTestInfo+48, uint64(fipsTestBase+fipsTestInfo))
	for i, contents := range []string{"abc", "defgh", "", "ijkl"} {
		offset := fipsTestPayload + 16*i
		copy(data[offset:], contents)
		put(fipsTestInfo+56+16*i, uint64(fipsTestBase+offset))
		put(fipsTestInfo+64+16*i, uint64(fipsTestBase+offset+len(contents)))
	}
	return data
}

func TestFinalizePrelinkELF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "executable")
	original := fipsTestELF(t)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := finalizePrelinkELF(path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Independent known-answer vector: zero-key HMAC-SHA256 of the Go domain
	// followed by big-endian uint64 lengths and "abc", "defgh", "", "ijkl".
	checksum, err := hex.DecodeString("12656af87a47626793ad8235c340c9b6435f7f1ffcf87ca94cb2a1c6f09984b7")
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), original...)
	copy(want[fipsTestInfo+16:], checksum)
	if !bytes.Equal(got, want) {
		t.Fatal("finalization did not write exactly the expected checksum")
	}
	if err := finalizePrelinkELF(path); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("repeated finalization changed the result: %v", err)
	}
}

func TestFinalizePrelinkELFWithoutMetadata(t *testing.T) {
	original := fipsTestELF(t)
	// Rename the FIPS section so the executable has no FIPS metadata.
	copy(original[fipsTestNames+11:], ".other_info")
	path := filepath.Join(t.TempDir(), "executable")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := finalizePrelinkELF(path); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("executable without FIPS metadata was changed: %v", err)
	}
}

func TestFinalizePrelinkELFRejectsMalformedInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte)
	}{
		{"not ELF", func(b []byte) { b[0] = 0 }},
		{"not amd64", func(b []byte) { binary.LittleEndian.PutUint16(b[18:], uint16(elf.EM_AARCH64)) }},
		{"PIE", func(b []byte) { binary.LittleEndian.PutUint16(b[16:], uint16(elf.ET_DYN)) }},
		{"duplicate metadata", func(b []byte) { binary.LittleEndian.PutUint32(b[fipsTestShdr+64:], 11) }},
		{"metadata type", func(b []byte) { binary.LittleEndian.PutUint32(b[fipsTestInfoHdr+4:], uint32(elf.SHT_NOBITS)) }},
		{"metadata size", func(b []byte) { binary.LittleEndian.PutUint64(b[fipsTestInfoHdr+32:], 119) }},
		{"metadata past EOF", func(b []byte) { binary.LittleEndian.PutUint64(b[fipsTestInfoHdr+24:], uint64(len(b)-16)) }},
		{"metadata magic", func(b []byte) { b[fipsTestInfo] = 0 }},
		{"self pointer", func(b []byte) { b[fipsTestInfo+48]++ }},
		{"metadata mapping", func(b []byte) {
			binary.LittleEndian.PutUint64(b[fipsTestInfoHdr+16:], fipsTestBase+fipsTestInfo+1)
			binary.LittleEndian.PutUint64(b[fipsTestInfo+48:], fipsTestBase+fipsTestInfo+1)
		}},
		{"segment past EOF", func(b []byte) { binary.LittleEndian.PutUint64(b[64+32:], uint64(len(b)+1)) }},
		{"segment address overflow", func(b []byte) { binary.LittleEndian.PutUint64(b[64+16:], ^uint64(0)-8) }},
		{"reversed range", func(b []byte) { binary.LittleEndian.PutUint64(b[fipsTestInfo+64:], fipsTestBase+fipsTestPayload-1) }},
		{"range outside segment", func(b []byte) { binary.LittleEndian.PutUint64(b[fipsTestInfo+64:], fipsTestBase+uint64(len(b))+1) }},
		{"range in zero-fill memory", func(b []byte) {
			binary.LittleEndian.PutUint64(b[64+40:], uint64(len(b)+128))
			binary.LittleEndian.PutUint64(b[fipsTestInfo+56:], fipsTestBase+uint64(len(b)))
			binary.LittleEndian.PutUint64(b[fipsTestInfo+64:], fipsTestBase+uint64(len(b))+1)
		}},
		{"range includes checksum", func(b []byte) {
			binary.LittleEndian.PutUint64(b[fipsTestInfo+56:], fipsTestBase+fipsTestInfo+16)
			binary.LittleEndian.PutUint64(b[fipsTestInfo+64:], fipsTestBase+fipsTestInfo+48)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := fipsTestELF(t)
			tt.mutate(original)
			path := filepath.Join(t.TempDir(), "executable")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			if err := finalizePrelinkELF(path); err == nil {
				t.Fatal("malformed executable was accepted")
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, original) {
				t.Fatalf("failed finalization modified the executable: %v", err)
			}
		})
	}
}
