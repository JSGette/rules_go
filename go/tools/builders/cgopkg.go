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

// Native classification is a separate output from the projected Go files.
// Its bytes do not change when a selected native source's body changes.
type cgoNativePlan struct {
	Sources    []cgoNativeSource `json:"sources"`
	Headers    []string          `json:"headers"`
	HaveCgo    bool              `json:"have_cgo"`
	HasImportC bool              `json:"has_import_c"`
}

type cgoNativeSource struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// Generated Go and C objects can be reused independently of original native
// source bodies. MainObject is for the cgo probe, not the package archive.
type cgoGenerated struct {
	GoFiles     []string `json:"go_files"`
	ObjectFiles []string `json:"object_files"`
	MainObject  string   `json:"main_object"`
	PackageName string   `json:"package_name"`
	LdFlags     []string `json:"ldflags"`
}

// Keep cgo2's order: a lexical listing puts _x10.o before _x2.o.
type cgoPrepared struct {
	GoFiles     []string `json:"go_files"`
	ObjectFiles []string `json:"object_files"`
}

// The Go compiler's inputs must not contain native source or object bytes.
// Sources contains Go assembly and its selected headers only when HaveCgo is
// false. All files share one projected root so relative includes still work.
type cgoCompileInputs struct {
	HaveCgo  bool               `json:"have_cgo"`
	Sources  []cgoProjectedFile `json:"sources"`
	Root     string             `json:"root,omitempty"`
	Execroot string             `json:"execroot,omitempty"`
}

type cgoPreparedGo struct {
	GoFiles []string `json:"go_files"`
}

