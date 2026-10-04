# kx design notes

## Problem

Several clients, each with a few clusters. Projects come and go. Day-to-day
work happens in Lens and k9s, which read `~/.kube/config`. Editing that file by
hand got old: merging configs people send, untangling `kubernetes`/`admin`
name clashes, cleaning up after clients who left.

## Idea

Keep the clusters in a store of their own and treat `~/.kube/config` as a
generated file.

```
~/.config/kx/
  clusters/
    acme/
      prod.yaml        # self-contained kubeconfig: 1 cluster + 1 user + 1 context
      stage.yaml
    globex/
      main.yaml
  state.yaml           # what is turned off, plus what the last build generated
~/.local/state/kx/
  backups/             # the last 10 versions of ~/.kube/config
  exec/                # kx-<pid>.yaml for each running kx exec
~/.cache/kx/
  checks.json          # last check results

~/.kube/config         # built from the clusters that are on

KX_HOME puts all three in one directory instead.
```

- The main unit is the **client**. A cluster is addressed as `client/cluster`.
- Inside each file the context, the cluster and the user all share one name,
  `acme/prod`, so merging can't produce clashes.
- Every file in the store is self-contained: certificates referenced as files
  are inlined (`*-data`) on import, so the downloaded kubeconfig can be thrown
  away. Each file works on its own:
  `KUBECONFIG=~/.config/kx/clusters/acme/prod.yaml kubectl ...`.
- A cluster that is turned off stays in the store but is left out of
  `~/.kube/config`, so Lens and k9s don't see it.
- A whole client can be turned off too.

## Commands

```
kx add <file|-> -c <client> [--name N] [--context C]... [--force] [--check]
kx ls [client] [--json]
kx use [client/cluster]
kx ns [client/cluster] [namespace]
kx on  <client|client/cluster>...
kx off <client|client/cluster>...
kx rm  <client|client/cluster>... [-y]
kx mv  <from> <to>
kx export <client|client/cluster>...
kx exec <client|client/cluster>... -- <command> [args...]
kx check [client|client/cluster]... [--all] [--json] [--timeout 10s]
kx import-current [-c unsorted]
kx sync
kx build [--force]
```

| command | what it does |
|---|---|
| `add` | Splits the input kubeconfig into contexts, renames them to `client/<name>`, inlines files and puts them in the store. `--context` takes only the named ones. `--name` sets the cluster name (single context only). By default the name is the original context name, sanitized (for an EKS ARN, the part after the last `/`). `--force` overwrites an existing cluster. Before anything is written, every context must have an http(s) server address and credentials (token, token file, client certificate with key, exec or auth-provider plugin, basic auth); otherwise the whole add fails and says what is missing. `--check` probes the added clusters right after (like `check`, one line each) and exits 1 if a check fails; the clusters stay added. |
| `ls` | Table of `CLIENT CLUSTER SERVER NAMESPACE VERSION STATE`, `*` marks the current context. In a pipe every line has the same fields: a `CURRENT` column with `*` or `-` comes first and empty cells are `-`. VERSION comes from the last check. `--json` for scripts and agents, no credentials. |
| `use` | Sets `current-context`, the cluster kubectl, helm and k9s talk to by default. Without an argument prints the current one. Only that one field is rewritten, with no rebuild and no backup, so switching back and forth doesn't push useful backups out. A cluster that is off can't be made current; turning off or removing the current cluster clears `current-context`. |
| `ns` | Prints or sets the default namespace: of the current context, or of the named cluster (even one that is off). Written to the target and to the store, rewriting only that field, with no rebuild and no backup. The name is checked against Kubernetes rules (DNS-1123 label) but not looked up in the cluster, so it works offline. |
| `on`/`off` | Turn on or off. `on acme/prod` while `acme` is off turns on prod only. `off acme` remembers which of its clusters were off already, and `on acme` brings that back; `on acme` when acme as a whole wasn't off turns every cluster on. For a client the output counts its clusters and names the ones still off. Every argument is checked first, and if `~/.kube/config` can't be written the state is put back as it was. |
| `rm` | Remove. Always asks first, one cluster or many: once a cluster is off, the store holds the only copy of its credentials. `-y` skips the question. Without a terminal there's no question at all: kx exits 2 and asks for `-y`. |
| `mv` | `mv acme/old acme/new` renames a cluster, `mv unsorted/x acme` moves it to a client, `mv acme acme-corp` renames a client. |
| `export` | Prints a kubeconfig with the given clusters to stdout, including ones that are off. |
| `exec` | Runs a command with `KUBECONFIG` pointing at a temporary file holding just the given clusters (including ones that are off). The first one is current. Sets `KX_SCOPE`. Exits with the command's exit code (127 if it isn't found, 126 if it can't be run). The temporary file is `exec/kx-<pid>.yaml` in the state dir, removed afterwards; one left by a kx killed with -9 is removed by the next `exec`. |
| `check` | For each cluster, in parallel: `/version` (reachable, version), then `SelfSubjectReview` (credentials work, as whom; `GET /api` before 1.28), then `/readyz?verbose` (HEALTH `ok`/`degraded` plus what failed; `/healthz` before 1.16), then nodes (NODES `ready/total`, using the Table rendering, a few hundred bytes per node). Client certificate and JWT expiry is read locally. No permission for readyz or nodes leaves the column empty. Checks the clusters that are on by default. Exits 1 if any cluster is down or degraded. Errors, failed readyz checks and outdated versions are listed under the table. Results are saved to `checks.json`. A terminal gets "checking N clusters…" on stderr while it waits; Ctrl-C stops it with exit 130. |
| `import-current` | Takes over every context in `~/.kube/config` that isn't in the store, under client `-c` (`unsorted` by default). Sort them out with `mv` afterwards. |
| `sync` | Takes hand edits of `~/.kube/config` into the store (see below). Every command does this first; `sync` runs it alone. |
| `build` | Rebuilds `~/.kube/config`. Every command that changes something does this on its own. |

