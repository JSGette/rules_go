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
	"bufio"
	"bytes"
	"debug/elf"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

const (
	prelinkCaptureEnv     = "GO_BAZEL_PRELINK_CAPTURE"
	prelinkHandoffExit    = 77
	prelinkHandoffMessage = "GO_BAZEL_PRELINK_HANDOFF_COMPLETE"
	prelinkOutputName     = "not-linked"
	prelinkReceiptName    = "handoff.json"
	prelinkPlanName       = "plan.json"
	prelinkTokenPrefix    = "__GO_BAZEL_PRELINK_"
	prelinkTreeToken      = prelinkTokenPrefix + "TREE__"
	prelinkRootToken      = prelinkTokenPrefix + "ROOT__"
	prelinkParentToken    = prelinkTokenPrefix + "PARENT__"
	prelinkOutputToken    = prelinkTokenPrefix + "OUTPUT__"
)

var prelinkHostObject = regexp.MustCompile(`^[0-9]{6,}\.o$`)

// This deliberately versioned recipe is private to the experimental action.
// Only argv and paths are recorded: native library and sidecar bytes belong to
// the host-link action. Tokens are expanded in its execroot, not the producer's.
type prelinkPlan struct {
	Version           int      `json:"version"`
	SDK               string   `json:"sdk"`
	Compiler          string   `json:"compiler"`
	Args              []string `json:"args"`
	NativeInsertIndex int      `json:"native_insert_index"`
	NativeObjects     []string `json:"native_objects"`
	ContentBuildID    bool     `json:"content_build_id"`
}

type prelinkReceipt struct {
	Compiler string   `json:"compiler"`
	Args     []string `json:"args"`
}

// Keep this explicit list in sync with the action eligibility in link.bzl.
// Go 1.26.10 has the same Linux external-link protocol and FIPS finalizer as
// 1.26.7; its only hostlink change is a Darwin linker workaround.
func supportedPrelinkSDK(version string) bool {
	return version == "go1.26.7" || version == "go1.26.10"
}

func validatePrelinkSDKVersions(builderVersion, sdkVersion string) error {
	if !supportedPrelinkSDK(builderVersion) || sdkVersion != builderVersion {
		return fmt.Errorf("split linking requires matching Go 1.26.7 or 1.26.10 builder, linker, and recipe versions; builder is %q, input is %q", builderVersion, sdkVersion)
	}
	return nil
}

func checkPrelinkSDK(e *env) error {
	if !supportedPrelinkSDK(runtime.Version()) || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" || os.Getenv("GOOS") != "linux" || os.Getenv("GOARCH") != "amd64" {
		return fmt.Errorf("split linking requires a Go 1.26.7 or 1.26.10 builder and a native Linux amd64 target")
	}
	versionArgs := e.goTool("link", "-V")
	version, err := exec.Command(versionArgs[0], versionArgs[1:]...).Output()
	if err != nil {
		return err
	}
	linkerVersion, ok := strings.CutPrefix(strings.TrimSpace(string(version)), "link version ")
	if !ok {
		return fmt.Errorf("unexpected linker version response %q", version)
	}
	return validatePrelinkSDKVersions(runtime.Version(), linkerVersion)
}

// Reject user overrides of the handoff and unsupported executable formats.
// In particular absCCLinker wraps only the first separate -extld argument.
func validatePrelinkToolArgs(args []string) error {
	extldCount := 0
	valueFlags := map[string]bool{
		"extld": true, "extldflags": true, "extar": true, "linkmode": true,
		"buildmode": true, "buildid": true, "X": true, "B": true, "I": true,
		"L": true, "R": true, "T": true, "D": true, "E": true, "r": true,
		"k": true, "libgcc": true, "installsuffix": true, "strictdups": true,
		"debugtramp": true, "debugtextsize": true, "funcalign": true, "randlayout": true,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			return fmt.Errorf("unsupported split-link argument %q", arg)
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if valueFlags[name] && !hasValue {
			if i+1 == len(args) {
				return fmt.Errorf("missing value for %s", arg)
			}
			i++
			value = args[i]
		}
		switch name {
		case "extld":
			extldCount++
			if hasValue || value == "" || strings.ContainsAny(value, "\t\r\n ") {
				return fmt.Errorf("split linking requires one separate -extld executable argument")
			}
		case "linkmode":
			if value != "external" {
				return fmt.Errorf("split linking requires -linkmode=external")
			}
		case "buildmode":
			if value != "exe" {
				return fmt.Errorf("split linking requires -buildmode=exe")
			}
		case "o", "tmpdir", "importcfg", "H", "fipso", "linkshared", "pluginpath", "race", "msan", "asan", "capturehostobjs", "cpuprofile", "memprofile", "benchmarkprofile", "V":
			return fmt.Errorf("%s is unsupported with split linking", arg)
		}
	}
	if extldCount != 1 {
		return fmt.Errorf("split linking requires exactly one -extld argument")
	}
	return nil
}

