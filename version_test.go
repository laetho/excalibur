package main

import "testing"

func TestAppVersionReadsBuildConfig(t *testing.T) {
	if appVersion() == "" {
		t.Fatal("expected info.version in build/config.yml")
	}
}
