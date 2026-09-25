# Rescrobble: core import engine

## Problem and motivation

Spotify's integration with Last.fm silently stops scrobbling for
individual users from time to time — a broken auth token, an app
permission change, a missed webhook — and there's no notification when
it happens. By the time someone notices, they've lost weeks or months
of listening history from their Last.fm profile permanently, because
Last.fm has no mechanism of its own to backfill gaps. The only
complete record of what was actually played during that gap lives in
Spotify's own "Extended Streaming History" export, which any Spotify
user can request and download but which isn't in a form Last.fm (or
any tool today) can reconcile against existing scrobbles and import.

Rescrobble is a cross-platform CLI that closes this gap: it reads a
user's Spotify Extended Streaming History export, compares it against
what's already scrobbled on their Last.fm account, and submits exactly
the plays that are missing — nothing that's already there, nothing
that Spotify itself wouldn't have scrobbled in the first place. This
PRD covers the first shippable version of that tool: a real,
functioning product for Spotify → Last.fm specifically, released as an
open-source project from day one.

Provider extensibility beyond Spotify and Last.fm (Apple Music,
YouTube Music, Plex, Jellyfin, generic CSV, other scrobble targets) is
a deliberate non-goal here. The internal design should not paint itself
into a corner — data flows through an import/export/match/submit
pipeline shaped so a second provider could plug in later — but no
second provider is built, specified, or promised in this PRD. That's
future work, to be scoped as its own PRD once there's a concrete reason
to build it.

## Who this is for and how they'll use it

Rescrobble ships as a public, MIT-licensed, single static binary for
Windows, macOS, and Linux, distributed via GitHub Releases (Homebrew,
Scoop, and Docker distribution are explicitly deferred past this PRD).
Because it's public from the start, the bar for a stranger being able
to install it, understand what went wrong when something fails, and
trust it with their Last.fm credentials and personal listening history
is part of "done" here, not a later polish pass.

The intended flow: a user notices (or suspects) a scrobbling gap, logs
in once, downloads their Spotify export, runs `analyse` to see the
scope of what's missing before committing to anything, then runs
`import` to actually submit it. Nothing is written to Last.fm without
either an explicit `import` invocation or explicit confirmation at the
point of submission.

## Capability: authentication and credential storage

Rescrobble authenticates against the official Last.fm API using an API
key/secret pair (already provisioned for development) and per-user
session authentication obtained through Last.fm's auth flow. `login`
performs that flow and stores the resulting session; `logout` clears
it; `status` reports what's currently stored.

Credentials are stored exclusively via the OS-native secure credential
store — Windows Credential Manager, macOS Keychain, Linux Secret
Service — through a credential-storage abstraction internal to the
tool. There is deliberately no fallback (no plaintext file, no
encrypted-file-on-disk alternative): if no keyring backend is available
on the host (a common situation on headless Linux), `login` fails with
an explicit, actionable error rather than silently degrading security.
`doctor` (below) is where a user diagnoses that condition before it
surprises them mid-import.

Rescrobble supports multiple named Last.fm profiles from this first
release (e.g. a personal and a shared/family account), each with its
own stored credentials and its own import journal. The first profile a
user logs into becomes the active profile implicitly; `rescrobble
profile use <name>` switches which one is active. Commands operate on
the active profile unless a `--profile` flag names a different one.
`logout` without `--profile` logs out the active profile only.

## Capability: Spotify export parsing

Rescrobble reads Spotify Extended Streaming History exports: one or
more JSON files, optionally still packaged in the ZIP as Spotify
delivers it, potentially spanning multiple years and multiple files,
and scaling up to accounts with hundreds of thousands of recorded
plays. Parsing is streaming rather than load-everything-into-memory,
since the largest real-world exports (500k+ plays) should not require
loading the whole history into memory at once — this is a hard
constraint on the parser and the overall pipeline design, not an
optimization to add later, even though the MVP's own performance
validation only benchmarks it directly at 10,000–100,000 plays.

The parser extracts, per play: track name, artist, album, timestamp,
ms played, platform, Spotify track URI/identifier, and whatever else
the export schema provides. It must be resilient to the export schema
changing shape over time or across years of history, and to individual
records being malformed or missing expected fields — a bad record (or
even an entire corrupt file within a multi-file export) is skipped
with a logged warning, and the run continues; it does not abort the
whole import over one bad entry. All such warnings are visible in the
run's report (below), not just buried in logs.

