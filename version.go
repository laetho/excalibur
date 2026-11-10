package main

import (
	_ "embed"
	"regexp"
	"strings"
)

// build/config.yml is the single source of the app version: the release
// workflow writes the tag's version into `info.version` before building.
//
//go:embed build/config.yml
var buildConfig string

var buildConfigVersion = regexp.MustCompile(`(?m)^  version:\s*["']([^"']+)["']\s*$`)

// AppVersionInfo describes the running build for the About page.
type AppVersionInfo struct {
	Version string `json:"version"`
	// Development is true for `task dev` and other non-production builds.
	Development bool `json:"development"`
}

func appVersion() string {
	match := buildConfigVersion.FindStringSubmatch(buildConfig)
	if match == nil {
		return ""
	}
	return match[1]
}

// AppVersion returns the version of the running build. Release builds report
// the stamped version; development builds report their git description, or
// an empty version when that is unavailable.
func (a *App) AppVersion() AppVersionInfo {
	if !productionBuild {
		return AppVersionInfo{Version: strings.TrimPrefix(developmentVersion(), "v"), Development: true}
	}
	return AppVersionInfo{Version: appVersion()}
}
