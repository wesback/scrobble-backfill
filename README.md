# Rescrobble

Rescrobble repairs missing Last.fm scrobbles from a Spotify Extended
Streaming History export. It is a cross-platform command-line tool for
Spotify and Last.fm: it reads the plays Spotify recorded, compares them with
the selected Last.fm profile, and submits only eligible plays that are
missing.

This is first-release software. The supported distribution channel is GitHub
Releases for Windows, macOS, and Linux. The current command set is:

```text
login       authenticate a Last.fm profile in the browser
logout      remove the selected profile's saved session
status      show whether the selected profile is logged in
doctor      diagnose configuration, credential tier and status, API, and journal
analyse     read-only Spotify-to-Last.fm comparison
import      compare and, unless dry-running, submit missing scrobbles
verify      check journaled submissions against current Last.fm history
report      render a journaled run as JSON, CSV, or HTML
profile use select the active named profile
```

Rescrobble supports Windows, Intel macOS, Apple Silicon macOS, and Linux on
the published release builds. It is MIT-licensed; see [LICENSE](LICENSE).
Homebrew, Scoop, Docker, and other package-manager distributions are
explicitly deferred for this release.

## Install

### GitHub Releases

Download the asset for the operating system from the
[latest GitHub Release](https://github.com/wesback/scrobble-backfill/releases).
The supported assets are:

| Platform | Asset | First invocation |
| --- | --- | --- |
| Windows | `rescrobble-windows-amd64.exe` | `.\rescrobble-windows-amd64.exe --help` |
| Intel macOS | `rescrobble-darwin-amd64` | `./rescrobble-darwin-amd64 --help` |
| Apple Silicon macOS | `rescrobble-darwin-arm64` | `./rescrobble-darwin-arm64 --help` |
| Linux | `rescrobble-linux-amd64` | `./rescrobble-linux-amd64 --help` |

On macOS or Linux, make the downloaded file executable:

```sh
chmod +x rescrobble-darwin-arm64  # use the asset matching your platform
./rescrobble-darwin-arm64 --help  # use the asset matching your platform
```

Before executing a downloaded binary, download the release's `SHA256SUMS`
file into the same directory and verify the matching asset:

```sh
sha256sum --ignore-missing -c SHA256SUMS
```

On PowerShell, compare the output of
`(Get-FileHash .\rescrobble-windows-amd64.exe -Algorithm SHA256).Hash.ToLower()`
with the Windows entry in `SHA256SUMS`. Do not run a binary when its digest
does not match. Also verify the signed build provenance with GitHub CLI:

```sh
gh attestation verify ./rescrobble-windows-amd64.exe --repo wesback/scrobble-backfill
gh attestation verify ./rescrobble-darwin-amd64 --repo wesback/scrobble-backfill
gh attestation verify ./rescrobble-darwin-arm64 --repo wesback/scrobble-backfill
gh attestation verify ./rescrobble-linux-amd64 --repo wesback/scrobble-backfill
```

Use the command matching the downloaded asset and do not run it if attestation
verification fails. See [docs/releases.md](docs/releases.md) for fallback
checksum commands on systems without `--ignore-missing`.

The binary names and publishing process are maintained in
[docs/releases.md](docs/releases.md). There is currently no supported
Homebrew formula, Scoop package, Docker image, or other package-manager
installation.

### Build from source

Install Go 1.23 or newer, clone this repository, and build the command:

```sh
go build -o rescrobble ./cmd/rescrobble
```

Run `./rescrobble --help` (or `.\rescrobble.exe --help` on Windows) from the
directory containing the binary.

## Before you start

### Configure Last.fm API access

Rescrobble uses a Last.fm API application key and secret for API requests.
Create a new application at <https://www.last.fm/api/account/create>, or
view and manage existing applications at <https://www.last.fm/api/accounts>.
Each application's page shows two separate 32-character values: an API key
and a Shared secret. Both are required, and the API key is easy to overlook
because it is not visually distinct from the secret.

The Callback URL field on that page has no effect on Rescrobble. Rescrobble
uses Last.fm's desktop application flow (`auth.getToken`, authorization on
the Last.fm site, then `auth.getSession`), not the web application flow that
relies on a callback redirect, so you can leave the field blank or enter any
placeholder value.

