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
		"runner: windows-2022",
		"runner: macos-15-intel",
		"runner: macos-14",
		"runner: ubuntu-24.04",
	} {
		if !strings.Contains(workflow, target) {
			t.Errorf("release workflow does not define %q", target)
		}
	}
	for _, binary := range []string{
		"rescrobble-windows-amd64.exe",
		"rescrobble-darwin-amd64",
		"rescrobble-darwin-arm64",
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
	if strings.Contains(workflow, "\npermissions:\n  contents: write") {
		t.Error("release workflow grants contents write permission to every job")
	}
	for _, permission := range []string{
		"contents: write",
		"id-token: write",
		"attestations: write",
	} {
		if !strings.Contains(workflow, permission) {
			t.Errorf("release workflow does not grant %q", permission)
		}
	}
	for _, command := range []string{
		"--help",
		"--version",
		`actual_version="$(./"${{ matrix.binary }}" --version)"`,
		`[[ "${actual_version}" != "${VERSION}" ]]`,
	} {
		if !strings.Contains(workflow, command) {
			t.Errorf("release workflow does not validate %q", command)
		}
	}
	for _, target := range []string{
		"goos: darwin\n            goarch: arm64\n            binary: rescrobble-darwin-arm64",
	} {
		if !strings.Contains(workflow, target) {
			t.Errorf("release workflow does not define %q", target)
		}
	}
	if strings.Count(workflow, "binary: ") != 4 {
		t.Errorf("release workflow defines %d binaries, want exactly 4", strings.Count(workflow, "binary: "))
	}
	checksumStart := strings.Index(workflow, "binaries=(\n")
	if checksumStart == -1 {
		t.Fatal("release workflow does not define a checksum binary list")
	}
	checksumEnd := strings.Index(workflow[checksumStart:], "\n          )")
	if checksumEnd == -1 {
		t.Fatal("release workflow does not define a checksum binary list")
	}
	checksumBinaries := workflow[checksumStart : checksumStart+checksumEnd]
	if strings.Count(checksumBinaries, "rescrobble-darwin-arm64") != 1 {
		t.Errorf("checksum binary list contains rescrobble-darwin-arm64 %d times, want exactly once",
			strings.Count(checksumBinaries, "rescrobble-darwin-arm64"))
	}
	if !strings.Contains(workflow, "uses: actions/attest-build-provenance@v2") ||
		!strings.Contains(workflow, "subject-path: ${{ matrix.binary }}") {
		t.Error("release workflow does not attest each built binary")
	}
	if !strings.Contains(workflow, `uses: softprops/action-gh-release@v2`) {
		t.Error("release workflow does not upload to a GitHub Release")
	}
	for _, asset := range []string{
		"release-assets/rescrobble-windows-amd64.exe",
		"release-assets/rescrobble-darwin-amd64",
		"release-assets/rescrobble-darwin-arm64",
		"release-assets/rescrobble-linux-amd64",
		"release-assets/SHA256SUMS",
	} {
		if !strings.Contains(workflow, asset) {
			t.Errorf("release workflow does not publish %q", asset)
		}
	}
	if strings.Contains(workflow, "files: release-assets/*") {
		t.Error("release workflow publishes an uncontrolled asset glob")
	}
	for _, required := range []string{
		`sha256sum "${binaries[@]}" > SHA256SUMS`,
		"generate_release_notes: true",
		"fail_on_unmatched_files: true",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("release workflow does not contain %q", required)
		}
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
	for _, path := range []string{"README.md", "docs/releases.md"} {
		documentation := readRepositoryFile(t, path)

		for _, required := range []string{
			"GitHub Releases",
			"Homebrew",
			"Scoop",
			"Docker",
			"rescrobble-windows-amd64.exe",
			"rescrobble-darwin-amd64",
			"rescrobble-darwin-arm64",
			"rescrobble-linux-amd64",
			"Apple Silicon macOS",
			`.\rescrobble-windows-amd64.exe --help`,
			"./rescrobble-darwin-amd64 --help",
			"./rescrobble-darwin-arm64 --help",
			"./rescrobble-linux-amd64 --help",
			"SHA256SUMS",
			"sha256sum --ignore-missing -c SHA256SUMS",
			"gh attestation verify ./rescrobble-windows-amd64.exe --repo wesback/scrobble-backfill",
			"gh attestation verify ./rescrobble-darwin-amd64 --repo wesback/scrobble-backfill",
			"gh attestation verify ./rescrobble-darwin-arm64 --repo wesback/scrobble-backfill",
			"gh attestation verify ./rescrobble-linux-amd64 --repo wesback/scrobble-backfill",
		} {
			if !strings.Contains(documentation, required) {
				t.Errorf("%s does not contain %q", path, required)
			}
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
