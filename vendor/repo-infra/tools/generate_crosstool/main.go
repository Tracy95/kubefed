/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	crosstoolpb "github.com/bazelbuild/bazel/src/main/protobuf/crosstool_config_go_proto"
	"github.com/golang/protobuf/proto"
)

var (
	out         = flag.String("out", "", "filename for CROSSTOOL text proto to write")
	boilerplate = flag.String("boilerplate", "", "file containing boilerplate header")

	// The common toolchain fields shared across all targeted platforms.
	// This was auto-generated by Bazel in a docker container with gcc installed,
	// then manually updated to remove unnecessary fields and override others where needed.
	baseToolchain = `
    # These are required but will be overridden
	toolchain_identifier: ""
	target_system_name: ""
	target_cpu: ""
	target_libc: ""
	compiler: ""
	abi_version: ""
	abi_libc_version: ""

	builtin_sysroot: ""
	host_system_name: "host"
	needsPic: true
	supports_gold_linker: false
	supports_incremental_linker: false
	supports_fission: false
	supports_interface_shared_objects: false
	supports_normalizing_ar: false
	supports_start_end_lib: false

	objcopy_embed_flag: "-I"
	objcopy_embed_flag: "binary"

	# Anticipated future default.
	unfiltered_cxx_flag: "-no-canonical-prefixes"
	unfiltered_cxx_flag: "-fno-canonical-system-headers"

	# Make C++ compilation deterministic. Use linkstamping instead of these
	# compiler symbols.
	unfiltered_cxx_flag: "-Wno-builtin-macro-redefined"
	unfiltered_cxx_flag: "-D__DATE__=\"redacted\""
	unfiltered_cxx_flag: "-D__TIMESTAMP__=\"redacted\""
	unfiltered_cxx_flag: "-D__TIME__=\"redacted\""

	# Security hardening on by default.
	# Conservative choice; -D_FORTIFY_SOURCE=2 may be unsafe in some cases.
	# We need to undef it before redefining it as some distributions now have
	# it enabled by default.
	compiler_flag: "-U_FORTIFY_SOURCE"
	compiler_flag: "-D_FORTIFY_SOURCE=1"
	compiler_flag: "-fstack-protector"
	linker_flag: "-Wl,-z,relro,-z,now"

	# All warnings are enabled. Maybe enable -Werror as well?
	compiler_flag: "-Wall"
	# Enable a few more warnings that aren't part of -Wall.
	compiler_flag: "-Wunused-but-set-parameter"
	# But disable some that are problematic.
	compiler_flag: "-Wno-free-nonheap-object" # has false positives

	# Keep stack frames for debugging, even in opt mode.
	compiler_flag: "-fno-omit-frame-pointer"

	# Anticipated future default.
	linker_flag: "-no-canonical-prefixes"
	# Have gcc return the exit code from ld.
	linker_flag: "-pass-exit-codes"

	compilation_mode_flags {
	  mode: DBG
	  # Enable debug symbols.
	  compiler_flag: "-g"
	}
	compilation_mode_flags {
	  mode: OPT

	  # No debug symbols.
	  # Maybe we should enable https://gcc.gnu.org/wiki/DebugFission for opt or
	  # even generally? However, that can't happen here, as it requires special
	  # handling in Bazel.
	  compiler_flag: "-g0"

	  # Conservative choice for -O
	  # -O3 can increase binary size and even slow down the resulting binaries.
	  # Profile first and / or use FDO if you need better performance than this.
	  compiler_flag: "-O2"

	  # Disable assertions
	  compiler_flag: "-DNDEBUG"

	  # Removal of unused code and data at link time (can this increase binary size in some cases?).
	  compiler_flag: "-ffunction-sections"
	  compiler_flag: "-fdata-sections"
	  linker_flag: "-Wl,--gc-sections"
	}
	linking_mode_flags { mode: DYNAMIC }
`
)

func addToolchain(cpu, os string, cross bool) (*crosstoolpb.CToolchain, error) {
	toolchain := &crosstoolpb.CToolchain{}
	if err := proto.UnmarshalText(baseToolchain, toolchain); err != nil {
		return nil, err
	}

	var system string
	if cross {
		system = fmt.Sprintf("cross-%s-%s", cpu, os)
	} else {
		system = "host"
		cpu = "k8"
	}
	compiler := "gcc"
	libc := fmt.Sprintf("%s-%s", cpu, os)
	toolchain.Compiler = proto.String(compiler)
	toolchain.TargetLibc = proto.String(libc)
	toolchain.TargetCpu = proto.String(cpu)
	toolchain.TargetSystemName = proto.String(system)
	toolchain.ToolchainIdentifier = proto.String(system)
	toolchain.AbiVersion = proto.String(libc)
	toolchain.AbiLibcVersion = proto.String(libc)

	tools := []string{
		"ar", "ld", "cpp", "dwp", "gcc", "gcov", "ld",
		"nm", "objcopy", "objdump", "strip",
	}
	for _, tool := range tools {
		var path string
		if cross {
			path = fmt.Sprintf("/usr/bin/%s-%s", libc, tool)
		} else {
			path = fmt.Sprintf("/usr/bin/%s", tool)
		}
		toolchain.ToolPath = append(toolchain.ToolPath,
			&crosstoolpb.ToolPath{
				Name: proto.String(tool),
				Path: proto.String(path),
			})
	}

	if cross {
		toolchain.CxxBuiltinIncludeDirectory = append(
			toolchain.CxxBuiltinIncludeDirectory,
			fmt.Sprintf("/usr/%s/include", libc),
			fmt.Sprintf("/usr/lib/gcc-cross/%s", libc),
		)
	} else {
		toolchain.CxxBuiltinIncludeDirectory = append(
			toolchain.CxxBuiltinIncludeDirectory,
			"/usr/lib/gcc",
			"/usr/local/include",
			"/usr/include")
	}

	return toolchain, nil
}

func main() {
	flag.Parse()
	if *out == "" {
		log.Fatalf("--out must be provided")
	}

	crosstool := &crosstoolpb.CrosstoolRelease{
		MajorVersion: proto.String("local"),
		MinorVersion: proto.String(""),
	}
	targets := []struct {
		cpu   string
		libc  string
		cross bool
	}{
		{"k8", "local", false},
		{"arm", "linux-gnueabihf", true},
		{"aarch64", "linux-gnu", true},
		{"powerpc64le", "linux-gnu", true},
		{"s390x", "linux-gnu", true},
	}
	for _, t := range targets {
		toolchain, err := addToolchain(t.cpu, t.libc, t.cross)
		if err != nil {
			log.Fatalf("error creating toolchain for target %v: %q", t, err)
		}
		crosstool.Toolchain = append(crosstool.Toolchain, toolchain)

	}

	f, err := os.Create(*out)
	if err != nil {
		log.Fatalf("failed to open %q for writing: %q", *out, err)
	}

	if *boilerplate != "" {
		bp, err := os.Open(*boilerplate)
		if err != nil {
			log.Fatalf("failed to open %q for reading: %q", *boilerplate, err)
		}
		defer bp.Close()
		if _, err := io.Copy(f, bp); err != nil {
			log.Fatalf("failed copying boilerplate: %q", err)
		}
	}

	fmt.Fprintf(f, `# DO NOT EDIT
# This file contains the text format encoding of a
# %s
# protocol buffer generated by generate_crosstool.

`,
		proto.MessageName(crosstool))
	proto.MarshalText(f, crosstool)
}