Only music-track plays are in scope for scrobbling. Podcast episodes
and local files without a Spotify track URI present in the same export
are recognized and explicitly excluded — reported as "not scrobblable"
rather than silently dropped — since they generally lack the metadata
(or, for podcasts, the relevance) needed for Last.fm scrobbling.
Offline-mode plays of Spotify tracks are included when they have the
required metadata.

ZIP handling treats the archive as untrusted input: it must resist
path-traversal ("zip slip") extraction and must not allow an
adversarial or corrupted archive to exhaust memory or disk (a size cap
on decompressed content, with a clear error if exceeded, rather than
an unbounded extraction).

## Capability: missing-scrobble detection

For a given profile and Spotify export, Rescrobble fetches the user's
existing Last.fm scrobble history for the relevant date range and
compares it against the parsed Spotify plays to determine what's
missing. A play only counts as eligible for scrobbling at all if it
would satisfy Last.fm's own scrobble-eligibility rule (source track
audible for at least 30 seconds, and played for at least 4 minutes or
half the track's length, whichever is shorter) — Rescrobble should not
propose importing a play that a working Spotify↔Last.fm connection
would never have scrobbled in the first place. Because the Spotify
export provides `ms_played` but not the track's total duration, the
exact mechanics of approximating "half of track length" without a
separate metadata lookup is an open question this PRD does not resolve
— the epic/story that implements this comparison needs to either find
an acceptable approximation using ms_played alone and document its
error characteristics, or pull in a duration source, and should treat
that decision as part of its own scope rather than assuming it away.

Matching an eligible Spotify play against existing Last.fm scrobbles
compares artist, track, and timestamp, using ms-played as a secondary
signal to disambiguate repeated plays of the same track close together
in time (e.g. a skip immediately followed by a full replay). Timestamp
comparison uses a tolerance window, defaulting to ±60 seconds and
configurable via `--timestamp-tolerance`, to absorb clock drift between
Spotify's and Last.fm's own recorded timestamps.

Every comparison resolves to one of three confidence tiers — high,
medium, low — reflecting how certain the match (or non-match) is:
high confidence covers an exact or near-exact match within a tight
timestamp window; medium covers a match found only via the wider
configured tolerance or via the ms-played secondary signal; low covers
genuinely ambiguous cases (timestamps at the edge of tolerance, or
metadata variants like remasters/album re-releases where a name-based
match is uncertain). `analyse` surfaces these tiers so a user can judge
the comparison's overall reliability before running `import`. The
policy for what to do with a low-confidence "possible duplicate, or
possible genuine gap" case is deliberately to treat it as missing and
import it: the tool optimizes for actually repairing gaps, accepting
that a small number of already-scrobbled plays might occasionally be
resubmitted, over optimizing for never creating a duplicate at the cost
of leaving real gaps unfilled.

`analyse` is read-only: it performs this entire comparison and reports
the results (total Spotify plays, total in-scope Last.fm scrobbles,
estimated missing count, date range covered, confidence breakdown)
without submitting anything or writing any local state. `import`
performs the same comparison itself rather than reusing anything
`analyse` may have computed in an earlier invocation — the two commands
are independent, so that `import` is never acting on a comparison that
may have gone stale between when `analyse` ran and when the user
actually commits to importing.

## Capability: import engine

`import` submits exactly the plays identified as missing, respecting
Last.fm's scrobble-submission API (batches of up to 50 tracks per
call). Submission uses a fixed, conservative delay between batch
requests plus exponential backoff on 429/5xx responses — not an
adaptive/dynamic pacing scheme — favoring predictability and
debuggability over squeezing out maximum throughput, since the
practical bottleneck here is Last.fm's own rate limit, not local
compute.

Every submitted batch is recorded in a local, per-profile import
journal before Rescrobble proceeds to the next one. If the process is
interrupted or crashes partway through a run, restarting the same
`import` invocation resumes correctly: batches already recorded as
submitted in the journal are not resubmitted, and the run picks up
from the first unconfirmed batch. This journal is retained indefinitely
per profile as an audit trail (submitted plays are metadata, not audio,
so the storage cost is negligible even at hundreds of thousands of
plays); it is not auto-pruned on any time or event basis in this PRD,
though a future `doctor`/cleanup-style command may offer manual
pruning.

