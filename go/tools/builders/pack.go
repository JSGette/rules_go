// Copyright 2026 The Bazel Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The Go-only archive contains the compiler archive followed by any Go
// assembly and .syso members. The conventional archive inserts native cgo
// objects between those parts, preserving the combined action's member order.
type goPackMetadata struct {
	CompilerArchiveSize int64 `json:"compiler_archive_size"`
}

func writeGoPackMetadata(path string, compilerArchiveSize int64) error {
	return writeCgoJSON(path, goPackMetadata{CompilerArchiveSize: compilerArchiveSize})
}

func packArchive(args []string) error {
	args, _, err := expandParamsFiles(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("GoPack", flag.ContinueOnError)
	goenv := envFlags(fs)
	var packTool, goArchive, metadata, objectDir, output string
	fs.StringVar(&packTool, "pack", "", "Path of the pack tool")
	fs.StringVar(&goArchive, "go_archive", "", "Go-only archive including Go assembly and .syso members")
	fs.StringVar(&metadata, "metadata", "", "Metadata identifying the compiler archive boundary")
	fs.StringVar(&objectDir, "cgo_objects", "", "Directory of ordered native cgo objects")
	fs.StringVar(&output, "o", "", "Conventional archive to write")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := goenv.checkFlagsAndSetGoroot(); err != nil {
		return err
	}
	if packTool == "" || goArchive == "" || metadata == "" || objectDir == "" || output == "" {
		return fmt.Errorf("-pack, -go_archive, -metadata, -cgo_objects, and -o are required")
	}
	return packGoArchive(goArchive, metadata, objectDir, output, func(out string, objects []string) error {
		return appendToArchive(goenv, packTool, out, objects)
	})
}

// packGoArchive keeps the native-tool invocation separate from the byte copies:
// pack must receive a closed, complete compiler archive before adding objects.
func packGoArchive(goArchive, metadataPath, objectDir, output string, appendNative func(string, []string) error) error {
	var metadata goPackMetadata
	if err := readCgoJSON(metadataPath, &metadata); err != nil {
		return fmt.Errorf("read Go pack metadata: %w", err)
	}
	_, objects, err := readCgoPrepared(objectDir, objectDir)
	if err != nil {
		return fmt.Errorf("read native objects: %w", err)
	}
	inputs := append([]string{goArchive, metadataPath, filepath.Join(objectDir, cgoManifestName)}, objects...)
	for _, input := range inputs {
		if err := checkGoPackOutput(input, output); err != nil {
			return err
		}
	}
	in, err := os.Open(goArchive)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if err := validateGoPackArchive(in, info.Size(), metadata.CompilerArchiveSize); err != nil {
		return fmt.Errorf("invalid Go archive %q: %w", goArchive, err)
	}
	// Without native members the conventional archive is exactly the Go archive.
	copySize := metadata.CompilerArchiveSize
	if len(objects) == 0 {
		copySize = info.Size()
	}
	out, err := os.Create(output)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyN(out, in, copySize)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if len(objects) == 0 {
		return nil
	}
	if err := appendNative(output, objects); err != nil {
		return err
	}
	out, err = os.OpenFile(output, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	_, copyErr = io.CopyN(out, in, info.Size()-metadata.CompilerArchiveSize)
	closeErr = out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func checkGoPackOutput(input, output string) error {
	if abs(input) == abs(output) {
		return fmt.Errorf("Go pack output must differ from input %q", input)
	}
	inputInfo, err := os.Stat(input)
	if err != nil {
		return err
	}
	outputInfo, err := os.Stat(output)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && os.SameFile(inputInfo, outputInfo) {
		return fmt.Errorf("Go pack output aliases input %q", input)
	}
	return nil
}

// Validate every member and the insertion boundary without loading object
// contents. Do not use header.size, which panics on malformed archive lengths.
func validateGoPackArchive(in io.ReaderAt, size, boundary int64) error {
	if boundary < int64(len(arHeader)) || boundary > size {
		return fmt.Errorf("compiler archive boundary %d is outside archive size %d", boundary, size)
	}
	var magic [len(arHeader)]byte
	if _, err := in.ReadAt(magic[:], 0); err != nil {
		return err
	}
	if string(magic[:]) != arHeader {
		return fmt.Errorf("invalid archive magic")
	}
	foundBoundary := boundary == int64(len(arHeader))
	for offset := int64(len(arHeader)); offset < size; {
		var hdr [entryLength]byte
		if _, err := in.ReadAt(hdr[:], offset); err != nil {
			return fmt.Errorf("read member header at %d: %w", offset, err)
		}
		if string(hdr[58:]) != "`\n" {
			return fmt.Errorf("invalid member header at %d", offset)
		}
		memberSize, err := strconv.ParseInt(strings.TrimSpace(string(hdr[48:58])), 10, 64)
		if err != nil || memberSize < 0 {
			return fmt.Errorf("invalid member size at %d", offset)
		}
		offset += entryLength
		remaining := size - offset
		if memberSize > remaining || memberSize%2 > remaining-memberSize {
			return fmt.Errorf("truncated member at %d", offset-entryLength)
		}
		offset += memberSize + memberSize%2
		foundBoundary = foundBoundary || offset == boundary
	}
	if !foundBoundary {
		return fmt.Errorf("compiler archive boundary %d is not a member boundary", boundary)
	}
	return nil
}
