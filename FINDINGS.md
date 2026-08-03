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