// Traverse import manifests in a deterministic breadth-first order, with the
// main package first. A declared but unimported dependency contributes no raw
// objects (and therefore no constructors or conflicting native definitions).
// This function never opens the native sidecars.
func reachableNativeObjects(mainPackage, mainImports string, importsEntries, nativeEntries []string) ([]string, []string, error) {
	if mainPackage == "" || mainImports == "" {
		return nil, nil, fmt.Errorf("split linking requires the main package path and imports manifest")
	}
	imports, err := parseKeyedPaths(importsEntries)
	if err != nil {
		return nil, nil, err
	}
	natives, err := parseKeyedPaths(nativeEntries)
	if err != nil {
		return nil, nil, err
	}
	imports[mainPackage] = mainImports
	queue := []string{mainPackage}
	seen := make(map[string]bool)
	var selected, used []string
	for len(queue) > 0 {
		pkg := queue[0]
		queue = queue[1:]
		if seen[pkg] {
			continue
		}
		seen[pkg] = true
		if path := natives[pkg]; path != "" {
			selected = append(selected, path)
		}
		if path := imports[pkg]; path != "" {
			deps, err := readImportsManifest(path)
			if err != nil {
				return nil, nil, err
			}
			used = append(used, path)
			queue = append(queue, deps...)
		}
	}
	return selected, used, nil
}

// The supported SDKs write long external-link argv using one %q string per line.
// Only its own response file is expanded; arbitrary user response files are
// outside this handoff's supported, declared-input contract.
func expandPrelinkResponse(args []string, tree string) ([]string, error) {
	var expanded []string
	for _, arg := range args {
		if !strings.HasPrefix(arg, "@") {
			expanded = append(expanded, arg)
			continue
		}
		if abs(arg[1:]) != filepath.Join(tree, "response") {
			return nil, fmt.Errorf("split linking does not support external response file %q", arg)
		}
		data, err := os.ReadFile(arg[1:])
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 4096), 16*1024*1024)
		for scanner.Scan() {
			value, err := strconv.Unquote(scanner.Text())
			if err != nil {
				return nil, fmt.Errorf("invalid Go linker response file: %w", err)
			}
			if strings.HasPrefix(value, "@") {
				return nil, fmt.Errorf("nested external response file is unsupported")
			}
			expanded = append(expanded, value)
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
	}
	return expanded, nil
}

// capturePrelinkCC runs inside builder-cc before placeholder normalization.
// Capability probes still execute the real compiler. Only the unique final
// command containing this action's go.o produces a receipt and exits 77.
func capturePrelinkCC(tree, compiler string, args []string) (bool, error) {
	expanded, err := expandPrelinkResponse(args, tree)
	if err != nil {
		return false, err
	}
	goObject := filepath.Join(tree, "go.o")
	count := 0
	for _, arg := range expanded {
		if arg == goObject {
			count++
		}
	}
	if count == 0 {
		return false, nil
	}
	if count != 1 {
		return false, fmt.Errorf("ambiguous external-link handoff")
	}
	if _, err := prelinkObjectInsertion(expanded, tree); err != nil {
		return false, err
	}
	if err := validatePrelinkCCArgs(expanded); err != nil {
		return false, err
	}
	if err := validatePrelinkObject(goObject); err != nil {
		return false, err
	}
	data, err := json.Marshal(prelinkReceipt{Compiler: compiler, Args: expanded})
	if err != nil {
		return false, err
	}
	f, err := os.OpenFile(filepath.Join(tree, prelinkReceiptName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0666)
	if err != nil {
		return false, fmt.Errorf("creating unique prelink receipt: %w", err)
	}
	_, writeErr := f.Write(append(data, '\n'))
	closeErr := f.Close()
	if writeErr != nil {
		return false, writeErr
	}
	if closeErr != nil {
		return false, closeErr
	}
	fmt.Fprintln(os.Stderr, prelinkHandoffMessage)
	return true, nil
}

func validatePrelinkObject(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("reading prelink Go object: %w", err)
	}
	defer f.Close()
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB || f.Machine != elf.EM_X86_64 || f.Type != elf.ET_REL {
		return fmt.Errorf("prelink Go object must be a little-endian ELF64 amd64 relocatable object")
	}
	return nil
}

