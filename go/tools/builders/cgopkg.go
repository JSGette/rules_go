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
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const cgoManifestName = "manifest.json"

// A projection contains only selected import-C files. Ordinary Go source
// contents and their digests must not be part of its action output.
type cgoProjection struct {
	PackageName string             `json:"package_name"`
	Sources     []cgoProjectedFile `json:"sources"`
}

type cgoProjectedFile struct {
	Path     string `json:"path"`
	Original string `json:"original"`
}

// Keep cgo2's order: a lexical listing puts _x10.o before _x2.o.
type cgoPrepared struct {
	GoFiles     []string `json:"go_files"`
	ObjectFiles []string `json:"object_files"`
}

func cgoproject(args []string) error {
	args, _, err := expandParamsFiles(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("GoCgoProject", flag.ContinueOnError)
	goenv := envFlags(fs)
	var sources multiFlag
	var testFilter, outDir string
	fs.Var(&sources, "src", "Go source file to classify")
	fs.StringVar(&testFilter, "testfilter", "off", "Controls test package filtering")
	fs.StringVar(&outDir, "out", "", "Directory for the selected cgo sources")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := goenv.checkFlagsAndSetGoroot(); err != nil {
		return err
	}
	if outDir == "" {
		return fmt.Errorf("-out is required")
	}
	for i := range sources {
		sources[i] = abs(sources[i])
	}
	srcs, err := filterAndSplitFiles(sources)
	if err != nil {
		return err
	}
	if err := applyTestFilter(testFilter, &srcs); err != nil {
		return err
	}
	execroot, err := os.Getwd()
	if err != nil {
		return err
	}
	return writeCgoProjection(srcs.goSrcs, outDir, execroot)
}

func writeCgoProjection(srcs []fileInfo, outDir, execroot string) error {
	if err := os.MkdirAll(outDir, 0777); err != nil {
		return err
	}
	projection := cgoProjection{}
	dirs := make(map[string]string)
	for _, src := range srcs {
		if !src.isCgo {
			continue
		}
		if len(projection.Sources) != 0 && projection.PackageName != src.pkg {
			return fmt.Errorf("cgo sources have different package names: %s and %s", projection.PackageName, src.pkg)
		}
		original, err := filepath.Rel(execroot, src.filename)
		if err != nil {
			return fmt.Errorf("relative cgo source path: %w", err)
		}
		originalDir := filepath.Dir(original)
		projectedDir, ok := dirs[originalDir]
		if !ok {
			// Sibling external repositories can have paths starting with ../.
			// Assign directories instead of using those paths as output names.
			projectedDir = filepath.Join("src", strconv.Itoa(len(dirs)))
			dirs[originalDir] = projectedDir
			if err := os.MkdirAll(filepath.Join(outDir, projectedDir), 0777); err != nil {
				return err
			}
		}
		projected := filepath.Join(projectedDir, filepath.Base(src.filename))
		// The native action must read these without the original Go inputs.
		// Symlinks, as used by gatherSrcs, are not suitable here.
		if err := copyFile(src.filename, filepath.Join(outDir, projected)); err != nil {
			return err
		}
		if len(projection.Sources) == 0 {
			projection.PackageName = src.pkg
		}
		projection.Sources = append(projection.Sources, cgoProjectedFile{
			Path: filepath.ToSlash(projected), Original: filepath.ToSlash(original),
		})
	}
	return writeCgoManifest(outDir, projection)
}

func cgoprepare(args []string) error {
	args, _, err := expandParamsFiles(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("GoCgoPrepare", flag.ContinueOnError)
	goenv := envFlags(fs)
	var sources multiFlag
	var projectionDir, goDir, objectDir, exportHeader, importPath, packagePath, testFilter string
	var cppFlags, cFlags, cxxFlags, objcFlags, objcxxFlags, ldFlags quoteMultiFlag
	fs.Var(&sources, "src", "Native source file to filter and compile")
	fs.StringVar(&projectionDir, "projection", "", "Directory of selected cgo sources")
	fs.StringVar(&goDir, "cgo_go_srcs", "", "Directory for generated Go sources")
	fs.StringVar(&objectDir, "cgo_objects", "", "Directory for native objects and their manifest")
	fs.StringVar(&exportHeader, "cgoexport", "", "The exported C header to write")
	fs.StringVar(&importPath, "importpath", "", "Package import path")
	fs.StringVar(&packagePath, "p", "", "Package import map")
	fs.StringVar(&testFilter, "testfilter", "off", "Controls test package filtering")
	fs.Var(&cppFlags, "cppflags", "C preprocessor flags")
	fs.Var(&cFlags, "cflags", "C compiler flags")
	fs.Var(&cxxFlags, "cxxflags", "C++ compiler flags")
	fs.Var(&objcFlags, "objcflags", "Objective-C compiler flags")
	fs.Var(&objcxxFlags, "objcxxflags", "Objective-C++ compiler flags")
	fs.Var(&ldFlags, "ldflags", "C linker flags")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := goenv.checkFlagsAndSetGoroot(); err != nil {
		return err
	}
	if projectionDir == "" || goDir == "" || objectDir == "" {
		return fmt.Errorf("-projection, -cgo_go_srcs, and -cgo_objects are required")
	}
	projectionDir, goDir, objectDir = abs(projectionDir), abs(goDir), abs(objectDir)
	var projection cgoProjection
	if err := readCgoManifest(projectionDir, &projection); err != nil {
		return err
	}
	cgoSrcs, rewrites, err := projection.inputs(projectionDir)
	if err != nil {
		return err
	}
	for i := range sources {
		sources[i] = abs(sources[i])
	}
	srcs, err := filterAndSplitFiles(sources)
	if err != nil {
		return err
	}
	if len(srcs.goSrcs) != 0 {
		return fmt.Errorf("cgoprepare Go sources must come from -projection")
	}
	if err := applyTestFilter(testFilter, &srcs); err != nil {
		return err
	}
	for _, dir := range []string{goDir, objectDir} {
		if err := os.MkdirAll(dir, 0777); err != nil {
			return err
		}
	}
	// Own the temporary files returned by cgo2. Its nested workDir call then
	// has a no-op cleanup, allowing us to copy the outputs before deletion.
	_, cleanup, err := goenv.workDir()
	if err != nil {
		return err
	}
	defer cleanup()

	prepared := cgoPrepared{}
	haveCgo := len(cgoSrcs)+len(srcs.cSrcs)+len(srcs.cxxSrcs)+len(srcs.objcSrcs)+len(srcs.objcxxSrcs) > 0
	if haveCgo && os.Getenv("CGO_ENABLED") == "1" {
		_, generatedGo, objects, err := cgo2(goenv, nil, cgoSrcs,
			cgoFileNames(srcs.cSrcs), cgoFileNames(srcs.cxxSrcs),
			cgoFileNames(srcs.objcSrcs), cgoFileNames(srcs.objcxxSrcs),
			cgoFileNames(srcs.sSrcs), cgoFileNames(srcs.hSrcs),
			packagePath, projection.PackageName, os.Getenv("CC"),
			cppFlags, cFlags, cxxFlags, objcFlags, objcxxFlags, ldFlags,
			exportHeader, goDir, rewrites...)
		if err != nil {
			return err
		}
		for _, src := range generatedGo {
			prepared.GoFiles = append(prepared.GoFiles, filepath.Base(src))
		}
		for _, obj := range objects {
			name := filepath.Base(obj)
			if err := copyFile(obj, filepath.Join(objectDir, name)); err != nil {
				return err
			}
			prepared.ObjectFiles = append(prepared.ObjectFiles, name)
		}
	}
	// Native-only packages, or packages whose cgo files are excluded by build
	// constraints, still have to materialize every declared output.
	if exportHeader != "" && len(prepared.GoFiles) == 0 {
		if err := os.WriteFile(exportHeader, nil, 0666); err != nil {
			return err
		}
	}
	return writeCgoManifest(objectDir, prepared)
}

func (p cgoProjection) inputs(dir string) (sources, rewrites []string, err error) {
	seenDirs := make(map[string]bool)
	for _, src := range p.Sources {
		name := filepath.FromSlash(src.Path)
		if filepath.IsAbs(name) || name == "" || filepath.Clean(name) == "." || name == ".." || strings.HasPrefix(filepath.Clean(name), ".."+string(filepath.Separator)) {
			return nil, nil, fmt.Errorf("invalid projected cgo path %q", src.Path)
		}
		sources = append(sources, filepath.Join(dir, name))
		projectedDir := filepath.Join(dir, filepath.Dir(name))
		if !seenDirs[projectedDir] {
			seenDirs[projectedDir] = true
			originalDir := filepath.Dir(filepath.FromSlash(src.Original))
			if originalDir == "." {
				originalDir = ""
			}
			// Rewrite before cgo resolves //line directives and emits C #line.
			rewrites = append(rewrites, projectedDir+"=>"+originalDir)
		}
	}
	return sources, rewrites, nil
}

func readCgoPrepared(goDir, objectDir string) (goSrcs, objects []string, err error) {
	var prepared cgoPrepared
	if err := readCgoManifest(objectDir, &prepared); err != nil {
		return nil, nil, err
	}
	for _, output := range []struct {
		dir   string
		names []string
		paths *[]string
	}{{goDir, prepared.GoFiles, &goSrcs}, {objectDir, prepared.ObjectFiles, &objects}} {
		for _, name := range output.names {
			if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
				return nil, nil, fmt.Errorf("invalid prepared cgo output name %q", name)
			}
			*output.paths = append(*output.paths, filepath.Join(output.dir, name))
		}
	}
	return goSrcs, objects, nil
}

func cgoFileNames(srcs []fileInfo) []string {
	var names []string
	for _, src := range srcs {
		names = append(names, src.filename)
	}
	return names
}

func writeCgoManifest(dir string, manifest interface{}) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, cgoManifestName), append(data, '\n'), 0666)
}

func readCgoManifest(dir string, manifest interface{}) error {
	data, err := os.ReadFile(filepath.Join(dir, cgoManifestName))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, manifest)
}