Running `import` with no `--from`/`--to` bounds processes the entire
export every time; the run remains safe and idempotent because
duplicate detection against Last.fm's actual current history (not a
locally cached "last import" watermark) is what determines what's
submitted. `--from`/`--to` narrow the date range under consideration,
and those bounds are interpreted in the local system's timezone,
matching how a person actually thinks about calendar dates. Any Spotify
play that matches an existing Last.fm scrobble under the matching rules
above is a true duplicate: it is silently skipped and counted, not
individually logged by default, matching the tool's summary-style
progress reporting.

Because a single `import` run can involve submitting a very large
number of scrobbles (e.g. months of a broken integration), `import`
prompts for interactive confirmation before submitting when more than
100 missing scrobbles are detected, showing the same kind of summary
`analyse` would show. Imports of 100 or fewer missing scrobbles do not
prompt. `--yes` skips this prompt for scripted/CI use.
`--dry-run` performs the entire comparison and would-be-submission
planning without making any network write, useful both as a safety
check ahead of a real run and as part of automation.

`verify` re-fetches the user's Last.fm history after an import and
confirms that scrobbles recorded as submitted in the local journal are
actually present on Last.fm — closing the loop on a known class of API
behavior where a submission can be accepted without actually landing.
Any journaled-but-missing scrobble is reported explicitly.

## Capability: reporting and diagnostics

`report --json` / `--csv` / `--html` produces a record of a run (or of
the current journal state) containing: counts and details of imported
scrobbles, skipped duplicates, failures, warnings (including any
malformed-input warnings from parsing), metadata issues, and execution
statistics (timing, batch counts, etc.). Each report also records the
configuration that produced it — timestamp tolerance, eligibility rule
applied, profile, date range — so that a user filing a bug report, or a
maintainer helping debug one, can see exactly what settings produced a
given outcome without having to ask.

`doctor` is an environment and connectivity diagnostic, not a
functional self-test: it checks whether an OS keyring backend is
available, whether stored credentials for the active (or a named)
profile are still valid, whether the Last.fm API is reachable and
authenticating correctly, and whether the local journal/config files
are intact and readable. It's the first thing a user (or someone
helping them) should run when something isn't working, and it should
give a specific, actionable answer rather than a generic "something's
wrong."

Structured logging is available at normal/verbose/debug levels.
Rescrobble auto-detects whether it's attached to an interactive
terminal: when it is, progress renders as the kind of live progress
bar and summary shown in the original product brief; when output is
piped, redirected, or otherwise non-interactive, it falls back to
plain, line-based log output so redirected output and log files stay
clean and parseable, following the convention most CLI tools already
use for this.

Configuration defaults (timestamp tolerance, active profile, batch
delay, etc.) can be set persistently in a config file, with any
command-line flag overriding the config file's value for that
invocation.

## Out of scope for this PRD

- Any music provider or scrobble target other than Spotify and
  Last.fm. The internal pipeline shape should not preclude adding one,
  but none is designed, built, or committed to here.
- Homebrew, Scoop, and Docker distribution — GitHub Releases binaries
  only for this release.
- Telemetry/usage analytics beyond a possible future opt-in mechanism
  that is off by default; nothing is collected in this PRD's scope.
- A formal, CI-gated benchmark at the full 500,000-play target scale.
  The architecture must not preclude that scale (streaming parsing, no
  full-history-in-memory requirement), but the MVP's own performance
  validation only exercises and measures 10,000–100,000 plays.
- Automatic per-profile "resume from last import" state — every
  `import` run considers the whole export (or the given `--from`/`--to`
  window) and relies on duplicate detection against live Last.fm data,
  not a local watermark.

## Assumptions

- A registered Last.fm API application (key and secret) already exists
  for development and is available to whoever implements this.
- Last.fm's documented API behavior (rate limits, batch submission
  limits, scrobble-eligibility rule) is accurate and stable enough to
  build against without our own separate confirmation process.
- Spotify's Extended Streaming History export format, while it may
  vary across years/schema versions, remains a JSON structure
  containing (at minimum) artist, track, timestamp, and ms-played
  information per play.

## Open questions

- How to approximate Last.fm's "played at least half the track's
  length" eligibility rule given that the Spotify export provides
  ms-played but not the track's total duration. This needs to be
  resolved (or an accepted approximation documented) as part of
  implementing the missing-scrobble detection capability, not deferred
  silently.
- Exact wording/thresholds for what counts as "a large number of
  missing scrobbles" for the interactive-confirmation prompt in
  `import` is left to whoever implements that capability to define and
  document, rather than fixed here.
