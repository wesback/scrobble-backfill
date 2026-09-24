package release_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTagReleaseRejectsInvalidArguments(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "missing", want: "usage:"},
		{name: "missing version", args: []string{""}, want: "invalid version"},
		{name: "missing v prefix", args: []string{"1.2.0"}, want: "invalid version"},
		{name: "missing patch", args: []string{"v1.2"}, want: "invalid version"},
		{name: "prerelease suffix", args: []string{"v1.2.0-rc1"}, want: "invalid version"},
		{name: "extra argument", args: []string{"v1.2.0", "extra"}, want: "usage:"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := runTagRelease(t, test.args, nil)
			if result.err == nil {
				t.Fatal("script succeeded, want invalid arguments to be rejected")
			}
			if !strings.Contains(result.stderr, test.want) {
				t.Errorf("stderr %q does not explain the invalid arguments", result.stderr)
			}
			if result.log != "" {
				t.Errorf("commands ran before argument validation:\n%s", result.log)
			}
		})
	}
}

func TestTagReleaseRejectsUnsafeRepositoryState(t *testing.T) {
	for _, test := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "dirty working tree",
			env:  map[string]string{"STATUS_OUTPUT": " M tracked.txt"},
			want: "working tree is not clean",
		},
		{
			name: "wrong branch",
			env:  map[string]string{"BRANCH_OUTPUT": "story-63"},
			want: "current branch must be main",
		},
		{
			name: "local main behind origin",
			env:  map[string]string{"ORIGIN_MAIN_OUTPUT": "older-commit"},
			want: "HEAD is not up to date with origin/main",
		},
		{
			name: "local main ahead of origin",
			env:  map[string]string{"ORIGIN_MAIN_OUTPUT": "newer-commit"},
			want: "HEAD is not up to date with origin/main",
		},
		{
			name: "existing local tag",
			env:  map[string]string{"LOCAL_TAG_EXISTS": "1"},
			want: "already exists locally",
		},
		{
			name: "existing remote tag",
			env:  map[string]string{"REMOTE_TAG_OUTPUT": "tag-object refs/tags/v1.2.0"},
			want: "already exists on origin",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := runTagRelease(t, []string{"v1.2.0"}, test.env)
			if result.err == nil {
				t.Fatal("script succeeded, want repository state to be rejected")
			}
			if !strings.Contains(result.stderr, test.want) {
				t.Errorf("stderr %q does not contain %q", result.stderr, test.want)
			}
			assertNoTagOrPush(t, result.log)
		})
	}
}

func TestTagReleasePrintsGoTestFailureWithoutCreatingTag(t *testing.T) {
	result := runTagRelease(t, []string{"v1.2.0"}, map[string]string{
		"GO_TEST_EXIT":   "1",
		"GO_TEST_OUTPUT": "FAIL example.test/package [build failed]",
	})
	if result.err == nil {
		t.Fatal("script succeeded, want failing tests to reject the release")
	}
	for _, output := range []string{
		"FAIL example.test/package [build failed]",
		"go test ./... failed",
	} {
		if !strings.Contains(result.stdout+result.stderr, output) {
			t.Errorf("combined output %q does not contain %q", result.stdout+result.stderr, output)
		}
	}
	assertNoTagOrPush(t, result.log)
}

func TestTagReleaseCreatesAnnotatedTagAndPushesAfterChecks(t *testing.T) {
	result := runTagRelease(t, []string{"v1.2.0"}, nil)
	if result.err != nil {
		t.Fatalf("script failed: %v\nstdout:\n%s\nstderr:\n%s", result.err, result.stdout, result.stderr)
	}

	want := []string{
		"git status --porcelain",
		"git branch --show-current",
		"git fetch origin main:refs/remotes/origin/main",
		"git rev-parse HEAD",
		"git rev-parse origin/main",
		"git rev-parse --verify --quiet refs/tags/v1.2.0",
		"git ls-remote --tags origin refs/tags/v1.2.0",
		"go test ./...",
		"git tag -a v1.2.0 -m v1.2.0",
		"git push origin v1.2.0",
	}
	gotCommands := make([]string, 0)
	for _, line := range strings.Split(strings.TrimSpace(result.log), "\n") {
		if line != "" {
			gotCommands = append(gotCommands, line)
		}
	}
	if strings.Join(gotCommands, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands ran in unexpected order:\ngot:\n%s\nwant:\n%s",
			strings.Join(gotCommands, "\n"), strings.Join(want, "\n"))
	}
}

