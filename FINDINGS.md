# FINDINGS

An honest account of what RTS does, what it measurably saves, where it does
nothing at all, and what's known to be broken or approximate.

RTS is a token-reduction tool. The temptation with tools like this is to quote
the best-case number and stop talking. This document exists so nobody — the
author included — has to reverse-engineer the caveats from the source later.

---

## What RTS optimizes

**File reading. That's the whole surface area.**

RTS is an MCP server plus a PreToolUse hook. It intercepts the ways Claude Code
would otherwise pull file contents into context and serves smaller
representations instead:

- **Skeleton view of large Ruby files** — files at or over 100 lines
  (`tool.SkeletonThreshold`) come back as structure only: classes, modules,
  method signatures, Rails macros (`has_many`, `before_action`, `validates`,
  `scope`, …), visibility keywords, and Minitest `test "..." do` descriptions.
  Bodies, comments, and blank lines are dropped. Every kept line carries its
  real line number, and leading whitespace is stripped.
- **Surgical method extraction** — `view_bionic_method` pulls named method
  bodies (and Minitest blocks, matched by description substring) out of a file
  without reading the rest. Multiple names in one call cost one file scan.
- **Symbol lookup** — `find_symbol` answers "where is this defined?" from a
  prebuilt `.rts/symbols.json` index, replacing `grep -rn 'def foo'` and its
  noise.
- **Cross-file search** — `search_text` wraps ripgrep with `.gitignore`
  awareness and `vendor`/`tmp`/`log` exclusions.
- **Directory listing** — `list_directory` gives a depth-bounded tree with
  per-file line counts, so the next tool choice (skeleton vs. full read) is
  informed.

Everything else Claude Code does is untouched.

---

## Measured value

On the reads it actually handles, RTS is **~58% cheaper than native Read**.
That number is real and it is also the most misleading number in this document
if quoted alone.

The thing that matters is: **reading is often a minority of total session
cost.** RTS's share of the win is bounded by how much of a session is spent
reading files in the first place.

Two measured extremes from real session data:

- A **command-execution-heavy and debugging-heavy** session: RTS accounted for
  **~7% of total token usage**. Most tokens went to Bash output and to
  back-and-forth reasoning. A 58% discount on 7% of the bill is a rounding
  error.
- **Read-heavy exploration** sessions: the slice RTS touches is much larger,
  and so is the saving.

**Net:** high value on exploration-heavy work — landing in an unfamiliar
codebase, tracing how a feature hangs together, answering "where does X live".
Modest value on refactor-heavy and debug-heavy work, where the files are
already known and the tokens are going somewhere RTS can't reach.

If you are choosing whether to run RTS for a given task, that's the question to
ask: *am I about to explore, or am I about to iterate on files I already know?*

---

## Where RTS does not help

Being specific here is more useful than being encouraging.

- **Bash tool output.** Test runs, lint output, migration logs,
  `rails runner` diagnostics, stack traces. RTS does not touch any of it. In
  practice this is frequently the single largest line item in a session.
- **Reasoning and debugging tokens.** The back-and-forth of forming a
  hypothesis, testing it, and revising it. No file-reading tool affects this.
- **Tasks where the files are already known.** If you already know you're
  editing three specific methods, RTS saves you the cost of finding them —
  which you weren't going to pay anyway.
- **Editing.** RTS serves reads. Edit/Write go through native tools untouched.

---

## Cross-session memory

Every tool above makes one read cheaper. None of them addressed the cost that
is not paid inside a session at all: **a new session knows nothing about the
codebase, so it re-derives the same orientation the last one did, and throws
it away again at the end.** Cheaper reads make that rediscovery cheaper. They
do not make it stop happening.

Three stores under `.rts/` now do, and the line between them is the whole
design:

| Store | What it is | Rebuildable? |
|---|---|---|
| `symbols.json` | Every definition in the project | Yes — `rts index` |
| `map.json` | The derived map: project kind, layout, entry points, most-read files, files too big to read whole | Yes — `rts index` |
| `insights.json` | Findings a session concluded and wrote down | **No** |
| `graph.db` | Feature maps: which classes and files make up a feature, and how they connect (see *Feature maps* below) | **No** |

