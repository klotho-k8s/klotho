//go:build mage

// Mage build file for klotho. Install mage once with
// "go install github.com/magefile/mage@latest", then run "mage -l" for the
// target list, "mage" for the default build, or "mage ci" for the lint, test
// and build pipeline. With no mage on PATH, "go run github.com/magefile/mage
// <target>" works too.
package main

import (
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
)

// Default is the target mage runs when given none.
var Default = Build

// binary is the output path, with a .exe suffix on Windows.
func binary() string {
	b := "bin/klotho"
	if runtime.GOOS == "windows" {
		b += ".exe"
	}
	return b
}

// version resolves the build version: $VERSION if set, else `git describe`,
// else "dev".
func version() string {
	if v := os.Getenv("VERSION"); v != "" {
		return v
	}
	if out, err := sh.Output("git", "describe", "--tags", "--always", "--dirty"); err == nil && out != "" {
		return out
	}
	return "dev"
}

// ldflags stamps version, commit and build date into cmd/klotho's main.
func ldflags() string {
	commit, err := sh.Output("git", "rev-parse", "--short", "HEAD")
	if err != nil || commit == "" {
		commit = "none"
	}
	date := time.Now().UTC().Format(time.RFC3339)
	return fmt.Sprintf("-s -w -X main.version=%s -X main.commit=%s -X main.date=%s", version(), commit, date)
}

// Build compiles the klotho binary into bin/.
func Build() error {
	return sh.RunV("go", "build", "-trimpath", "-ldflags", ldflags(), "-o", binary(), "./cmd/klotho")
}

// Test runs the test suite with the race detector.
func Test() error {
	return sh.RunV("go", "test", "-race", "./...")
}

// Lint runs golangci-lint over the module.
func Lint() error {
	return sh.RunV("golangci-lint", "run")
}

// Fmt formats the code with go fmt.
func Fmt() error {
	return sh.RunV("go", "fmt", "./...")
}

// Vet runs go vet over the module.
func Vet() error {
	return sh.RunV("go", "vet", "./...")
}

// All formats, vets, tests and builds.
func All() {
	mg.SerialDeps(Fmt, Vet, Test, Build)
}

// CI runs lint, tests and build, the same pipeline as GitHub Actions.
func CI() {
	mg.SerialDeps(Lint, Test, Build)
}

// Clean removes build artifacts.
func Clean() error {
	for _, p := range []string{"bin", "cover.out"} {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}
