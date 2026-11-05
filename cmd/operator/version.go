/*
Copyright Gregor Pogacnik 2026.

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
	"os"
	"runtime/debug"
)

// version is set at build time with -ldflags "-X main.version=<version>" (see Makefile and
// Dockerfile).
var version = ""

// buildVersion returns the stamped version, else the VCS revision Go embedded in the binary, else
// "unknown".
func buildVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	revision, dirty := "", false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if revision == "" {
		return "unknown"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if dirty {
		revision += "-dirty"
	}
	return revision
}

// usage prints the version above the flag defaults.
func usage() {
	out := flag.CommandLine.Output()
	_, _ = fmt.Fprintf(out, "ingress-doperator operator %s\n\nUsage of %s:\n", buildVersion(), os.Args[0])
	flag.PrintDefaults()
}
