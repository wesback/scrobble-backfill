<!-- ci-fix-loop-pipeline:begin -->
## Working with this repo's automation pipeline

This repo is polled by a GitHub-label-driven pipeline: a PR touching
`docs/prds/` (see that directory's own README), an issue labeled
`type:epic` or `type:story` with `needs-review` cleared, or a PR
labeled `autofix` each trigger an automated stage once a human has
approved it. Full mechanism: the pipeline source's `README.md`.

**Asked to draft a bug report or small feature request** (too small
for a full PRD)? Give it a `## Acceptance criteria` section specific
enough that someone else could verify it without asking you anything
— concrete, checkable outcomes, not "should work properly" or "handle
edge cases." That section is the contract the deterministic story runner and
acceptance reviewer verify once the issue is labeled `type:story`. Ask what's needed to
pin that down rather than guessing.

**Acceptance criteria rules for stories and bug reports:**
Criteria are evaluated by an automated runner and an independent
acceptance reviewer against the test output and diff of a single worktree.
To avoid stalling implementation or failing review convergence:
- **Use absolute measurable thresholds, not comparative claims.** Never write
  "no regression," "at least as readable/fast as before," or "remains consistent."
  The runner does not retain historical baselines from prior commits. State the
  exact numeric target, status code, or standard (e.g. "WCAG AA contrast ratio of
  at least 4.5:1 across both light and dark themes").
- **State definitive styling and behavior, not open-ended conditionals.** Never
  write "whatever the background ends up being" or "unless intentional, in which
  case document in the PR." Settle visual hierarchy and design choices upfront.
- **Specify concrete automated verification.** Frame criteria around checkable
  assertions (unit tests, integration tests, or browser tests measuring rendered
  properties) rather than subjective visual inspection.
- **Never require verification this repo's deterministic test command doesn't
  perform.** "The production build passes," "no lint errors," "typecheck is
  clean" — the story runner hands the acceptance reviewer only that command's
  own output, so a criterion about a build/lint/typecheck step it doesn't run
  can never be demonstrated and the story will retry to its ceiling and
  escalate no matter how good the implementation is. `run-planning-stage.sh`
  now rejects such a criterion before creating the issue, so don't rely on
  that as your check — write it correctly the first time. If a check that
  runs an additional command genuinely belongs in "done," say so in the
  narrative body as something a human needs to configure (a test-command
  override), not as an acceptance criterion, until that override exists.
- **A named command must already exist and be freely addable.** If a
  criterion says running a package script or another repository verification
  command must exit 0 with no warnings, that command must already exist in
  `package.json` or `.pipeline-verification-commands`, or adding it must not
  require touching a file the blast-radius classifier treats as
  shared-or-control-plane (version pins, CI workflow files, backend/provider
  blocks, and the like) — that class of change needs an operator to add the
  `hard-to-reverse-approved` label before it can be published, which an
  autonomous run cannot grant itself. A criterion that quietly needs both
  "add missing tooling" and "touch a hard-to-reverse file" has no path to
  self-heal and will escalate every time for the same reason. Either wire
  the missing script (and whatever config it depends on) yourself before
  filing the story, or say plainly in the narrative that an operator needs
  to add the approval label first.

**Asked to draft a PRD** for something bigger? See `docs/prds/README.md`
in this repo — that's the convention this pipeline expects.

**Issue and PRD quality bar** — if anything is ambiguous, missing, or
under-specified, stop and ask a clarifying question before writing the
issue or PRD. Do not guess missing business context, desired behavior,
scope, user impact, edge cases, or acceptance criteria. Drafts should
include: a short problem statement, user impact, in-scope and out-of-scope
items, assumptions, success criteria / acceptance criteria, and any open
questions. A story or PRD without concrete acceptance criteria is not ready
for the pipeline.

Don't clear a `needs-review` label yourself unless you're the human
actually approving that stage — it's the pipeline's only gate.
<!-- ci-fix-loop-pipeline:end -->