Set both environment variables in the shell that will run Rescrobble:

macOS and Linux:

```sh
printf 'Last.fm API key: '
IFS= read -r RESCOBBLE_LASTFM_API_KEY
export RESCOBBLE_LASTFM_API_KEY
printf 'Last.fm API secret: '
restore_echo() { stty echo 2>/dev/null || true; }
trap 'restore_echo; exit 130' INT
trap 'restore_echo; exit 143' TERM
trap 'restore_echo; exit 1' HUP
trap restore_echo EXIT
if ! stty -echo; then
  printf '\nUnable to disable terminal echo.\n' >&2
  exit 1
fi
IFS= read -r RESCOBBLE_LASTFM_API_SECRET
if [ $? -ne 0 ]; then
  printf '\nUnable to read the API secret.\n' >&2
  exit 1
fi
restore_echo
trap - EXIT INT TERM HUP
printf '\n'
export RESCOBBLE_LASTFM_API_SECRET
```

The `printf`, `read -r`, and `stty` form above is compatible with the
default shells on macOS and Linux. The API secret is not echoed while it is
entered.

Windows PowerShell:

```powershell
$env:RESCOBBLE_LASTFM_API_KEY = Read-Host 'Last.fm API key'
$secret = Read-Host 'Last.fm API secret' -AsSecureString
$env:RESCOBBLE_LASTFM_API_SECRET = [System.Net.NetworkCredential]::new('', $secret).Password
```

These prompts keep the values out of the command text and normal shell
history. Do not commit them or put them in a README, script checked into
source control, export file, or report. Rescrobble reads them from the
process environment and does not persist them; unset them when finished.

### Secure session credentials

`login` obtains a Last.fm session after browser authorization and stores the
session credential in the selected credential tier. Rescrobble prefers the
operating system's native secure store:

- Windows Credential Manager
- macOS Keychain
- Linux Secret Service

On Linux only, if Secret Service is unavailable, Rescrobble selects an
encrypted local file fallback. The fallback is weaker than a native keyring:
a user with access to the same host account, or root, can derive its key. It
is bound to the Linux machine identity and user, so copying its files to
another machine does not make the credential portable. If the machine
identity is lost or changes and the credential can no longer be decrypted,
log in again to establish a usable credential.

Credential tiers are selected when an operation runs; credentials are not
automatically migrated when native-store availability changes. `login`
establishes a credential in the tier currently selected, so explicitly log
in again to establish a credential in a different tier. To return to native
protection on Linux, make Secret Service available, then run `logout` and
`login`: logout clears the selected profile's credential from both the
native store and encrypted fallback, including a dormant credential in the
other tier, and the subsequent login stores a fresh credential in Secret
Service. On Windows and macOS, credentials always use the native store.

### Profiles

Credentials, Last.fm usernames, and import journals are scoped to named
profiles. The first profile used by `login` becomes the active profile; the
default name is `default` when no name is supplied. Commands use the active
profile unless `--profile <name>` selects another configured profile for that
invocation:

```sh
rescrobble --profile personal login
rescrobble --profile personal analyse spotify-2025.json
rescrobble profile use personal
rescrobble status
```

`profile use <name>` changes the active profile. `--profile <name>` takes
precedence for the command where it appears and does not change the active
profile. Use `logout` with the same profile selection to remove only that
profile's session:

```sh
rescrobble --profile personal logout
```

## Command reference

These forms match the output of `rescrobble --help` at this revision:

