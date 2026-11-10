package main

import (
	_ "embed"
	"regexp"
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
	// Development is true for `task dev` and other non-production builds, whose
	// config.yml version is a placeholder rather than a released version.
	Development bool `json:"development"`
}

func appVersion() string {
	match := buildConfigVersion.FindStringSubmatch(buildConfig)
	if match == nil {
		return ""
	}
	return match[1]
}

// AppVersion returns the version of the running build.
func (a *App) AppVersion() AppVersionInfo {
	return AppVersionInfo{Version: appVersion(), Development: !productionBuild}
}
