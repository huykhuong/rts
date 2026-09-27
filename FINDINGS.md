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
- **The map describes a Ruby project.** `rts index` parses `.rb` only. In a Go
  or Node repo the map still reports layout, entry points and large files, and
  the brief stops advertising the Ruby-only tools — but the symbol counts that
  rank everything will be zero.

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

Per real session data, **the largest untouched token cost is Bash and tool
output** — test runs, migration logs, lint, stack traces. RTS currently does
nothing about any of it, and in execution-heavy sessions it dominates
everything RTS *does* optimize.

Compressing or summarizing command output is the most promising unexplored
direction for this tool. Everything on the file-reading side is incremental
tuning by comparison.

The second-largest cost was **rediscovery across sessions**, and that one is
now addressed — see *Cross-session memory* above. It is worth saying what is
still unmeasured about it: the savings from a session that starts oriented are
real but **not instrumented**. `get_stats` reports chars saved against a naive
full read, and there is no honest baseline for "the exploration this session
did not have to do." Do not quote a number for it. The claim RTS can defend is
narrower and still worth making: the orientation now costs a few hundred
tokens of injected context, once, instead of a directory walk and several
exploratory reads, every time.
