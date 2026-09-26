package agentcontext_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const openingParagraph = "# Repository-specific agent context\n\n" +
	"Use this file for concise, durable facts that help future contributors and\n" +
	"coding agents work in this repository. Keep facts specific, verifiable from\n" +
	"the code, tests, or an explicit maintainer decision. Do not copy run logs,\n" +
	"task summaries, speculative explanations, or secrets here.\n\n"

func TestAgentContextDocument(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate test source")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", ".."))
	documentPath := filepath.Join(repoRoot, "docs", "agent-context.md")
	document, err := os.ReadFile(documentPath)
	if err != nil {
		t.Fatalf("read agent context: %v", err)
	}
	content := string(document)

	if !strings.HasPrefix(content, openingParagraph) {
		t.Fatal("agent context opening paragraph changed")
	}
	for _, forbidden := range []string{"TODO", "TBD", "FIXME"} {
		if strings.Contains(content, forbidden) {
			t.Errorf("agent context contains %q", forbidden)
		}
	}

	headings := []string{
		"## Domain invariants",
		"## Architecture and integration points",
		"## Verification and tooling",
		"## Operational workflows",
	}
	headingIndexes := make([]int, len(headings))
	for i, heading := range headings {
		if count := strings.Count(content, heading); count != 1 {
			t.Errorf("heading %q occurs %d times, want exactly once", heading, count)
		}
		headingIndexes[i] = strings.Index(content, heading)
		if headingIndexes[i] < 0 {
			t.Errorf("missing heading %q", heading)
		}
		if i > 0 && headingIndexes[i-1] >= 0 && headingIndexes[i] >= 0 &&
			headingIndexes[i-1] >= headingIndexes[i] {
			t.Errorf("heading %q is out of order", heading)
		}
	}

	verificationHasTestCommand := false
	for i, heading := range headings {
		if headingIndexes[i] < 0 {
			continue
		}
		start := headingIndexes[i] + len(heading)
		end := len(content)
		if i+1 < len(headings) && headingIndexes[i+1] >= 0 {
			end = headingIndexes[i+1]
		}
		section := content[start:end]
		bulletCount := 0
		for _, line := range strings.Split(section, "\n") {
			if !strings.HasPrefix(line, "- ") {
				continue
			}
			bulletCount++
			if i == 2 && strings.Contains(line, "single test command") &&
				strings.Contains(line, "go test ./...") {
				verificationHasTestCommand = true
			}
		}
		if bulletCount < 3 {
			t.Errorf("section %q has %d bullets, want at least 3", heading, bulletCount)
		}
	}
	if !verificationHasTestCommand {
		t.Error("Verification and tooling must state that go test ./... is the single test command")
	}

	for lineNumber, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		referenceStart := strings.LastIndex(line, " (`")
		if referenceStart <= len("- ") || !strings.HasSuffix(line, "`)") {
			t.Errorf("line %d bullet must end with a backtick-quoted source path: %q", lineNumber+1, line)
			continue
		}
		sourcePath := line[referenceStart+3 : len(line)-2]
		if sourcePath == "" || strings.Contains(sourcePath, "`") {
			t.Errorf("line %d has an invalid source path reference: %q", lineNumber+1, sourcePath)
			continue
		}
		cleanPath := filepath.Clean(filepath.FromSlash(sourcePath))
		if filepath.IsAbs(cleanPath) || cleanPath == "." || cleanPath == ".." ||
			strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) ||
			filepath.ToSlash(cleanPath) != sourcePath {
			t.Errorf("line %d source path is not repository-relative: %q", lineNumber+1, sourcePath)
			continue
		}
		if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(sourcePath))); err != nil {
			t.Errorf("line %d source path %q does not exist: %v", lineNumber+1, sourcePath, err)
		}
	}
}
