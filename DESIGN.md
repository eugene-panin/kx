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
  checks.json          # last check results
  backups/             # the last 10 versions of ~/.kube/config

~/.kube/config         # built from the clusters that are on
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
kx add <file|-> -c <client> [--name N] [--context C]... [--force]
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
kx build [--force]
```

| command | what it does |
|---|---|
| `add` | Splits the input kubeconfig into contexts, renames them to `client/<name>`, inlines files and puts them in the store. `--context` takes only the named ones. `--name` sets the cluster name (single context only). By default the name is the original context name, sanitized (for an EKS ARN, the part after the last `/`). `--force` overwrites an existing cluster. |
| `ls` | Table of `CLIENT CLUSTER SERVER NAMESPACE VERSION STATE`, `*` marks the current context. VERSION comes from the last check. `--json` for scripts and agents, no credentials. |
| `use` | Sets `current-context`, the cluster kubectl, helm and k9s talk to by default. Without an argument prints the current one. Only that one field is rewritten, with no rebuild and no backup, so switching back and forth doesn't push useful backups out. A cluster that is off can't be made current; turning off or removing the current cluster clears `current-context`. |
| `ns` | Prints or sets the default namespace: of the current context, or of the named cluster (even one that is off). Written to the target and to the store, rewriting only that field, with no rebuild and no backup. The name is checked against Kubernetes rules (DNS-1123 label) but not looked up in the cluster, so it works offline. |
| `on`/`off` | Turn on or off. `on acme/prod` while `acme` is off turns on prod only. |
| `rm` | Remove. Removing more than one cluster asks first (`-y` skips the question). |
| `mv` | `mv acme/old acme/new` renames a cluster, `mv unsorted/x acme` moves it to a client, `mv acme acme-corp` renames a client. |
| `export` | Prints a kubeconfig with the given clusters to stdout, including ones that are off. |
| `exec` | Runs a command with `KUBECONFIG` pointing at a temporary file holding just the given clusters (including ones that are off). The first one is current. Sets `KX_SCOPE`. Exits with the command's exit code. The temporary file is removed afterwards. |
| `check` | For each cluster, in parallel: `/version` (reachable, version), then `SelfSubjectReview` (credentials work, as whom; `GET /api` before 1.28), then `/readyz?verbose` (HEALTH `ok`/`degraded` plus what failed; `/healthz` before 1.16), then nodes (NODES `ready/total`, using the Table rendering, a few hundred bytes per node). Client certificate and JWT expiry is read locally. No permission for readyz or nodes leaves the column empty. Checks the clusters that are on by default. Exits 1 if any cluster is down or degraded. Errors, failed readyz checks and outdated versions are listed under the table. Results are saved to `checks.json`. |
| `import-current` | Takes over every context in `~/.kube/config` that isn't in the store, under client `-c` (`unsorted` by default). Sort them out with `mv` afterwards. |
| `build` | Rebuilds `~/.kube/config`. Every command that changes something does this on its own. |

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

Before every rewrite of `~/.kube/config` the old version goes to `backups/`.
Writes are atomic (temp file plus rename) with mode 0600. If the content
didn't change, the file isn't touched.

`current-context` survives a rebuild as long as that context still exists.

A namespace switched in `~/.kube/config` by other tools (k9s, kubens,
`kubectl config set-context`) is copied back into the store before every
change, so it survives rebuilds and `off`/`on`.

## Interactive mode

`kx` with no arguments on a terminal (or `kx ui`) opens a full-screen
bubbletea UI. In a pipe, plain `kx` prints help.

The main use case is "someone sent a config, I drop it in, Lens picks it up":

- `p` reads a kubeconfig from the clipboard (pbpaste, wl-paste, xclip or
  xsel), then asks for the client (the selected one is prefilled). A config
  pasted in chat goes in with two key presses.
- `a` reads a kubeconfig from a file. The file can be dragged from Finder into
  the terminal: quotes and escaped spaces in the path are handled.
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
  backups and namespace sync behave the same.
- While the UI is open, `os.Stderr` points at `/dev/null`: exec auth plugins
  write there directly and would garble the screen.

## Cluster state

- The last check results live in `~/.config/kx/checks.json`. `ls` takes
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
as is), bubbletea and lipgloss for the interactive mode and colors.

## Not done yet

- A separate current context per shell (`kx exec <client> -- $SHELL` covers
  part of it).
- Importing straight from clouds (`kx sync aws|yc|do`).
