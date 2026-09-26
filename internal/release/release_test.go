package release_test

import (
	"os"
	"strings"
	"testing"
)

func TestReleaseWorkflowDefinesSupportedTargetsAndTagBuilds(t *testing.T) {
	workflow := readRepositoryFile(t, ".github/workflows/release.yml")

	lowercaseWorkflow := strings.ToLower(workflow)
	for _, unsupported := range []string{"darwin", "macos"} {
		if strings.Contains(lowercaseWorkflow, unsupported) {
			t.Errorf("release workflow contains unsupported platform reference %q", unsupported)
		}
	}

	for _, target := range []string{
		"goos: windows",
		"goos: linux",
		"runner: windows-2022",
		"runner: ubuntu-24.04",
	} {
		if !strings.Contains(workflow, target) {
			t.Errorf("release workflow does not define %q", target)
		}
	}
	for _, binary := range []string{
		"rescrobble-windows-amd64.exe",
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
		"          - name: Windows amd64\n            runner: windows-2022\n            goos: windows\n            goarch: amd64\n            binary: rescrobble-windows-amd64.exe",
		"          - name: Linux amd64\n            runner: ubuntu-24.04\n            goos: linux\n            goarch: amd64\n            binary: rescrobble-linux-amd64",
	} {
		if !strings.Contains(workflow, target) {
			t.Errorf("release workflow does not define %q", target)
		}
	}
	if strings.Count(workflow, "          - name: ") != 2 {
		t.Errorf("release workflow defines %d matrix entries, want exactly 2", strings.Count(workflow, "          - name: "))
	}
	if strings.Count(workflow, "binary: ") != 2 {
		t.Errorf("release workflow defines %d binaries, want exactly 2", strings.Count(workflow, "binary: "))
	}
	checksumList := "binaries=(\n            rescrobble-windows-amd64.exe\n            rescrobble-linux-amd64\n          )"
	if strings.Count(workflow, checksumList) != 1 {
		t.Errorf("release workflow checksum list does not contain exactly the Windows and Linux binaries once")
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
		"release-assets/rescrobble-linux-amd64",
		"release-assets/SHA256SUMS",
	} {
		if !strings.Contains(workflow, asset) {
			t.Errorf("release workflow does not publish %q", asset)
		}
	}
	releaseAssetList := "files: |\n            release-assets/rescrobble-windows-amd64.exe\n            release-assets/rescrobble-linux-amd64\n            release-assets/SHA256SUMS"
	releaseAssetStart := strings.Index(workflow, "files: |")
	if releaseAssetStart == -1 || strings.TrimSpace(workflow[releaseAssetStart:]) != releaseAssetList {
		t.Error("release asset upload list does not contain exactly the Windows and Linux binaries and checksum once")
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
			"rescrobble-linux-amd64",
			`.\rescrobble-windows-amd64.exe --help`,
			"./rescrobble-linux-amd64 --help",
			"SHA256SUMS",
			"sha256sum --ignore-missing -c SHA256SUMS",
			"gh attestation verify ./rescrobble-windows-amd64.exe --repo wesback/scrobble-backfill",
			"gh attestation verify ./rescrobble-linux-amd64 --repo wesback/scrobble-backfill",
		} {
			if !strings.Contains(documentation, required) {
				t.Errorf("%s does not contain %q", path, required)
			}
		}
		for _, unsupported := range []string{
			"rescrobble-darwin",
			"Intel macOS",
			"Apple Silicon macOS",
		} {
			if strings.Contains(documentation, unsupported) {
				t.Errorf("%s contains unsupported release reference %q", path, unsupported)
			}
		}
	}
}

func TestReadmeSupportedPlatformsAreWindowsAndLinux(t *testing.T) {
	readme := readRepositoryFile(t, "README.md")
	sentenceStart := strings.Index(readme, "The supported distribution channel")
	if sentenceStart == -1 {
		t.Fatal("README does not state its supported distribution channel")
	}
	sentenceEnd := strings.Index(readme[sentenceStart:], ".")
	if sentenceEnd == -1 {
		t.Fatal("README supported-platforms sentence is not terminated")
	}
	supportedPlatformsSentence := readme[sentenceStart : sentenceStart+sentenceEnd]
	for _, platform := range []string{"Windows", "Linux"} {
		if !strings.Contains(supportedPlatformsSentence, platform) {
			t.Errorf("README supported-platforms sentence does not name %s", platform)
		}
	}
	if strings.Contains(strings.ToLower(supportedPlatformsSentence), "macos") {
		t.Error("README supported-platforms sentence names macOS")
	}
}

func TestReleaseDocumentationUsesValidatedTagHelper(t *testing.T) {
	documentation := readRepositoryFile(t, "docs/releases.md")

	for _, required := range []string{
		"scripts/tag-release.sh v1.0.0",
		"clean, up-to-date `main` checkout",
		"Do not use `git push --tags`",
	} {
		if !strings.Contains(documentation, required) {
			t.Errorf("release documentation does not contain %q", required)
		}
	}
	if strings.Contains(documentation, "by pushing a version tag") {
		t.Error("release documentation still directs maintainers to push a version tag manually")
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