```text
rescrobble [--profile <name>] login
rescrobble [--profile <name>] logout
rescrobble [--profile <name>] status
rescrobble [--profile <name>] doctor
rescrobble [--profile <name>] analyse [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--timestamp-tolerance duration] <export>...
rescrobble [--profile <name>] import [--from YYYY-MM-DD] [--to YYYY-MM-DD] [--timestamp-tolerance duration] [--batch-delay duration] [--dry-run] [--yes] <export>...
rescrobble [--profile <name>] verify [<invocation-id>...]
rescrobble [--profile <name>] report (--json|--csv|--html) [<invocation-id>]
rescrobble [--profile <name>] profile use <name>
```

Global options are `--profile <name>`, `--log-level <level>` where level is
`normal`, `verbose`, or `debug`, `--verbose`, `--debug`, the report selectors
`--json`, `--csv`, and `--html`, and `--help`. Report selectors are used
exactly once with `report`.

## Recommended workflow

The safest workflow is deliberately ordered: inspect first, plan without
writing, then submit, then verify.

1. **Check the environment and log in.**

   ```sh
   rescrobble doctor
   rescrobble login
   rescrobble status
   ```

   `login` opens the official Last.fm authorization page in the default
   browser. Authorize the displayed token, return to the terminal, and press
   Enter. A successful login stores the session in the currently selected tier.
   `logout` removes the selected profile's session from both Linux tiers.

2. **Request and download Spotify data.** In Spotify's account privacy/data
   download flow, request **Extended Streaming History**. Spotify may provide
   one or more JSON files or a ZIP archive containing them. Keep the original
   files; do not edit timestamps or metadata.

3. **Run the read-only analysis.**

   ```sh
   rescrobble analyse my_spotify_export.zip
   ```

   Multiple JSON or ZIP inputs are accepted in one invocation:

   ```sh
   rescrobble analyse StreamingHistory0.json StreamingHistory1.json
   ```

4. **Preview the import without submitting anything.**

   ```sh
   rescrobble import --dry-run my_spotify_export.zip
   ```

5. **Run the confirmed import.**

   ```sh
   rescrobble import my_spotify_export.zip
   ```

   Imports with more than 100 missing plays ask `Continue? [y/N]`. Type
   `y` or `yes` to continue, or use `--yes` only when an unattended run is
   intentional:

   ```sh
   rescrobble import --yes my_spotify_export.zip
   ```

6. **Verify what Last.fm accepted.** Each live import has an invocation ID
   in its per-profile journal. Verify all journaled runs with submitted
   batches, or select one:

   ```sh
   rescrobble verify
   rescrobble verify import-1758540000000000000
   ```

   The number in the second example is illustrative; use the invocation ID
   printed or recorded for the actual run.

`analyse`, `import --dry-run`, and `doctor` do not submit writes to Last.fm.
Only a live `import` without `--dry-run`, after any required confirmation,
submits scrobbles with Last.fm's `track.scrobble` API. `login` and `logout`
change local authentication state, not scrobbles. `verify` and `report` are
also read-only.

## Spotify exports and analysis

`analyse` and `import` accept one or more Spotify Extended Streaming History
JSON files and `.zip` files. ZIP entries are read safely; path-traversal
entries and archives over the decompressed-size limit are rejected or
reported rather than extracted without bounds. Input is streamed, so the
whole export is not loaded into memory at once.

The analysis compares the selected date range of Spotify plays with the
selected Last.fm profile and prints:

- total Spotify plays read;
- in-scope Last.fm scrobbles;
- estimated missing plays;
- the covered date range;
- timestamp tolerance;
- high, medium, and low confidence counts; and
- source exclusion reasons.

Without `--from` or `--to`, Rescrobble discovers the bounds from the input.
Use inclusive local-calendar date bounds when narrowing a run:

```sh
rescrobble analyse --from 2025-01-01 --to 2025-03-31 \
  --timestamp-tolerance 60s my_spotify_export.zip
```

`--timestamp-tolerance` accepts a Go duration such as `60s`, or a
non-negative number of seconds. The default is 60 seconds. Matching uses
artist, track, and timestamp, with listening duration as a secondary signal.
High confidence is an exact or near-exact match, medium confidence uses the
wider tolerance or duration signal, and low confidence is ambiguous. A
low-confidence possible duplicate is reported as `missing`, not as a safe
duplicate. Consequently, a live `import` can submit that play and create a
duplicate. Exact and ordinary tolerance matches are skipped; review the
low-confidence count from `analyse` before authorizing a live import if
avoiding possible duplicates is more important than repairing every gap.