The first two are *derived*: regenerated wholesale, always true of the code as
it stands, safe to delete. The third is not. It holds conclusions, which can
be wrong and can go stale the moment the code under them changes.

**They are kept in separate stores, and every read labels which one it is
handing back.** Merging them would have been less code and would have turned
RTS from a code lens into a confident liar — a past session's guess, read back
by the next one as a fact about the codebase, with a tool's authority behind
it. The brief says so in as many words: *these are notes, NOT verified facts.*

Two mechanisms carry them into a session:

- **A `SessionStart` hook** emits the map, the findings, and — only when there
  is drift — a staleness warning, as `additionalContext`. Not as a system
  prompt: static instructions stay byte-identical across sessions so they keep
  being cached, and only the part that genuinely differs per project and per
  day is paid for. A `resume` is skipped, because the transcript already
  carries the brief; a `compact` is not, because the surviving summary
  routinely drops it.
- **A `project_map` tool** returns the same text on demand. It exists as well
  as the hook because the hook cannot reach Claude Code's built-in Explore and
  Plan subagents (see below) — and a subagent is precisely the context that
  opens knowing nothing.

`record_insight` is the one write in an otherwise read-only server. Its
description gates it hard: not unprompted trivia, not anything obvious from a
file's structure, not anything specific to the current task. A store full of
noise is worse than an empty one.

### What keeps it from rotting

A memory that only accumulates is a memory that ends up wrong. Four things
push back, and none of them require anybody to remember to tidy up:

- **Dead links drop out.** A finding names the files it rests on. When every
  one of them is gone, the finding stops being shown — it can only mislead the
  session that reads it. A finding that named three files and has one left
  survives with the surviving link.
- **Corrections replace rather than accumulate.** `record_insight` takes a
  `supersedes` id, so a wrong note is removed in the same write that fixes it,
  instead of sitting there arguing with its replacement.
- **There is a ceiling.** Sixty findings, oldest evicted first. A memory with
  no eviction is a context tax that grows forever.
- **A person can delete one.** `rts insights rm <id>`, without having to ask a
  model to do it.

### The index keeps itself current

Two weeks of real use in a large Rails repo settled the question of whether
anyone would run `rts index` by hand: nobody did. Across 38 Claude Code
sessions the index sat **17 days old**, and `find_symbol` spent that whole
stretch missing whatever had been added since. Every session opened on a map
of a codebase that had moved on.

The `SessionStart` hook (`hook/refresh.go`, `refreshIfStale`) now closes that
gap itself. On every start it checks drift with the same content
fingerprints described above (`index.CheckDrift`), and if the code has
actually changed, it starts `rts index` as a detached background process and
returns immediately — the session never waits on it. A reindex of a
~4,100-file Rails app takes about 1.7s, and the MCP server reloads
`symbols.json` whenever its mtime moves, so `find_symbol` is current a moment
after the session opens. Because drift is content-based, a restamp with
identical content — rebase, checkout — does not trigger a reindex. The brief
that session receives says a refresh is under way and that its map is from
the previous run, rather than telling the reader to go run `rts index`. A
resumed session is refreshed too — it skips the brief, but still uses
`find_symbol`. And this is opt-in: only a project that already has
`.rts/symbols.json` is ever touched, so opening Claude Code in some other
repo never creates a `.rts` directory there. An index written before
fingerprints existed can't be checked, so it's rebuilt outright — the
rebuild is what gives it fingerprints in the first place.

Running unattended creates two risks a manual, one-off run never had:

- **Two sessions opening together would start two indexes writing the same
  files.** `rts index` now takes a lock (`.rts/index.lock`); a second run
  sees it held and skips, since the run already in flight produces the same
  index. A lock older than 5 minutes is treated as left by a dead run and is
  broken. The lock is released before exiting on failure — `os.Exit` skips
  deferred calls, so that release could not be left to a `defer`.
- **The MCP server could reload `symbols.json` mid-write and read truncated
  JSON.** The derived stores are now written through a temp file plus
  rename (`index/safewrite.go`), so a reader sees the old file or the new
  one, never half of one.

