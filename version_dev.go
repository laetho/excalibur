//go:build !production

package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const productionBuild = false

// developmentVersion describes a dev build by its place in git history, e.g.
// "v0.4.0-14-g678e2a7-dirty". Dev builds are not stamped by the release
// workflow, so build/config.yml only holds a placeholder. Returns "" when git
// or the repository is unavailable.
func developmentVersion() string {
	// Dev builds keep absolute source paths, so this file's directory is the
	// repository, whatever the app's working directory is.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "describe", "--tags", "--always", "--dirty")
	cmd.Dir = filepath.Dir(file)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