The Last.fm eligibility check excludes plays below 30 seconds. Shorter
eligible candidates must reach at least half the Last.fm track duration;
plays of at least four minutes take the fast path. Duration lookup failures
are surfaced in diagnostics and do not turn an ineligible record into a
submission.

Date bounds are interpreted in the machine's local timezone, including when
the export timestamps contain an offset. Comparison reads the bounded
Last.fm history and streams the Spotify input; it does not persist comparison
results.

The following are explicitly excluded from scrobbling and counted in the
summary or diagnostics:

- podcast episodes;
- local or offline files;
- malformed records or records missing required metadata;
- corrupt or unavailable JSON inputs; and
- unreadable or unsafe ZIP entries.

One bad record does not discard later valid inputs. Warnings include the
input and, where available, record, field, stable diagnostic code, and
reason.

## Import safety, pacing, and recovery

`import` performs its own live comparison rather than trusting an older
`analyse` result. Existing Last.fm history is checked again, so duplicates
are skipped and counted instead of being submitted. It submits missing plays
in Last.fm-compatible batches of at most 50. The default delay between
batches is one second and can be overridden per invocation:

```sh
rescrobble import --batch-delay 2s my_spotify_export.zip
```

Requests receive rate-aware retries for HTTP 429 and 5xx responses, with
exponential retry delays. A failure is reported instead of being silently
treated as success. `--batch-delay 0s` is valid but removes the normal
between-batch pacing; use it only when the consequences are understood.

Imports are journaled separately for each profile. A run records its
non-secret settings, planned batches, successful submissions, warnings, and
failures. A batch is made durable before dispatch and is marked submitted
only after the request succeeds. The journal therefore distinguishes
submitted batches from planned work after a process interruption, and its
batch state is resumable when the same invocation is reopened by the
submission service. The current CLI does not expose a `resume` command or
flag, however: every `import` command creates a new invocation ID. After an
interrupted CLI process, keep the journal, rerun `import` with the same
export, and understand that this starts a new journal run rather than
continuing the old one. The new live comparison skips plays already visible
in Last.fm; use the old invocation ID with `verify` or `report` to inspect
what the interrupted run recorded.

An import of 100 or fewer missing plays proceeds without an interactive
confirmation. More than 100 requires an explicit `y`/`yes`, unless `--yes`
is supplied. `--dry-run` performs parsing, date filtering, eligibility,
duplicate comparison, and journal batch planning, but sends no Last.fm
submission requests:

```sh
rescrobble import --from 2025-01-01 --to 2025-01-31 \
  --dry-run my_spotify_export.zip
```

## Verification and reports

`verify` re-reads current Last.fm history for each submitted payload in the
selected journal run. With no invocation IDs it checks all profile runs that
contain submitted scrobbles; with one or more IDs it checks only those runs:

```sh
rescrobble verify
rescrobble --profile family verify import-1758540000000000000
```

The output reports submitted, confirmed, and absent journaled scrobbles and
lists each absent artist, track, and timestamp. An absent item means it was
recorded as submitted locally but was not found in the current Last.fm
history; it is a verification mismatch, not proof that the original HTTP
request never reached Last.fm.

`report` renders the latest journaled run for the active profile, or a
specified invocation, in exactly one portable format:

```sh
rescrobble report --json > import-report.json
rescrobble report --csv import-1758540000000000000 > import-report.csv
rescrobble report --html > import-report.html
```

Reports contain structured run information: format version, profile,
invocation ID, local date range, timestamp tolerance, eligibility rule,
batch delay, counts for imported scrobbles, skipped duplicates, failures,
warnings, metadata issues, eligible plays and exclusions, execution timing,
and sanitized journal events. Reports do not contain session credentials.
JSON is intended for automation, CSV for tabular tools, and HTML for a
human-readable saved report.