func TestTagReleaseRemovesLocalTagWhenPushFails(t *testing.T) {
	result := runTagRelease(t, []string{"v1.2.0"}, map[string]string{
		"PUSH_EXIT": "1",
	})
	if result.err == nil {
		t.Fatal("script succeeded, want failed push to reject the release")
	}
	if !strings.Contains(result.stderr, "removed the local tag") {
		t.Errorf("stderr %q does not explain local tag cleanup", result.stderr)
	}
	for _, command := range []string{
		"git tag -a v1.2.0 -m v1.2.0",
		"git push origin v1.2.0",
		"git tag -d v1.2.0",
	} {
		if !strings.Contains(result.log, command) {
			t.Errorf("command log %q does not contain %q", result.log, command)
		}
	}
}

type tagReleaseResult struct {
	stdout string
	stderr string
	log    string
	err    error
}

func runTagRelease(t *testing.T, args []string, overrides map[string]string) tagReleaseResult {
	t.Helper()

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repoRoot, "scripts", "tag-release.sh")
	info, err := os.Stat(script)
	if err != nil {
		t.Fatalf("stat release script: %v", err)
	}
	if info.Mode().Perm()&0111 == 0 {
		t.Fatal("release script is not executable")
	}

	tempDir := t.TempDir()
	binDir := filepath.Join(tempDir, "bin")
	if err := os.Mkdir(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(tempDir, "commands.log")
	writeExecutable(t, filepath.Join(binDir, "git"), `#!/usr/bin/env bash
printf 'git %s\n' "$*" >> "$COMMAND_LOG"
case "$1" in
  status) printf '%s' "${STATUS_OUTPUT:-}" ;;
  branch) printf '%s\n' "${BRANCH_OUTPUT:-main}" ;;
  fetch) exit "${FETCH_EXIT:-0}" ;;
  rev-parse)
    case "${4:-$2}" in
      HEAD) printf 'head-commit\n' ;;
      origin/main) printf '%s\n' "${ORIGIN_MAIN_OUTPUT:-head-commit}" ;;
      refs/tags/*)
        if [[ "${LOCAL_TAG_EXISTS:-0}" == 1 ]]; then
          printf 'tag-object\n'
          exit 0
        fi
        exit 1
        ;;
      *) exit 2 ;;
    esac
    ;;
  ls-remote)
    printf '%s' "${REMOTE_TAG_OUTPUT:-}"
    ;;
  tag)
    if [[ "${2:-}" == -d ]]; then
      exit "${TAG_DELETE_EXIT:-0}"
    fi
    ;;
  push) exit "${PUSH_EXIT:-0}" ;;
  *) exit 2 ;;
esac
`)
	writeExecutable(t, filepath.Join(binDir, "go"), `#!/usr/bin/env bash
printf 'go %s\n' "$*" >> "$COMMAND_LOG"
printf '%s' "${GO_TEST_OUTPUT:-}"
exit "${GO_TEST_EXIT:-0}"
`)

	command := exec.Command(script, args...)
	command.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"COMMAND_LOG="+logPath,
	)
	for key, value := range overrides {
		command.Env = append(command.Env, fmt.Sprintf("%s=%s", key, value))
	}
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	log, readErr := os.ReadFile(logPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read command log: %v", readErr)
	}

	return tagReleaseResult{
		stdout: stdout.String(),
		stderr: stderr.String(),
		log:    string(log),
		err:    err,
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertNoTagOrPush(t *testing.T, log string) {
	t.Helper()
	for _, line := range strings.Split(log, "\n") {
		if strings.HasPrefix(line, "git tag ") || strings.HasPrefix(line, "git push ") {
			t.Errorf("tag or push command ran before all checks passed: %s", line)
		}
	}
}