Honest limits: the session that triggers a refresh still opens on the old
map — the new one is ready moments later, not immediately. And the
`SessionStart` hook logs nothing, so there is no measurement yet of how
often refreshes actually run, or whether the brief's "refresh under way"
message helps compared to silence.

### Worktrees share the main checkout's memory

Real sessions run inside git worktrees created under a project's main
checkout (`.claude/worktrees/<name>/`), and `.rts/` is gitignored, so a
worktree never has one of its own. Across the last two weeks, 35 of 73
sessions ran in such a worktree since 2026-09-13: no `SessionStart` brief, no
recorded findings, and a `find_symbol` that told each of them to run `rts
index`, as if the project had never been touched.

The rule now is that a linked worktree uses its main checkout's `.rts/` — for
reading, writing, and indexing (`index.ResolveRoot`). This resolves cleanly
because symbol paths in the index are stored relative to the project root:
the main checkout's index describes the same code the worktree has checked
out, just from a different directory, so it still resolves correctly against
the worktree's own files. Findings become shared across every worktree as a
side effect, rather than siloed one store per checkout.

`ResolveRoot` parses `.git`, `commondir` and the worktree's own `.git` file
directly rather than running `git rev-parse` — the MCP server is launched
with an empty environment (`env: {}`), so `git` is not on its PATH there.

Honest limit: a symbol added only on a worktree's own branch is not in the
main index until that code reaches the main checkout, so `find_symbol` misses
it there in the meantime.

### What it does not do

- **Nothing generates the findings automatically.** Bionic's data layer — the
  system this design is taken from — runs a paid extraction model over every
  document to produce its graph. RTS is a zero-dependency Go binary with a
  heuristic line parser and no network, and it never will do that. The map is
  derived structurally; the findings are written by whichever Claude session
  bothers to write one. An unused `record_insight` means an empty store.
- **Staleness is detected by content hash, after mtime proved unusable.** The
  first version compared modification times. Measured on a working Rails repo
  an hour after a rebase: **493 Ruby files reported changed, 492 of them
  byte-identical to HEAD.** Every git operation that rewrites the working tree
  — rebase, checkout, branch switch, stash pop — restamps mtimes wholesale, so
  the warning fired at full volume on days nothing had changed. A session that
  read it learned to distrust the map, and the one file that had genuinely
  moved was buried under 492 that had not.

  `rts index` now fingerprints every indexed file (FNV-64a, in
  `.rts/fingerprints.json`) and the check compares content. mtime survives as
  a pre-filter only — a file whose mtime has not moved cannot have changed, so
  it is never read — which keeps the walk fast while making the answer true.
  Deletions became visible in the same change: a path in the fingerprints with
  nothing on disk is a deletion, which the mtime check structurally could not
  see.
- **The "most-read files" list starts empty on an existing install.** It is
  counted from `~/.rts/events.jsonl`, which is one global file for every
  project on this machine — and relative paths are not unique across repos, so
  two Rails apps both hold `config/application.rb`. Events now carry the
  project root they were logged under and are counted only against it. Events
  written before that do not, and are skipped rather than guessed at: a wrong
  list of hot files is worse than no list, and the section omits itself when
  it has nothing.
- **The "largest files" list excludes tests, on purpose.** Measured on a real
  Rails app, the ten largest Ruby files were eight test files — a session
  orienting itself was handed `parse_job_test.rb` and never saw `project.rb`
  or `content_element.rb` at all. A test file is something you open because
  the task took you there, not something you orient on. They stay in the
  layout table, where knowing the suite is large is worth something.
- **TypeScript and JavaScript are indexed and get the code views.**
  `rts index` also reads `.ts`, `.tsx`, `.js` and their variants (not `.d.ts`,
  and not `dist/` or `app/assets/builds/`): top-level classes, interfaces,
  types, enums, functions and constants, and the methods inside a class —
  including the unnamed `export default class extends Controller` every
  Stimulus controller is, whose methods are filed under no class. Hangout, a
  TypeScript project with 425 such files, went from 0 symbols to 7,996, in
  under a second; steroidd gains about 2,400 from its JavaScript. So
  `find_symbol` and named feature-map members work in both.
  `view_bionic_skeleton` (and `view_file` on a large script) outlines a
  script from the same parse — imports, then every declaration with its line,
  bodies cut at their opening brace, short interfaces and types shown whole:
  Hangout's 6,647-line `main.ts` comes back in 13% of its characters.
  `view_bionic_method` returns any declaration's full text, matched bare
  (`put`) or by class (`BlobStore.put`), with every definition when a bare
  name has several. A Go repo still has no symbols.

