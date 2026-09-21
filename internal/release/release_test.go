package release_test

import (
	"os"
	"strings"
	"testing"
)

func TestReleaseWorkflowDefinesSupportedTargetsAndTagBuilds(t *testing.T) {
	workflow := readRepositoryFile(t, ".github/workflows/release.yml")

	for _, target := range []string{
		"goos: windows",
		"goos: darwin",
		"goos: linux",
	} {
		if !strings.Contains(workflow, target) {
			t.Errorf("release workflow does not define %q", target)
		}
	}
	for _, binary := range []string{
		"rescrobble-windows-amd64.exe",
		"rescrobble-darwin-amd64",
		"rescrobble-linux-amd64",
	} {
		if !strings.Contains(workflow, binary) {
			t.Errorf("release workflow does not define binary %q", binary)
		}
	}
	if !strings.Contains(workflow, `tags:
      - "v*"`) {
		t.Error("release workflow is not triggered by version tags")
	}
	if !strings.Contains(workflow, `uses: softprops/action-gh-release@v2`) {
		t.Error("release workflow does not upload to a GitHub Release")
	}
	if !strings.Contains(workflow, "files: release-assets/*") {
		t.Error("release workflow does not upload the target binary")
	}
	if !strings.Contains(workflow, "VERSION: ${{ github.ref_name }}") ||
		!strings.Contains(workflow, `-X main.version=${VERSION}`) {
		t.Error("release workflow does not inject the tag version")
	}
	if !strings.Contains(workflow, "needs: build") {
		t.Error("release publication does not wait for all target builds")
	}
	if strings.Contains(workflow, `-X main.version=${{ github.ref_name }}`) {
		t.Error("release workflow interpolates the tag directly in a shell command")
	}
}

func TestReleaseDocumentationDefinesGitHubOnlyMVPDistribution(t *testing.T) {
	documentation := readRepositoryFile(t, "docs/releases.md")

	for _, required := range []string{
		"GitHub Releases",
		"Homebrew",
		"Scoop",
		"Docker",
		"rescrobble-windows-amd64.exe",
		"rescrobble-darwin-amd64",
		"rescrobble-linux-amd64",
		`.\rescrobble-windows-amd64.exe --help`,
		"./rescrobble-darwin-amd64 --help",
		"./rescrobble-linux-amd64 --help",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("release documentation does not contain %q", required)
		}
	}
}

func TestMITLicenseIsIncluded(t *testing.T) {
	license := readRepositoryFile(t, "LICENSE")
	for _, required := range []string{
		"MIT License",
		"Permission is hereby granted, free of charge",
		"THE SOFTWARE IS PROVIDED \"AS IS\"",
	} {
		if !strings.Contains(license, required) {
			t.Errorf("LICENSE does not contain %q", required)
		}
	}
}

func readRepositoryFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile("../../" + path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