// Keep the stock go.o/host-object prefix intact (notably runtime/cgo's
// constructors), then place sidecar objects before any cgo libraries.
func prelinkObjectInsertion(args []string, tree string) (int, error) {
	goIndex, outputCount := -1, 0
	for i, arg := range args {
		if arg == filepath.Join(tree, "go.o") {
			if goIndex >= 0 {
				return 0, fmt.Errorf("multiple Go objects in handoff")
			}
			goIndex = i
		}
		if arg == "-o" {
			outputCount++
			if i+1 == len(args) || args[i+1] != filepath.Join(tree, prelinkOutputName) {
				return 0, fmt.Errorf("unexpected handoff output")
			}
		}
	}
	if goIndex < 0 || outputCount != 1 {
		return 0, fmt.Errorf("incomplete external-link handoff")
	}
	i := goIndex + 1
	for i < len(args) && filepath.Dir(args[i]) == tree && prelinkHostObject.MatchString(filepath.Base(args[i])) {
		i++
	}
	return i, nil
}

// cmd/link's capability probes discard ordinary -l/-L and bare native inputs,
// but retain -Wl and -f/-m/-p options. Allow only understood options in those
// retained families: a missing linker script or plugin must not silently change
// probe results in an action that deliberately has no native library inputs.
func validatePrelinkCCArgs(args []string) error {
	for _, arg := range args {
		if strings.Contains(arg, prelinkTokenPrefix) {
			return fmt.Errorf("reserved prelink token in argument %q", arg)
		}
		if strings.HasPrefix(arg, "-isysroot") || strings.HasPrefix(arg, "--sysroot") {
			return fmt.Errorf("explicit sysroot option %q is unsupported with split linking: compiler probe inputs must be declared independently of native libraries", arg)
		}
		if arg == "-Xlinker" || arg == "-static" || arg == "-static-pie" || arg == "-pie" || arg == "-shared" || arg == "-r" || arg == "-c" || arg == "-S" || arg == "-E" || strings.HasPrefix(arg, "@") || strings.HasPrefix(arg, "--output") || (strings.HasPrefix(arg, "-o") && arg != "-o") {
			return fmt.Errorf("external option %q is unsupported with split linking", arg)
		}
		if strings.HasPrefix(arg, "-Wl,") {
			parts := strings.Split(strings.TrimPrefix(arg, "-Wl,"), ",")
			for i := 0; i < len(parts); i++ {
				part := parts[i]
				switch part {
				case "-rpath", "--rpath", "--dynamic-linker", "-dynamic-linker":
					// These are output strings, not input files. A separate
					// argument is also accepted by the compiler driver.
					if i+1 < len(parts) {
						i++
					}
				case "-z":
					if i+1 == len(parts) {
						return fmt.Errorf("split linking requires -Wl,-z,VALUE in one argument")
					}
					i++
					switch parts[i] {
					case "relro", "norelro", "now", "lazy", "noexecstack", "execstack", "defs", "nodefs", "origin", "nocopyreloc", "separate-code", "noseparate-code":
					default:
						return fmt.Errorf("unsupported split-link -z option %q", parts[i])
					}
				case "--as-needed", "-as-needed", "--no-as-needed", "-no-as-needed", "--gc-sections", "--no-gc-sections", "--start-group", "--end-group", "--whole-archive", "--no-whole-archive", "--push-state", "--pop-state", "-Bstatic", "-Bdynamic", "-S", "-s", "-E", "--export-dynamic", "--no-export-dynamic", "--enable-new-dtags", "--disable-new-dtags", "--eh-frame-hdr", "--no-undefined", "--fatal-warnings", "--warn-common", "--build-id", "--compress-debug-sections":
				default:
					allowed := false
					for _, prefix := range []string{"--build-id=", "--compress-debug-sections=", "--export-dynamic-symbol=", "--hash-style=", "--rpath=", "-rpath=", "--dynamic-linker="} {
						if strings.HasPrefix(part, prefix) {
							allowed = true
							break
						}
					}
					if !allowed {
						return fmt.Errorf("forwarded linker option %q is unsupported with split linking", part)
					}
				}
			}
		} else if strings.HasPrefix(arg, "-f") || strings.HasPrefix(arg, "-m") || strings.HasPrefix(arg, "-p") {
			allowed := false
			switch arg {
			case "-fuse-ld=bfd", "-fuse-ld=gold", "-fuse-ld=lld", "-fuse-ld=mold", "-m64", "-pthread", "-pass-exit-codes", "-fPIC", "-fpic", "-fPIE", "-fpie", "-fno-PIC", "-fno-pic", "-fno-PIE", "-fno-pie", "-fno-canonical-system-headers", "-fno-omit-frame-pointer", "-ffunction-sections", "-fdata-sections", "-fno-stack-protector", "-fstack-protector", "-fstack-protector-strong", "-fstack-protector-all":
				allowed = true
			}
			if !allowed {
				return fmt.Errorf("compiler option %q is unsupported with split linking", arg)
			}
		}
	}
	return nil
}