### What a real session found, and what it cost

The first outside review of this feature ran on a live Rails repo and reported
two failures. Both were real; neither was quite what it looked like. Chasing
them turned up four separate defects, three of them older than the memory
feature itself:

1. **The staleness warning cried wolf** — 492 phantom changed files after a
   rebase. Fixed above; this is the one the review correctly identified.
2. **`find_symbol` reported its successes as failures.** A namespace match
   printed `No exact match for "PdfRequester"; found 4 symbol(s)…`. It had
   found four correct answers; the session read the first three words, treated
   it as a miss, and paid for follow-up calls. Now it leads with the find.
3. **`find_symbol` offered no near-misses on a true miss**, and blamed
   staleness for every one. A lookup for `Projects::Reports::PdfDownloads\
   Controller` was told to re-index, which would not have helped. The "did you
   mean" machinery `view_bionic_method` has always had was simply never wired
   to the symbol lookup. It is now, and re-indexing is mentioned last.
4. **A fully-qualified lookup could not match.** `Find`'s tiers compared the
   query against the bare name, the last segment, and `"::"+query` as a
   suffix — so a name that *was* the query matched nothing. **Searching for a
   class by its complete, correct name was the one way to be told it did not
   exist.** Found only because the new suggestion code started replying "did
   you mean" with the exact string that had just been searched for.

A fifth, cosmetic but wrong: qualified names rendered a nested class with a
dot (`Projects::Reports.Controller`), which is not valid Ruby, while class
methods rendered with `::`. The two separators were swapped.

One claim in that review needs correcting, because it fed the wrong diagnosis:
`find_symbol` was reported as having "saved literally nothing — 6 calls, 0
chars". It logs no baseline (see the instrumentation note above), so its
savings are **absent from the measurement, not measured as zero.** The misses
were real; the zero was an artifact.

---

## Feature maps

The three stores above still left one cost untouched. A session about board
templates in steroidd on 2026-10-20 opened by sending an Explore agent to
"map out everything involved". It ran **149 turns — 61 file reads, 38 greps,
5 shell commands, about 493k uncached input tokens** — and came back with a
24k-character checklist of every registry, prompt, asset, fixture and test a
new template touches. The symbol index could say where each class lived; it
could not say which twenty-nine of them were the feature. The next session on
board templates would have paid all of that again.

`record_feature` and `touchpoints` close that gap. A session that worked a
feature out saves it as a **feature map** — members (classes, files,
directories, each with a role), typed edges between them, and up to eight
notes — and every later session gets it back in one call. The steroidd map
built from that Explore report reads back in **6,348 characters, about 1,600
tokens**.

