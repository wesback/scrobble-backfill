# Repository-specific agent context

Use this file for concise, durable facts that help future contributors and
coding agents work in this repository. Keep facts specific, verifiable from
the code, tests, or an explicit maintainer decision. Do not copy run logs,
task summaries, speculative explanations, or secrets here.

## Domain invariants

- Last.fm `track.scrobble` requests contain at most 50 tracks per batch (`internal/lastfm/submission.go`)
- Submission intent is journaled before dispatch, and successful batches transition from `planned` to `submitted` (`internal/lastfm/submission.go`)
- Journal runs are identified by profile and invocation, and their submission payloads contain track metadata rather than credentials (`internal/journal/journal.go`)
- Spotify plays are emitted only for records with a track URI and required metadata; podcast episodes and records without a Spotify track URI are excluded (`internal/spotify/ingest.go`)

## Architecture and integration points

- The CLI routes commands and injects configuration, credential, Last.fm, journal, and input dependencies (`internal/cli/cli.go`)
- Spotify ingestion streams normalized plays to a consumer instead of retaining the full export (`internal/spotify/ingest.go`)
- The Last.fm submission service owns batch sizing, pacing, retries, and journal transitions, while missing-play selection is handled elsewhere (`internal/lastfm/submission.go`)
- Linux uses the native credential store first and selects the encrypted file fallback when native access is unavailable (`internal/credentials/default_store_linux.go`)
- Non-Linux platforms use the OS-native credential store (`internal/credentials/default_store_nonlinux.go`)

## Verification and tooling

- `go test ./...` is the repository's single test command (`TESTING.md`)
- Continuous integration runs `go test ./...` for pull requests and pushes to `main` (`.github/workflows/test.yml`)
- The release-tag helper runs `go test ./...` before creating or pushing a tag (`scripts/tag-release.sh`)
- Building from source requires Go 1.23 or newer and uses `go build -o rescrobble ./cmd/rescrobble` (`README.md`)

## Operational workflows

- GitHub Releases are the supported distribution channel for Windows and Linux; Homebrew, Scoop, and Docker are deferred (`docs/releases.md`)
- Publish releases from a clean, up-to-date `main` checkout with `scripts/tag-release.sh vX.Y.Z`; do not publish with `git push --tags` (`docs/releases.md`)
- Each release includes Windows and Linux binaries, a `SHA256SUMS` manifest, and signed build provenance (`docs/releases.md`)
- PRDs are self-contained `docs/prds/<slug>.md` files, and merging a PR that touches `docs/prds/` is the pipeline's approval signal (`docs/prds/README.md`)
