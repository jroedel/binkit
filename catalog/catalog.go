// Package catalog holds ready-made [binkit.Tool] definitions for tools people commonly
// need, so a consuming project does not have to re-derive release asset naming.
//
// The core binkit package deliberately does not import this one — the dependency arrow
// points in a single direction. Nothing here is privileged: a project that needs a tool
// the catalog lacks, or that disagrees with a definition, declares its own Tool literal
// and loses no functionality.
//
// Definitions are functions rather than variables so that a caller cannot mutate shared
// state that another caller depends on.
package catalog

import (
	"fmt"

	"github.com/jroedel/binkit"
)

// typstTargets maps GOOS/GOARCH to the Rust target triple Typst names its release
// assets after. Platforms absent from this map have no published Typst build.
var typstTargets = map[string]string{
	"linux/amd64":   "x86_64-unknown-linux-musl",
	"linux/arm64":   "aarch64-unknown-linux-musl",
	"linux/arm":     "armv7-unknown-linux-musleabi",
	"linux/riscv64": "riscv64gc-unknown-linux-gnu",
	"darwin/amd64":  "x86_64-apple-darwin",
	"darwin/arm64":  "aarch64-apple-darwin",
	"windows/amd64": "x86_64-pc-windows-msvc",
	"windows/arm64": "aarch64-pc-windows-msvc",
}

// Typst returns the definition for the Typst typesetter (https://typst.app).
//
// Typst publishes .tar.xz for every Unix target and .zip for Windows, each containing a
// single directory named after the target triple.
func Typst() binkit.Tool {
	return binkit.Tool{
		Name: "typst",
		Repo: "typst/typst",

		Asset: func(_, goos, goarch string) (string, error) {
			target, ok := typstTargets[goos+"/"+goarch]
			if !ok {
				return "", fmt.Errorf("typst publishes no build for %s/%s", goos, goarch)
			}
			if goos == "windows" {
				return "typst-" + target + ".zip", nil
			}
			return "typst-" + target + ".tar.xz", nil
		},

		BinaryPath: func(_, goos, goarch string) string {
			target := typstTargets[goos+"/"+goarch]
			if goos == "windows" {
				return "typst-" + target + "/typst.exe"
			}
			return "typst-" + target + "/typst"
		},
	}
}