**The first comparison saved nothing.** Two fresh plan-mode sessions on the
same prompt ("list every file I need to change to add a `bar_chart`
template"), one with the map and one without: 48 turns and $1.25 with it, 39
turns and $1.09 without, all 12 required items found by both. The session
with the map read it, said "useful map, but its notes are unverified — let me
check the key claims against the code", and checked everything. The map
gave it no reason not to: it labelled itself as notes to confirm, and two of
its notes named code that exists only on a branch, so on `dev` they looked
wrong.

Two changes followed. The map now says what its structure rests on — every
name resolved to real code when it was saved, and an unchanged fingerprint
means the files are byte-identical since — and keeps "not verified" on the
notes alone. And the steroidd map was rewritten without the branch-only
claims. Then three pairs, alternating which ran first:

| Pair | With the map | Without the map |
|---|---|---|
| 1 | 23 turns, $0.56 | 19 turns, $1.92 (sent an Explore agent) |
| 2 | 10 turns, $0.32 | 39 turns, $1.04 |
| 3 | 23 turns, $0.68 | 46 turns, $1.05 |
| Mean | 18.7 turns, $0.52 | 34.7 turns, $1.34 |

Every run with the map was cheaper than every run without it, and every run
found all 12 required items (scored by matching each item's file or registry
name in the answer). Against the two runs without an Explore agent, the map
roughly halves the cost ($0.52 against $1.05) and the uncached input (36k
against 70k tokens). It did not stop the session reading: runs with the map
still made 6 to 18 tool calls after it, checking the specific lines they
quoted. That is the right amount of checking; the first comparison's was
not. Three pairs on one task is a small sample, and the prompt asked for a
list rather than an edit, so "tokens before the first edit" is measured here
as tokens for the whole answer.

The first pair was also the first measurement of RTS in plan mode, because it
did not work there before: every RTS tool was refused ("Cannot call
mcp__rts__… while in plan mode") until the read-only ones declared MCP's
`readOnlyHint`. The session without the map then answered fully in 39 turns
using RTS's own search and skeleton tools, where the Explore agent that
prompted all this had taken 149 without them.

### Claude supplies the understanding; RTS keeps it honest

Nothing in RTS can work out what a feature *is* — that takes reading the code
with a purpose, which is what the session already did. What RTS can do is
refuse to store a map that is not about real code:

- **On write**, every member and edge end must resolve: a class or module in
  the index (an ambiguous short name, a method, or a typo is refused with what
  was found instead), or a path that exists. Edge kinds are a closed list. The
  whole map is refused if anything fails, with every problem listed at once —
  half a map stored silently would read back as the whole one.
- **Secrets, strictly**: a protected credential file can be neither a member
  nor named in a note, on top of the redaction every RTS tool already applies.
- **No pointers into scratch folders**: a note that names a file under `tmp/`,
  `log/`, `coverage/`, `node_modules/` or `.claude/` is refused. The first
  real map written from a resumed session pointed at `tmp/il_sample/build.rb`,
  which is gone once its worktree is cleaned up.
- **On read**, each member file is hashed and compared with its hash at write
  time. A changed file is marked `[changed]`, the map is still shown, and the
  reader is told to confirm around it. Most of a stale map is still right.
- **On `rts index`**, members and edges naming code that no longer exists are
  dropped, and a feature with no members left goes with them.
- Saving a feature that already exists **replaces** its map; the old one is
  kept in `feature_history`. Notes and structure are rendered separately, the
  notes under the same "not verified" label as findings.

### Why this store is SQLite

Not for speed. On a test graph of 40,000 classes and 200,000 edges — about five
times what steroidd would hold — a JSON file answered a two-step lookup in
0.04 ms after a 124 ms load, SQLite in about 0.5 ms with no load; Claude sees
the same text either way. The difference is how the store is written: the
other stores are rebuilt whole by `rts index`, while the graph is added to one
feature at a time by many sessions, two at once whenever worktrees share a
checkout's `.rts`. SQLite updates one feature without rewriting the file and
serialises concurrent writers.

That second point needed one fix to be true. `Record` reads before it writes,
and a SQLite transaction that starts as a reader cannot wait its way into
writing once another connection has committed: it fails at once with
`SQLITE_BUSY`, busy timeout or not. Eight concurrent writers in the test lost
five maps that way until transactions began `IMMEDIATE`.

The cost: the first dependencies in `go.mod` (`modernc.org/sqlite`, pure Go,
pinned to v1.59.0 because v1.60 requires Go 1.26), and the binary grew from
4.3 MB to 10.9 MB. `rts graph` prints the maps, since the file cannot be read
in an editor.

### A Stop hook asks for the map

Waiting for a session to call `record_feature` on its own does not work —
`record_insight` had been available for two weeks and steroidd held seven
notes. So RTS also answers Claude Code's `Stop` event, which fires when Claude
finishes a reply. It reads the session's transcript, and its subagents',
for the files read (`Read`, `view_file`, the skeleton and method views) and
edited (`Edit`, `Write`, `MultiEdit`, `NotebookEdit`); searches do not count,
since a grep that matched forty files read none of them. Files an
up-to-date map covers are dropped, unless the session edited them: in a
worktree it changed its own copy while the map still matches the main
checkout.

What is left is grouped into **areas** by file name — extensions, a
partial's underscore, `_test`/`_spec`, a Rails role suffix such as
`_controller` or `_job`, and a deploy task's timestamp stripped, then made
singular, with a generic name like `index.html.erb` taking its folder's — so
`board_template.rb`, `board_templates_controller.rb` and
`views/board_templates/index.html.erb` are one area. An area is asked about
when the session read five or more of its files, or edited any. The ask
blocks the stop once, names the areas, and tells the session to map only
what it read; every area named is recorded in `stop_asks` and never asked
about again in that session. It stays silent in plan mode (where
`record_feature` is refused), on a stop the hook itself caused, without an
index, and under `RTS_DISABLE=1`.

Replayed against the steroidd session that built `bar_chart` (97 files
touched, 27 edited), it would have asked once, at the end, naming 18 areas
— 11 once the board template map existed. File-name areas split a
cross-cutting feature into many, so the ask lists the eight largest and
names the rest in a line, and says that one feature may span several areas.

### Pointing sessions at maps when it matters

Listing the mapped features at session start was not enough. A steroidd
dispatch worker on 2026-10-21 had the list in context and explored by hand
anyway; asked why, it said it "should have called touchpoints before
exploring". Its first message was "Read …/worker-task.md and follow it", so
nothing it was told named a feature, and its first read of mapped code —
`html_generator.rb`, a member of five maps — came several calls later,
with nothing said.

Hangout's dispatcher showed two more gaps the same day. It runs in the main
checkout while its workers build in worktrees, and the Stop hook duly asked
it to map what they had built — but both its maps, "All Hands globe" and
"All Hands time capsule", were refused file by file: the code existed only
in a worker's worktree, and names were checked against the main checkout
and the session's own. They are now also looked for in every other worktree
git records, each indexed once and only on a miss; replayed, both maps
resolve. And a resumed session gets the brief when its transcript holds
none: the rule that skips a resume assumed the session had been briefed at
its start, which is not true of one begun before RTS's hooks were wired in.

Two hooks now speak at those moments, sharing one record per session
(`nudges`) so a feature is named at most once and never after the session
has passed it to `touchpoints`:

- **UserPromptSubmit** checks each message for a feature's name, a member's
  path, or a distinctive class name (namespaced or multi-word CamelCase, so
  "image" never means the Image model) — and follows any file path the
  message points to and checks that file the same way. That worker's
  `worker-task.md` names five maps, including "board refine change path",
  the one its task was about; it would have heard so before its first tool
  call. The file is read directly because the worker read it with `cat`,
  which a read hook does not see.
- **PostToolUse** on `Read`, `view_file` and the skeleton and method views
  names the features a file belongs to, member or inside a member folder.

Both add context rather than block anything; the session still decides.

The steroidd graph also showed the cost of maps written without checking
what exists: "board templates" shares 7 of its 10 files with "html board
templates". `record_feature` now refuses a map when 60% or more of its files
(at least 3) are already in one other feature, naming it, unless the call
lists that feature in `distinct_from`. On steroidd's first nine maps the
duplicate sat at 70%, and every related-but-distinct pair at 40% or less.

### The startup list stays short; a search finds the rest

The brief lists maps by name only — ten of steroidd's came to 953
characters, against 6,014 for one full map — but a list that grows with the
project is still a cost paid before any work starts. It now lists every map
while a project has up to 30, and past that only the 10 most recently
updated, with a line on how to find the others. Only the listed maps have
their files hashed for "changed since", so session start does not slow down
as maps accumulate either.

The others are found two ways. The hooks above match a message, task file or
read against every map, listed or not. And `touchpoints` now searches when
given words that name no feature or class: each word must begin a word in a
map's name, summary, member names or notes (so "ask" finds "asked" but not
"task", and two-letter words like "ai" must match whole), scored in that
order. On steroidd, "pdf sizing" finds exactly "html board sizing in report
and pdf"; "billing" says nothing matches and how many maps it searched,
instead of listing every name as a miss used to. A topic word no longer
indexes every worktree on the way to that answer: `touchpoints` looks a
class up in the main index only, which kept a miss in a repo with eight
worktrees from taking seconds.

### A field report, checked against the data

A steroidd session reported a day of use: `search_text`, `view_file`,
`view_bionic_method` and `find_symbol` carried every investigation; it never
called `touchpoints`, saved one map only because the Stop hook asked, and
trusted the code over the maps. Its three suggestions, checked:

- **Re-index after a pull — built, on the miss.** `find_symbol` found
  nothing for a class a mid-session `git pull` had brought in; the startup
  refresh had run before the pull. A miss now checks for drift (a tenth of
  a second on steroidd) and, if the code has moved, reindexes (about two
  seconds) and asks again, saying so. A git hook would have meant
  installing scripts into every repository and would still miss changes
  made without them.
- **Nudge at search time — not built.** The session skipped `touchpoints`
  because one search had already answered a one-line question, which was
  right: a map pays off for work across many files, not a lookup. Nudging
  on search results would fire earlier and name more maps for the same
  outcome. The nudge now says so itself: call `touchpoints` if the task
  goes beyond a quick lookup, carry on if the answer is in hand.
- **Down-weight shared files in the duplicate check — not built; the data
  disagreed.** The refused map's 7 files were all in "integration landscape
  board", its edit-marker audit and tests included — a slice of a bigger
  map, not shared plumbing alone. Ignoring files found in 3 or more maps
  still flagged it, and stopped catching the real duplicate. File overlap
  cannot tell a sub-feature from a repeat; `distinct_from` handled it in
  one resave, as designed.

What lowered the session's trust most was a fourth thing: "27 changed
since" on maps it read from dev. Checked from dev, 22 of "html bar chart
board"'s 24 files differed and none was missing — it had been mapped on an
unmerged branch. A map now records its branch (graph schema 5), `rts index`
fills it in once for older maps from the checkout whose files match most of
them (12 of steroidd's 14), and a map read from another branch says which
branch it matches and that differing files are likely that branch's
unmerged work.

### Maps can include a branch's own code

Names were first checked only against the main checkout's index, built from
whatever branch it has checked out. Of the steroidd sessions that explored
most in October, every one still on disk worked in a worktree on a branch
2 to 29 commits ahead of `dev` — so a session mapping a feature it was
building would have been refused for exactly the files it added.

A name the main index lacks is now looked for in the session's own checkout:
the worktree is indexed on the spot, once per write and only when needed
(0.8 s for steroidd's 6,045 files). Every resolved name is stored with the
file it was found in (`refs`), and `rts index` prunes a name only when that
file exists in no checkout — the main one or any worktree git still records.
Branch work therefore survives until it is merged, and goes when its worktree
is removed unmerged. "Changed since" is judged against the reader's own
checkout, so the session that wrote a map sees its branch's files as
unchanged, and a session on `dev` sees them marked.

### What it does not do yet

- **No edges are read off the code.** Every edge is a session's claim, checked
  only for its ends existing. `has_many`, `include` and inheritance could come
  from the parser (phase 3).

---

## Known limitations

### Small files carry a small net cost

`view_file` prepends a one-line header (`# rts: 42 lines .md (full)`) to
passthrough content. On a file well under the skeleton threshold, that header
is pure overhead — RTS returns the same bytes native Read would, plus the
header. The cost is bounded and small (one short line), and the compact-header
work minimized it, but it is **net-negative on tiny files** and honesty
requires saying so.

### Parsing is pattern-based, not a real Ruby parser

The skeletonizer, the method extractor, and the symbol indexer are all
heuristic — prefix matching on trimmed lines plus block-nesting counting. There
is no Ruby AST anywhere in this codebase. Consequences:

- Exotic or clever syntax may misparse.
- `define_method`, heavy metaprogramming, and dynamically generated methods are
  invisible to the indexer — they aren't `def`.
- Unusual block constructs can confuse the extractor's nesting tracker.

This is a deliberate tradeoff (no Ruby toolchain dependency, works on any file
instantly), not an oversight — but it means RTS's view of a file is an
approximation, and a sufficiently strange file will defeat it.

### The Bash hook is a heuristic and can be bypassed

The hook splits a command on unquoted `|`, `;`, `&&`, `||` and checks whether
any segment starts with a reading verb (`cat`, `head`, `tail`, `sed`, `awk`,
`less`, `more`, `nl`, `bat`, `view`) and names a path under cwd. It catches
ordinary usage, including the `\cat` alias-bypass trick.

It does **not** reliably catch:

- Command substitution — `$(cat file)`
- Heredocs
- `sudo cat`
- Anything sufficiently indirect (a script that reads the file, `xargs`, …)

The hook deliberately errs toward over-blocking rather than under-blocking,
because a silent bypass defeats its purpose. It is a guardrail, not a sandbox.

### Exploration subagents may not use RTS

Claude Code's built-in Explore and Plan subagents skip `CLAUDE.md` by design and
may not inherit RTS's MCP tools. This is upstream Claude Code behavior — see
anthropic/claude-code issues **#30280** and **#13605** — and is **not fixable
from RTS's side.**

Partial mitigation: the Bash hook still fires at the harness level, so the
worst case (a subagent `cat`-ing a huge file) is contained. But a subagent may
reach for `grep` instead of `find_symbol`/`search_text`, and RTS can't stop it.

Treat this as an **accepted limitation**.

### The Bash hook must stay project-scoped

RTS's Bash-blocking hook is installed per project (`.claude/settings.local.json`)
by design, and must stay that way. rtk's hook is global on the same `Bash`
matcher. Wiring RTS's hook globally would put two `PreToolUse` hooks in
contention on every Bash call in every project — one rewriting `cat` toward
`rtk read`, the other denying it toward `view_file`. Keep RTS's hook scoped to
the projects it indexes.

### `RTS_DISABLE=1` has to be set where the process can see it

The MCP config launches the server with `env: {}`, which strips the inherited
environment. Exporting `RTS_DISABLE=1` in your shell will **not** reach the
server. Set it inside the MCP config's `env` block.

(The same `env: {}` stripping is why `tool.findRipgrep()` falls back to
`os.Stat` on known absolute paths after `exec.LookPath` fails — there's no PATH
to look up.)

When the flag is set, every MCP tool returns an error naming the native
alternative, and the hook allows everything. Both halves are required: disabled
tools plus a blocking hook would leave Claude Code with no way to read a file
at all.

---

## The frontier

The most useful thing this document can say is where the remaining money is.

Per real session data, **the largest token cost RTS does not touch is Bash and
tool output** — test runs, migration logs, lint, stack traces. In
execution-heavy sessions it dominates everything RTS *does* optimize.

That cost is already handled, though — by `rtk`, a Rust
output-filter proxy that runs as a global `PreToolUse` hook on `Bash`
(`rtk hook claude` in `~/.claude/settings.json`) and has roughly 70
command-specific filters. Those already cover the Ruby commands RTS cares about
(`rspec`, `rake`/Minitest, `rubocop`), plus linters, build tools, VCS, and
generic error and log output. An `rts run` equivalent would mean re-implementing
those filters to reach parity with something `brew upgrade` keeps current. RTS
should not duplicate it.

What RTS has that rtk does not is **structure-awareness**: skeleton views,
method and Minitest-test extraction by name, and the `.rts/symbols.json`
symbol index. None of rtk's subcommands understand code structure — `rtk read`
is line-based truncation (`--level`, `--max-lines`, `--tail-lines`), and
`find_symbol` is a different kind of thing from `rtk grep`. That is the part
worth investing in.

For **Rails-specific** command output rtk does not yet cover (`rails runner`
diagnostics, migration logs), the right move is to extend rtk with its
project-local TOML filters (`rtk trust` / `rtk verify`) — days of work and no
hook collision — rather than building it into RTS.

One caveat on the numbers above: the ~7%-of-session figure in *Measured value*
was most likely measured while rtk was already filtering Bash output. That 7%
is a share of an already-discounted bill, so RTS's share of what remains is
larger than the raw figure suggests. Read it as "reading was a small slice of
that session", not as an indictment of RTS.

The second-largest cost was **rediscovery across sessions**, and that one is
now addressed — see *Cross-session memory* above. It is worth saying what is
still unmeasured about it: the savings from a session that starts oriented are
real but **not instrumented**. `get_stats` reports chars saved against a naive
full read, and there is no honest baseline for "the exploration this session
did not have to do." Do not quote a number for it. The claim RTS can defend is
narrower and still worth making: the orientation now costs a few hundred
tokens of injected context, once, instead of a directory walk and several
exploratory reads, every time.
