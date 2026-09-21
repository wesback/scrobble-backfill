<!-- ci-fix-loop-prds-readme:begin -->
PRDs for this repo live here as `<slug>.md` files. Merging a PR that
touches this directory is what the pipeline treats as approval — see
the parent repo's pipeline README for the full mechanism.

## What actually helps

`epic-planner` reads the exact file at your merge commit, in full, and
breaks it into epics — coherent, independently shippable capabilities,
not arbitrary chunks of the document's length. It never re-fetches or
combines other files, so **write one self-contained `<slug>.md`**, not
a PRD split across linked files — anything outside this one file is
invisible to it. It also refuses anything over 200,000 bytes, so keep
it a PRD, not a spec dump.

It works better from a few honest paragraphs than from a filled-out
form, but a few things consistently help it reason well:

- **What problem this solves and for whom** — the actual motivation,
  not just the feature name.
- **Distinguishable capabilities**, each concrete enough that a reader
  could propose splitting them into epics without asking you anything.
  This doesn't mean writing "Epic 1: ..." yourself — `epic-planner`
  does that split — but a PRD that reads as one undifferentiated wall
  of requirements gives it nothing to cut along.
- **Scope**, in plain terms: concrete enough that "is X included?" has
  an obvious answer from reading it.
- **Out of scope**, explicitly. An unstated boundary gets treated as
  something to guess at, and `epic-planner` guesses conservatively —
  under-scoping costs a review round-trip more often than
  over-scoping does.
- **Success criteria at the capability level** — what capability exists
  and why it matters, not test-level acceptance criteria. Those get
  written later, per story, by `story-planner`, against a concrete
  slice of work; writing them here just gets thrown away or ends up
  contradicting whatever the story actually covers.
- **Ordering between capabilities, spelled out in prose**, if one
  genuinely can't start before another (e.g. a data model another
  capability depends on). `epic-planner` has no structural dependency
  field at this stage — it only carries this forward as prose into each
  epic's body — so an unstated ordering constraint is invisible to
  everything downstream until a human notices.
- **Known overlap or dependencies** with what's already shipped, if
  you're aware of any. `epic-planner` also checks this repo's own
  memory for that, but you may know something it can't infer.

None of this needs headers or a fixed order — write it the way you'd
explain the feature to a colleague who's about to plan the work, not
as a form. If something is genuinely undecided, say so rather than
picking an answer to fill the section: `epic-planner` is built to open
fewer, honestly-scoped epics and flag what's unclear, not to resolve
ambiguity on your behalf.

## Starting one

Any conversation that ends in a `docs/prds/<slug>.md` commit and a PR
works — a coding CLI run in this checkout, a chat window, whatever you
have open. `GETTING-STARTED.md` in the pipeline source has a suggested
opening prompt if you want one.
<!-- ci-fix-loop-prds-readme:end -->
