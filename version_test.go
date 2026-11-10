package main

import "testing"

func TestAppVersionReadsBuildConfig(t *testing.T) {
	if appVersion() == "" {
		t.Fatal("expected info.version in build/config.yml")
	}
}

func TestAppVersionInDevelopment(t *testing.T) {
	if productionBuild {
		t.Skip("production build")
	}
	info := (&App{}).AppVersion()
	if !info.Development {
		t.Fatal("expected a development build")
	}
	t.Logf("development version: %q", info.Version)
}