func cgoproject(args []string) error {
	args, _, err := expandParamsFiles(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("GoCgoProject", flag.ContinueOnError)
	goenv := envFlags(fs)
	var sources, headers multiFlag
	var testFilter, outDir, nativeManifest, compileInputs string
	fs.Var(&sources, "src", "Go or native source file to classify")
	fs.Var(&headers, "header", "Additional header available to Go assembly")
	fs.StringVar(&testFilter, "testfilter", "off", "Controls test package filtering")
	fs.StringVar(&outDir, "out", "", "Directory for the selected cgo sources")
	fs.StringVar(&nativeManifest, "native_manifest", "", "File for selected native source metadata")
	fs.StringVar(&compileInputs, "compile_inputs", "", "Optional projection of Go assembly inputs and cgo classification")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := goenv.checkFlagsAndSetGoroot(); err != nil {
		return err
	}
	if outDir == "" || nativeManifest == "" {
		return fmt.Errorf("-out and -native_manifest are required")
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
	if err := writeCgoProjection(srcs.goSrcs, outDir, execroot); err != nil {
		return err
	}
	if compileInputs != "" {
		if err := writeCgoCompileInputs(srcs, sources, headers, compileInputs, execroot); err != nil {
			return err
		}
	}
	return writeCgoNativePlan(srcs, nativeManifest, execroot)
}

func archiveHasCgo(srcs archiveSrcs) bool {
	for _, src := range srcs.goSrcs {
		if src.isCgo {
			return true
		}
	}
	return len(srcs.cSrcs)+len(srcs.cxxSrcs)+len(srcs.objcSrcs)+len(srcs.objcxxSrcs) > 0
}

func writeCgoCompileInputs(srcs archiveSrcs, originalSources, headers []string, outDir, execroot string) error {
	if err := os.MkdirAll(outDir, 0777); err != nil {
		return err
	}
	inputs := cgoCompileInputs{HaveCgo: archiveHasCgo(srcs)}
	if inputs.HaveCgo || len(srcs.sSrcs) == 0 {
		return writeCgoManifest(outDir, inputs)
	}
	selected := append(append([]fileInfo{}, srcs.sSrcs...), srcs.hSrcs...)
	var files []string
	seen := make(map[string]bool)
	for _, src := range selected {
		files = append(files, src.filename)
	}
	// A source excluded from standalone assembly may still be #included by
	// another source. Preserve its bytes without adding it to Sources or to
	// the selected header directories passed to the assembler.
	for _, name := range originalSources {
		switch strings.ToLower(filepath.Ext(name)) {
		case ".s", ".h", ".hh", ".hpp", ".hxx":
			files = append(files, name)
		}
	}
	files = append(files, headers...)
	for i, name := range files {
		if !filepath.IsAbs(name) {
			name = filepath.Join(execroot, name)
		}
		files[i] = filepath.Clean(name)
	}
	// Start at the execroot, not the common source directory, so includes
	// resolved from the assembler's working directory retain their meaning.
	root := filepath.Clean(execroot)
	for _, name := range files {
		for {
			rel, err := filepath.Rel(root, name)
			if err != nil {
				return fmt.Errorf("project Go assembly input %q: %w", name, err)
			}
			if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				break
			}
			parent := filepath.Dir(root)
			if parent == root {
				return fmt.Errorf("no common root for Go assembly input %q", name)
			}
			root = parent
		}
	}
	rootRelative, err := filepath.Rel(execroot, root)
	if err != nil {
		return err
	}
	execrootRelative, err := filepath.Rel(root, execroot)
	if err != nil {
		return err
	}
	inputs.Root = filepath.ToSlash(rootRelative)
	inputs.Execroot = filepath.ToSlash(filepath.Join("src", execrootRelative))
	projectedExecroot := filepath.Join(outDir, filepath.FromSlash(inputs.Execroot))
	if err := os.MkdirAll(projectedExecroot, 0777); err != nil {
		return err
	}
	// The assembler runs here to preserve cwd-before-source-dir include lookup.
	// If all inputs belong to sibling repositories this directory is otherwise
	// empty; a marker keeps it present when a tree artifact is restored remotely.
	if err := os.WriteFile(filepath.Join(projectedExecroot, ".rules_go_cwd"), nil, 0666); err != nil {
		return err
	}
	for i, name := range files {
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		projected := filepath.Join("src", rel)
		if !seen[name] {
			out := filepath.Join(outDir, projected)
			if err := os.MkdirAll(filepath.Dir(out), 0777); err != nil {
				return err
			}
			if err := copyFile(name, out); err != nil {
				return err
			}
			seen[name] = true
		}
		if i < len(selected) {
			original, err := relativeCgoPath(execroot, name)
			if err != nil {
				return err
			}
			inputs.Sources = append(inputs.Sources, cgoProjectedFile{Path: filepath.ToSlash(projected), Original: original})
		}
	}
	return writeCgoManifest(outDir, inputs)
}

// readCgoCompileInputs returns already selected sources; filtering them again
// could incorrectly apply filename constraints to their projected directories.
// The first rewrite maps the projected execution root to the original working
// directory. Its prefix is also an assembler include directory.
func readCgoCompileInputs(dir string) (haveCgo bool, srcs archiveSrcs, rewrites []string, err error) {
	var inputs cgoCompileInputs
	if err := readCgoManifest(dir, &inputs); err != nil {
		return false, srcs, nil, err
	}
	if inputs.HaveCgo || len(inputs.Sources) == 0 {
		if len(inputs.Sources) != 0 {
			return false, srcs, nil, fmt.Errorf("cgo compile projection contains native assembly inputs")
		}
		return inputs.HaveCgo, srcs, nil, nil
	}
	if !validCgoProjectedPath(inputs.Execroot) || inputs.Root == "" {
		return false, srcs, nil, fmt.Errorf("invalid Go assembly projection root")
	}
	root := filepath.FromSlash(inputs.Root)
	if root != "." {
		for _, part := range strings.Split(inputs.Root, "/") {
			if part != ".." {
				return false, srcs, nil, fmt.Errorf("invalid Go assembly original root %q", inputs.Root)
			}
		}
	}
	rewrites = append(rewrites, filepath.Join(dir, filepath.FromSlash(inputs.Execroot))+"=>")
	if root != "." {
		rewrites = append(rewrites, filepath.Join(dir, "src")+"=>"+root)
	}
	for _, src := range inputs.Sources {
		if !validCgoProjectedPath(src.Path) {
			return false, srcs, nil, fmt.Errorf("invalid projected Go assembly path %q", src.Path)
		}
		original := filepath.FromSlash(src.Original)
		if src.Original == "" || filepath.IsAbs(original) || filepath.VolumeName(original) != "" || strings.HasPrefix(original, string(filepath.Separator)) {
			return false, srcs, nil, fmt.Errorf("invalid original Go assembly path %q", src.Original)
		}
		info := fileInfo{
			filename:         filepath.Join(dir, filepath.FromSlash(src.Path)),
			originalFilename: original,
			matched:          true,
		}
		switch strings.ToLower(filepath.Ext(src.Path)) {
		case ".s":
			info.ext = sExt
			srcs.sSrcs = append(srcs.sSrcs, info)
		case ".h", ".hh", ".hpp", ".hxx":
			info.ext = hExt
			srcs.hSrcs = append(srcs.hSrcs, info)
		default:
			return false, srcs, nil, fmt.Errorf("unexpected Go assembly input %q", src.Path)
		}
	}
	return false, srcs, rewrites, nil
}

func validCgoProjectedPath(name string) bool {
	path := filepath.FromSlash(name)
	clean := filepath.Clean(path)
	return name != "" && !strings.Contains(name, "\\") && !strings.HasPrefix(path, string(filepath.Separator)) && !filepath.IsAbs(path) && filepath.VolumeName(path) == "" && clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func writeCgoNativePlan(srcs archiveSrcs, outPath, execroot string) error {
	plan := cgoNativePlan{}
	for _, src := range srcs.goSrcs {
		plan.HasImportC = plan.HasImportC || src.isCgo
	}
	plan.HaveCgo = plan.HasImportC || len(srcs.cSrcs)+len(srcs.cxxSrcs)+len(srcs.objcSrcs)+len(srcs.objcxxSrcs) > 0
	// This is the same language order used by cgo2 when packing objects.
	for _, lang := range []struct {
		kind string
		srcs []fileInfo
	}{{"c", srcs.cSrcs}, {"cxx", srcs.cxxSrcs}, {"objc", srcs.objcSrcs}, {"objcxx", srcs.objcxxSrcs}, {"asm", srcs.sSrcs}} {
		for _, src := range lang.srcs {
			name, err := relativeCgoPath(execroot, src.filename)
			if err != nil {
				return err
			}
			plan.Sources = append(plan.Sources, cgoNativeSource{Path: name, Kind: lang.kind})
		}
	}
	for _, hdr := range srcs.hSrcs {
		name, err := relativeCgoPath(execroot, hdr.filename)
		if err != nil {
			return err
		}
		plan.Headers = append(plan.Headers, name)
	}
	return writeCgoJSON(outPath, plan)
}

func relativeCgoPath(execroot, name string) (string, error) {
	if !filepath.IsAbs(name) {
		name = filepath.Join(execroot, name)
	}
	relative, err := filepath.Rel(execroot, name)
	if err != nil {
		return "", fmt.Errorf("relative cgo source path: %w", err)
	}
	return filepath.ToSlash(relative), nil
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

func cgogenerate(args []string) error {
	args, _, err := expandParamsFiles(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("GoCgoGenerate", flag.ContinueOnError)
	goenv := envFlags(fs)
	var projectionDir, nativeManifest, outDir, exportHeader, installHeader, packagePath string
	var cppFlags, cFlags, ldFlags quoteMultiFlag
	fs.StringVar(&projectionDir, "projection", "", "Directory of selected cgo sources")
	fs.StringVar(&nativeManifest, "native_manifest", "", "Selected native source metadata")
	fs.StringVar(&outDir, "out", "", "Directory for generated Go sources and native objects")
	fs.StringVar(&exportHeader, "cgoexport", "", "The exported C header to write")
	fs.StringVar(&installHeader, "cgo_install_header", "", "Optional installed copy of the exported C header")
	fs.StringVar(&packagePath, "p", "", "Package import map")
	fs.Var(&cppFlags, "cppflags", "C preprocessor flags")
	fs.Var(&cFlags, "cflags", "C compiler flags")
	fs.Var(&ldFlags, "ldflags", "C linker flags")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := goenv.checkFlagsAndSetGoroot(); err != nil {
		return err
	}
	if projectionDir == "" || nativeManifest == "" || outDir == "" || exportHeader == "" {
		return fmt.Errorf("-projection, -native_manifest, -out, and -cgoexport are required")
	}
	projectionDir, outDir, exportHeader = abs(projectionDir), abs(outDir), abs(exportHeader)
	var projection cgoProjection
	if err := readCgoManifest(projectionDir, &projection); err != nil {
		return err
	}
	var plan cgoNativePlan
	if err := readCgoJSON(nativeManifest, &plan); err != nil {
		return err
	}
	if plan.HasImportC != (len(projection.Sources) > 0) {
		return fmt.Errorf("cgo source projection and native metadata disagree")
	}
	if err := os.MkdirAll(outDir, 0777); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(exportHeader), 0777); err != nil {
		return err
	}
	generated := cgoGenerated{}
	if plan.HasImportC {
		cc := os.Getenv("CC")
		if cc == "" {
			return fmt.Errorf("CC is not set for cgo generation")
		}
		cgoSrcs, rewrites, err := projection.inputs(projectionDir)
		if err != nil {
			return err
		}
		workDir, cleanup, err := goenv.workDir()
		if err != nil {
			return err
		}
		defer cleanup()
		var headers []string
		for _, hdr := range plan.Headers {
			headers = append(headers, abs(filepath.FromSlash(hdr)))
		}
		gen, err := generateCgoSources(goenv, workDir, "cgo", cgoSrcs,
			plan.sourcesOfKind("c"), plan.sourcesOfKind("cxx"), plan.sourcesOfKind("objc"), plan.sourcesOfKind("objcxx"),
			headers, packagePath, cppFlags, cFlags, ldFlags, exportHeader, rewrites...)
		if err != nil {
			return err
		}
		objects, mainObj, err := compileGeneratedCgo(goenv, gen, cc, cppFlags, cFlags)
		if err != nil {
			return err
		}
		if err := copyGeneratedGoSrcs(gen.genGoSrcs, outDir); err != nil {
			return err
		}
		for _, src := range gen.genGoSrcs {
			generated.GoFiles = append(generated.GoFiles, filepath.Base(src))
		}
		for _, object := range append(objects, mainObj) {
			if err := copyFile(object, filepath.Join(outDir, filepath.Base(object))); err != nil {
				return err
			}
		}
		for _, object := range objects {
			generated.ObjectFiles = append(generated.ObjectFiles, filepath.Base(object))
		}
		generated.MainObject = filepath.Base(mainObj)
		generated.PackageName = projection.PackageName
		generated.LdFlags = gen.combinedLdFlags
	} else if err := os.WriteFile(exportHeader, nil, 0666); err != nil {
		return err
	}
	if installHeader != "" {
		if err := copyFile(exportHeader, installHeader); err != nil {
			return err
		}
	}
	return writeCgoManifest(outDir, generated)
}

func (p cgoNativePlan) sourcesOfKind(kind string) []string {
	var paths []string
	for _, src := range p.Sources {
		if src.Kind == kind {
			paths = append(paths, src.Path)
		}
	}
	return paths
}

func cgocompile(args []string) error {
	args, _, err := expandParamsFiles(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("GoCgoCompile", flag.ContinueOnError)
	goenv := envFlags(fs)
	var nativeManifest, source, exportHeader, outObject string
	var cppFlags, cFlags quoteMultiFlag
	fs.StringVar(&nativeManifest, "native_manifest", "", "Selected native source metadata")
	fs.StringVar(&source, "src", "", "Original native source file to compile")
	fs.StringVar(&exportHeader, "cgoexport", "", "The generated C export header")
	fs.StringVar(&outObject, "o", "", "Native object file to write")
	fs.Var(&cppFlags, "cppflags", "C preprocessor flags")
	fs.Var(&cFlags, "cflags", "Compiler flags for this source's language")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := goenv.checkFlagsAndSetGoroot(); err != nil {
		return err
	}
	if nativeManifest == "" || source == "" || exportHeader == "" || outObject == "" {
		return fmt.Errorf("-native_manifest, -src, -cgoexport, and -o are required")
	}
	var plan cgoNativePlan
	if err := readCgoJSON(nativeManifest, &plan); err != nil {
		return err
	}
	execroot, err := os.Getwd()
	if err != nil {
		return err
	}
	sourceName, err := relativeCgoPath(execroot, source)
	if err != nil {
		return err
	}
	var kind string
	for _, candidate := range plan.Sources {
		if candidate.Path == sourceName {
			kind = candidate.Kind
			break
		}
	}
	if kind == "" || !plan.HaveCgo || os.Getenv("CGO_ENABLED") != "1" {
		// Analysis must declare an output even when build tags exclude this
		// source, or assembly belongs to the Go assembler instead of cgo.
		return os.WriteFile(outObject, nil, 0666)
	}
	cc := os.Getenv("CC")
	if cc == "" {
		return cgoError{source}
	}
	headerDir := filepath.Dir(abs(exportHeader))
	var includes []string
	if kind != "asm" {
		seen := make(map[string]bool)
		for _, hdr := range plan.Headers {
			dir := filepath.Dir(abs(filepath.FromSlash(hdr)))
			if !seen[dir] {
				seen[dir] = true
				includes = append(includes, "-iquote", dir)
			}
		}
		if plan.HasImportC {
			includes = append(includes, "-iquote", headerDir)
		}
	}
	// The specific mapping follows the general execroot mapping, matching
	// cgo2's treatment of _cgo_export.h when it lived in a temporary directory.
	flags := combineFlags(cppFlags, includes, cFlags, defaultCFlags(headerDir))
	return cCompile(goenv, abs(source), cc, flags, abs(outObject))
}

func cgoprepare(args []string) error {
	args, _, err := expandParamsFiles(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("GoCgoPrepare", flag.ContinueOnError)
	goenv := envFlags(fs)
	var generatedDir, nativeManifest, goDir, objectDir, goManifest string
	var nativeSources, nativeObjects multiFlag
	fs.StringVar(&generatedDir, "generated", "", "Generated Go sources, native objects, and link metadata")
	fs.StringVar(&nativeManifest, "native_manifest", "", "Selected native source metadata")
	fs.Var(&nativeSources, "native_src", "Original source path identifying a native object")
	fs.Var(&nativeObjects, "native_object", "Compiled object matching the corresponding -native_src")
	fs.StringVar(&goDir, "cgo_go_srcs", "", "Directory for all generated Go sources")
	fs.StringVar(&objectDir, "cgo_objects", "", "Directory for native objects and their manifest")
	fs.StringVar(&goManifest, "go_manifest", "", "Optional Go-source-only manifest, outside the generated Go tree")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := goenv.checkFlagsAndSetGoroot(); err != nil {
		return err
	}
	if generatedDir == "" || nativeManifest == "" || goDir == "" || objectDir == "" {
		return fmt.Errorf("-generated, -native_manifest, -cgo_go_srcs, and -cgo_objects are required")
	}
	if len(nativeSources) != len(nativeObjects) {
		return fmt.Errorf("-native_src and -native_object must be paired")
	}
	generatedDir, goDir, objectDir = abs(generatedDir), abs(goDir), abs(objectDir)
	var generated cgoGenerated
	if err := readCgoManifest(generatedDir, &generated); err != nil {
		return err
	}
	var plan cgoNativePlan
	if err := readCgoJSON(nativeManifest, &plan); err != nil {
		return err
	}
	if plan.HasImportC != (generated.MainObject != "") {
		return fmt.Errorf("generated cgo objects and native metadata disagree")
	}
	execroot, err := os.Getwd()
	if err != nil {
		return err
	}
	objectBySource := make(map[string]string, len(nativeSources))
	for i, src := range nativeSources {
		name, err := relativeCgoPath(execroot, src)
		if err != nil {
			return err
		}
		if _, ok := objectBySource[name]; ok {
			return fmt.Errorf("multiple native objects for source %q", src)
		}
		objectBySource[name] = abs(nativeObjects[i])
	}
	for _, dir := range []string{goDir, objectDir} {
		if err := os.MkdirAll(dir, 0777); err != nil {
			return err
		}
	}
	prepared := cgoPrepared{GoFiles: generated.GoFiles}
	for _, name := range generated.GoFiles {
		if err := copyFile(filepath.Join(generatedDir, name), filepath.Join(goDir, name)); err != nil {
			return err
		}
	}
	var objects []string
	for _, name := range generated.ObjectFiles {
		objects = append(objects, filepath.Join(generatedDir, name))
	}
	if plan.HaveCgo {
		for _, src := range plan.Sources {
			obj, ok := objectBySource[src.Path]
			if !ok {
				return fmt.Errorf("no native object provided for selected source %q", src.Path)
			}
			objects = append(objects, obj)
		}
	}
	var archiveObjects []string
	for _, obj := range objects {
		name := fmt.Sprintf("_x%d.o", len(archiveObjects))
		out := filepath.Join(objectDir, name)
		if err := copyFile(obj, out); err != nil {
			return err
		}
		archiveObjects = append(archiveObjects, out)
		prepared.ObjectFiles = append(prepared.ObjectFiles, name)
	}
	if generated.MainObject != "" {
		cc := os.Getenv("CC")
		if cc == "" {
			return fmt.Errorf("CC is not set for the cgo probe link")
		}
		workDir, cleanup, err := goenv.workDir()
		if err != nil {
			return err
		}
		defer cleanup()
		imports := filepath.Join(goDir, "_cgo_imports.go")
		if err := linkCgo(goenv, cc, generated.PackageName, filepath.Join(generatedDir, generated.MainObject),
			filepath.Join(workDir, "_cgo_.o"), imports, archiveObjects, generated.LdFlags); err != nil {
			return err
		}
		prepared.GoFiles = append(prepared.GoFiles, filepath.Base(imports))
	}
	if goManifest != "" {
		if err := writeCgoJSON(goManifest, cgoPreparedGo{GoFiles: prepared.GoFiles}); err != nil {
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

func readCgoPreparedGo(goDir, manifestPath string) ([]string, error) {
	var prepared cgoPreparedGo
	if err := readCgoJSON(manifestPath, &prepared); err != nil {
		return nil, err
	}
	var sources []string
	for _, name := range prepared.GoFiles {
		if !validCgoProjectedPath(name) || filepath.Base(name) != name || filepath.Ext(name) != ".go" {
			return nil, fmt.Errorf("invalid prepared cgo Go source name %q", name)
		}
		sources = append(sources, filepath.Join(goDir, name))
	}
	return sources, nil
}

func writeCgoManifest(dir string, manifest interface{}) error {
	return writeCgoJSON(filepath.Join(dir, cgoManifestName), manifest)
}

func writeCgoJSON(path string, manifest interface{}) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0666)
}

func readCgoManifest(dir string, manifest interface{}) error {
	return readCgoJSON(filepath.Join(dir, cgoManifestName), manifest)
}

func readCgoJSON(path string, manifest interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, manifest)
}