## Exit codes

0 done; 1 the command failed (a cluster down or degraded in `check`, a write
error, a "no" to a question); 2 kx was called wrong (unknown command or flag,
missing argument, invalid name or namespace, or a confirmation that can't be
asked because stdin is not a terminal). `exec` passes the child's code through. Usage errors
end with a pointer to the command's `--help`; a typo in a command name gets a
"did you mean"; `-h` anywhere on the line shows help even next to a bad flag.

Commands that change something say what changed, including side effects: a
cleared current-context, a cluster turned off by moving it into a disabled
client, foreign contexts dropped by `build --force` (with the backup path).
Prompts only happen on a terminal, and are checked for before the question is
printed.

## Output

- On a terminal: color (basic ANSI colors, so they follow the terminal theme),
  `ls` groups clusters by client, and tables are fitted to the window width:
  1. secondary values (SERVER, USER) are cut with `…`, by a third at most;
  2. if that's not enough, secondary columns are hidden (NAMESPACE; LATENCY,
     then VERSION, then USER);
  3. if even that's not enough, everything is cut down to its minimum width.
     Cluster names go last.
  `check` errors are printed under the table, word-wrapped and indented.
- In a pipe, without a TTY or with `NO_COLOR`: a flat table with no color and
  no truncation. `ls` repeats the client on every line so grep works. Machines
  should use `--json`.
- The width is read when the output is printed. Once printed, the terminal
  rewraps lines on resize; only the full-screen mode can relayout live.

## Not losing other people's contexts

`aws eks update-kubeconfig`, `yc managed-kubernetes cluster get-credentials`
and similar tools write straight into `~/.kube/config`. So that a rebuild
doesn't silently wipe their work:

- `state.yaml` keeps the list of contexts the last build generated;
- before any change kx checks `~/.kube/config` for contexts that are neither
  in the store nor on that list;
- if there are any, it stops and suggests `kx import-current -c <client>` or
  `kx build --force` (drop them).

Before every rebuild of `~/.kube/config` the old version goes to `backups/`
(`use` and `ns` change one field and make none).
Writes are atomic (temp file plus rename) with mode 0600. If the content
didn't change, the file isn't touched.

`current-context` survives a rebuild as long as that context still exists.

## Hand edits of the target

The store is kx's own; `~/.kube/config` is where people and other tools
make changes. kx treats such a change as deliberate and takes it in, instead
of undoing it on the next rebuild:

- a managed context whose cluster, credentials or namespace differ from the
  store replaces the stored copy (the context is re-extracted from the target,
  so file references are inlined as on import). Cluster and credential changes
  are reported; namespace switches (k9s, kubens) are taken silently;
- a managed context that the last build wrote but that is gone now is turned
  off, not removed, and reported. `kx on` brings it back;
- if the whole file is gone, or none of several contexts the last build wrote
  is left in it (emptied by a crashed editor, `>` instead of `>>`), that's a
  reset rather than a list of deletions: nothing is turned off and the next
  build writes everything again. A single context gone is a deletion, even
  when it was the only one: that's what removing the last cluster in Lens
  looks like;
- contexts kx doesn't manage are left alone (see above).

This runs once before every command except a `--dry-run` (a failure is only a
warning, given after the command, so a broken file doesn't block
`kx build --force`), at the start of the interactive mode, and
whenever the interactive mode sees the file change (it looks every two
seconds). `kx sync` runs it on its own.

## Interactive mode

`kx` with no arguments on a terminal (or `kx ui`) opens a full-screen
bubbletea UI. In a pipe, plain `kx` prints help.

The main use case is "someone sent a config, I drop it in, Lens picks it up":

- `p` reads a kubeconfig from the clipboard (pbpaste, wl-paste, xclip or
  xsel), then asks for the client (the selected one is prefilled). A config
  pasted in chat goes in with two key presses.