func prelinkEncode(value, tree, root string) (string, error) {
	if strings.Contains(value, prelinkTokenPrefix) {
		return "", fmt.Errorf("reserved prelink token in path or argument")
	}
	// Include a separator so a root named /work does not rewrite /worker.
	if value == filepath.Join(tree, prelinkOutputName) {
		return prelinkOutputToken, nil
	}
	for _, entry := range [][2]string{{tree, prelinkTreeToken}, {root, prelinkRootToken}, {filepath.Dir(root), prelinkParentToken}} {
		if value == entry[0] {
			value = entry[1]
		} else {
			value = strings.ReplaceAll(value, entry[0]+string(filepath.Separator), entry[1]+string(filepath.Separator))
		}
	}
	return value, nil
}

func prelinkDecode(value, tree, root, output string) (string, error) {
	for _, entry := range [][2]string{{prelinkTreeToken, tree}, {prelinkRootToken, root}, {prelinkParentToken, filepath.Dir(root)}, {prelinkOutputToken, output}} {
		value = strings.ReplaceAll(value, entry[0], entry[1])
	}
	if strings.Contains(value, prelinkTokenPrefix) {
		return "", fmt.Errorf("unknown prelink path token")
	}
	return value, nil
}

func runPrelink(e *env, args []string, tree string, nativeObjects []string, contentBuildID bool) error {
	old, hadOld := os.LookupEnv(prelinkCaptureEnv)
	if err := os.Setenv(prelinkCaptureEnv, tree); err != nil {
		return err
	}
	defer func() {
		if hadOld {
			os.Setenv(prelinkCaptureEnv, old)
		} else {
			os.Unsetenv(prelinkCaptureEnv)
		}
	}()
	var diagnostics bytes.Buffer
	linkErr := e.runCommandToFile(&diagnostics, &diagnostics, args)
	var exitErr *exec.ExitError
	if !errors.As(linkErr, &exitErr) || exitErr.ExitCode() != 2 || !strings.Contains(diagnostics.String(), prelinkHandoffMessage) || !strings.Contains(diagnostics.String(), "exit status 77") {
		return fmt.Errorf("Go linker did not complete the deliberate prelink handoff: %v\n%s", linkErr, diagnostics.Bytes())
	}
	if _, err := os.Stat(filepath.Join(tree, prelinkOutputName)); !os.IsNotExist(err) {
		return fmt.Errorf("prelink handoff unexpectedly produced a binary")
	}
	var receipt prelinkReceipt
	if err := readCgoJSON(filepath.Join(tree, prelinkReceiptName), &receipt); err != nil {
		return err
	}
	if receipt.Compiler == "" || receipt.Compiler != os.Getenv("GO_CC") {
		return fmt.Errorf("unexpected prelink compiler receipt")
	}
	insert, err := prelinkObjectInsertion(receipt.Args, tree)
	if err != nil {
		return err
	}
	if err := validatePrelinkCCArgs(receipt.Args); err != nil {
		return err
	}
	if err := validatePrelinkObject(filepath.Join(tree, "go.o")); err != nil {
		return err
	}
	root := abs(".")
	plan := prelinkPlan{Version: 1, SDK: runtime.Version(), NativeInsertIndex: insert, ContentBuildID: contentBuildID}
	plan.Compiler, err = prelinkEncode(receipt.Compiler, tree, root)
	if err != nil {
		return err
	}
	for _, arg := range receipt.Args {
		encoded, err := prelinkEncode(arg, tree, root)
		if err != nil {
			return err
		}
		plan.Args = append(plan.Args, encoded)
	}
	for _, path := range nativeObjects {
		encoded, err := prelinkEncode(path, tree, root)
		if err != nil {
			return err
		}
		plan.NativeObjects = append(plan.NativeObjects, encoded)
	}
	// Probes and the importcfg are temporary, potentially path-dependent files.
	// Only the relocatable Go object, copied native objects, and recipe remain.
	entries, err := os.ReadDir(tree)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "go.o" || prelinkHostObject.MatchString(entry.Name()) {
			continue
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unexpected prelink output %q", entry.Name())
		}
		if err := os.Remove(filepath.Join(tree, entry.Name())); err != nil {
			return err
		}
	}
	return writeCgoJSON(filepath.Join(tree, prelinkPlanName), plan)
}