## Logging and progress

Normal command output is concise summaries and diagnostics. When a terminal
is attached, progress can be presented interactively; when output is piped
or redirected, it remains line-oriented and suitable for a log file. Use
verbose or debug logging when diagnosing a problem:

```sh
rescrobble --verbose analyse my_spotify_export.zip
rescrobble --debug import --dry-run my_spotify_export.zip
rescrobble --log-level debug doctor
```

Warnings go to the diagnostic output and are also retained in import
journals and reports. Session values are redacted from diagnostic errors and
reports.

## Troubleshooting

### `doctor` reports configuration or profile failures

Run `rescrobble doctor` for the active profile, or
`rescrobble --profile <name> doctor` for a named profile. A missing or
malformed configuration, an unknown profile, or an unreadable journal is
reported as a failed check. Select an existing profile with
`rescrobble profile use <name>` or pass `--profile <name>`.

### The keyring is unavailable

`doctor` reports both native keyring availability and the selected credential
tier. Linux uses its encrypted local fallback when Secret Service is
unavailable; this is weaker than a native keyring, and a user with access to
the same host account or root can derive its key. The fallback is bound to
the Linux machine identity, so after that identity is lost and an existing
credential cannot be decrypted, run `login` again.

To restore native protection, install or enable the operating system's native
credential service and run `logout` followed by `login`. Store availability
changes do not migrate credentials automatically; `login` writes only to the
tier selected at that time. `logout` clears the selected profile's credential
from both Linux tiers. macOS requires Keychain access and Windows requires
Credential Manager.

### Login or authenticated commands reject the session

Confirm both API environment variables are set in the current shell, check
the profile with `status`, and run `login` again. If Last.fm rejects a stored
session, `doctor` reports that it was rejected and recommends logging in
again. Do not paste the session value into an issue or log.

### Last.fm cannot be reached or returns an API error

Run `doctor` and use `--debug` for more context. Check network access,
proxy/firewall policy, the API key and secret, and Last.fm availability.
HTTP connectivity errors, API errors, rate limits, and invalid
authentication are reported distinctly where Last.fm provides that
information. Imports retry rate-limit and server responses, but a
persistently failing request remains a failure in the journal.

### The export is malformed or contains unexpected sources

Keep the original Spotify JSON or ZIP and run `analyse` first. Individual
malformed records, missing fields, podcasts, and local/offline plays are
reported as warnings or exclusions while valid later records continue.
Correct the export or omit an unavailable input if the diagnostics identify
an input-level problem.

### ZIP safety diagnostics appear

Do not manually extract an archive with unsafe paths. Download the Spotify
export again and use the original ZIP. Path traversal entries, unreadable
entries, corrupt archives, and archives that exceed the decompressed-size
limit are not accepted as normal input.

### An import was interrupted

Do not delete the per-profile journal. Record the interrupted invocation ID
from the output or journal, then use `report <invocation-id>` and
`verify <invocation-id>` to inspect it. The current CLI has no resume command:
rerunning `import` against the same export starts a new invocation, performs
a fresh live comparison, and relies on Last.fm duplicate detection to avoid
resubmitting plays already visible there. It does not continue the old
invocation's planned batches. If a batch was accepted remotely but the
process stopped before it could be marked submitted, verification identifies
whether it is present.

### Verification reports an absent scrobble

Last.fm history can lag or differ at the timestamp/metadata boundary.
Confirm the profile and invocation ID, wait briefly, and run `verify` again.
Review the exact artist, track, and UTC timestamp printed for each absent
item. Do not assume that a local `submitted` journal state alone proves the
scrobble is visible in Last.fm.

## Project links

- [Core product PRD](docs/prds/rescrobble-core.md)
- [Release instructions and asset names](docs/releases.md)
- [License](LICENSE)
- [Testing instructions](TESTING.md)

Run the repository test command from the project root:

```sh
go test ./...
```