- `a` reads a kubeconfig from a file. The file can be dragged from Finder into
  the terminal: quotes and escaped spaces in the path are handled.
- After `p` or `a` the new clusters are checked right away and the cursor
  moves to the first one.
- `enter` makes a cluster current (`current-context`, marked `*`), `n` sets
  its default namespace.
- `space` turns a cluster or a whole client on or off, `r` renames, `d`
  deletes (asks first), `i` imports foreign contexts from `~/.kube/config`.
- Clusters are shown as a client/cluster tree; `/` filters by name and server.
- `c` checks the clusters on screen. Statuses show up as clusters answer; the
  details of the selected one (status, error, version, user, expiry, server)
  are on the line at the bottom.
- The layout follows window resizes live, using the same rules as `ls` and
  `check`.
- Actions call the same code as the CLI, so foreign-context protection,
  backups and syncing hand edits behave the same. Hand edits made while the
  view is open are picked up within a couple of seconds and shown on the
  status line.
- While the UI is open, `os.Stderr` points at `/dev/null`: exec auth plugins
  write there directly and would garble the screen.

## Cluster state

- The last check results live in `~/.cache/kx/checks.json`. `ls` takes
  VERSION from there; the interactive mode shows version and status on start
  (muted, with "checked 2h ago" on the details line) without asking the
  clusters again. Removed clusters are dropped from the cache on the next
  save.
- Outdated versions: upstream supports the three latest minor releases. With
  no network lookup, "latest" means the newest version among your own
  clusters; anything three or more minors behind gets a `!` and an explanation
  under the table. With a single cluster the flag never fires, on purpose.

## Agents

An agent doesn't need the whole zoo: with `kx exec acme -- <agent>` it only
sees `acme` clusters and can't stumble into another client's production. This
guards against mistakes; it is not a sandbox, since `~/.kube/config` and the
store are still readable.

`kx ls --json` is there for parsing. The only interactive question is the
confirmation for removing several clusters: `-y` skips it, and with no answer
the command fails instead of hanging.

No MCP server for now. If one shows up, it won't have `export`: credentials
shouldn't end up in a model's context.

## Environment

- `KX_HOME`: the store (default `$XDG_CONFIG_HOME/kx` or `~/.config/kx`).
- `KX_KUBECONFIG`: the file to build (default `~/.kube/config`).

## Code

```
main.go              cobra: commands, flags, completion
internal/app/        kx commands (add, on/off, rm, mv, use, export, exec, check, build), shared by CLI and TUI
internal/store/      the store: clusters, state.yaml, kubeconfig parsing and merging, atomic writes
internal/probe/      cluster checks: version, whoami, readyz, nodes, credential expiry, version policy, cache
internal/table/      tables: color, fitting to width
internal/tui/        interactive mode
internal/kxtest/     test fixtures: environment, kubeconfig, fake API server
```

`main.go` sits at the root so that `go install github.com/eugene-panin/kx@latest`
produces a `kx` binary. CLI end-to-end tests are in `main_test.go`, the rest
live next to their packages.

## Stack

Go, `cobra`, `k8s.io/client-go/tools/clientcmd` (loading, resolving relative
paths, flattening, writing; exec plugins and other fields are carried over
as is), bubbletea, bubbles and lipgloss v2 for the interactive mode and
colors. v2 matters: v1 queried the terminal in `init()`, which delayed every
command, not just the interactive one.

## Global flags

`--no-color` (also `NO_COLOR`, `KX_NO_COLOR`; `FORCE_COLOR` forces color),
`--no-input` (never ask, fail where a question would be needed), `-q/--quiet`
(no status lines such as "added …", results and errors still print).
`rm` and `build` take `--dry-run`. In a pipe, tables print one record per line
and empty cells as `-`, so `awk` fields don't shift.

Deprecated, still working with a warning until v1: `kx add -n` (use `--name`;
`-n` is the conventional `--dry-run`) and `kx -v` (use `--version`).

## Deviations from clig.dev

| rule | what kx does | why |
|---|---|---|
| O8 | read-only commands (`ls`, `export`, `check`) run the sync with `~/.kube/config` first, which can update the store | hand edits must never be lost, and a read that showed stale data would be worse; documented in `kx sync --help` |
| A9 | questions are skipped with `-y/--yes`, not `-f/--force` | `--force` already means "overwrite or drop" in `add` and `build`, which is a different promise from "don't ask" |
| D3 | no man pages | the terminal docs are `kx help <command>`, generated from the same code; the README is the web doc |
| N1 | the name `kx` is short enough to collide with a common kubectx alias | the owner chose it; an alias shadows the binary only where one is set |
| G1 | outside `check`, Ctrl-C ends kx at once without a message | everything else finishes in milliseconds and writes atomically, so there is nothing to clean up or report |

## Not done yet

- A separate current context per shell (`kx exec <client> -- $SHELL` covers
  part of it).
- Importing straight from clouds (`kx sync aws|yc|do`).