func hostlink(args []string) error {
	args, _, err := expandParamsFiles(args)
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("hostlink", flag.ContinueOnError)
	e := envFlags(flags)
	tree := flags.String("prelink", "", "Input prelink tree.")
	output := flags.String("o", "", "Output executable.")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *tree == "" || *output == "" || flags.NArg() != 0 {
		return fmt.Errorf("hostlink requires -prelink and -o")
	}
	if err := e.checkFlagsAndSetGoroot(); err != nil {
		return err
	}
	if err := checkPrelinkSDK(e); err != nil {
		return err
	}
	*tree, *output = abs(*tree), abs(*output)
	var plan prelinkPlan
	if err := readCgoJSON(filepath.Join(*tree, prelinkPlanName), &plan); err != nil {
		return err
	}
	if plan.Version != 1 || plan.NativeInsertIndex < 0 || plan.NativeInsertIndex > len(plan.Args) {
		return fmt.Errorf("unsupported or invalid prelink recipe")
	}
	if err := validatePrelinkSDKVersions(runtime.Version(), plan.SDK); err != nil {
		return err
	}
	root := abs(".")
	compiler, err := prelinkDecode(plan.Compiler, *tree, root, *output)
	if err != nil {
		return err
	}
	var linkArgs []string
	for i := 0; i <= len(plan.Args); i++ {
		if i == plan.NativeInsertIndex {
			for _, encoded := range plan.NativeObjects {
				dir, err := prelinkDecode(encoded, *tree, root, *output)
				if err != nil {
					return err
				}
				_, objects, err := readCgoPrepared(dir, dir)
				if err != nil {
					return fmt.Errorf("reading native sidecar %s: %w", dir, err)
				}
				linkArgs = append(linkArgs, objects...)
			}
		}
		if i == len(plan.Args) {
			break
		}
		arg, err := prelinkDecode(plan.Args[i], *tree, root, *output)
		if err != nil {
			return err
		}
		linkArgs = append(linkArgs, arg)
	}
	command, cleanup, err := prelinkHostResponse(ccCommandArgs(compiler, root, linkArgs), filepath.Dir(*output))
	if err != nil {
		return err
	}
	defer cleanup()
	if err := e.runCommand(command); err != nil {
		return err
	}
	if err := finalizePrelinkELF(*output); err != nil {
		return fmt.Errorf("finalizing prelink executable: %w", err)
	}
	if plan.ContentBuildID {
		if err := setContentBuildID(*output); err != nil {
			return fmt.Errorf("setting build ID: %w", err)
		}
	}
	return nil
}

// Sidecar insertion can make a previously short Go linker command exceed the
// process argument limit. Use the same GCC-compatible %q-per-line format as
// the pinned Go linker, after all paths have been normalized for this action.
func prelinkHostResponse(command []string, dir string) ([]string, func(), error) {
	size := 0
	for _, arg := range command {
		size += len(arg) + 1
	}
	if size < 30<<10 {
		return command, func() {}, nil
	}
	f, err := os.CreateTemp(dir, ".hostlink-*.rsp")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.Remove(f.Name()) }
	var buf bytes.Buffer
	for _, arg := range command[1:] {
		fmt.Fprintf(&buf, "%q\n", arg)
	}
	_, writeErr := f.Write(buf.Bytes())
	closeErr := f.Close()
	if writeErr != nil {
		cleanup()
		return nil, nil, writeErr
	}
	if closeErr != nil {
		cleanup()
		return nil, nil, closeErr
	}
	return []string{command[0], "@" + f.Name()}, cleanup, nil
}
