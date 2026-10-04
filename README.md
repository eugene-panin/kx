# kx

I work with several clients, each with two or three Kubernetes clusters, plus
short projects that show up for a month and then go away. I got tired of
editing `~/.kube/config` by hand: merging configs people send me, figuring out
why three clusters all have a user called `kubernetes-admin`, cleaning up after
projects that ended. kx does that for me.

Each cluster is kept as its own file in `~/.config/kx`, grouped by client, and
`~/.kube/config` is built from the clusters that are currently turned on. Lens,
k9s, kubectl and helm don't know kx exists. They just read the usual config.

## Install

With Homebrew (macOS and Linux):

```bash
brew install eugene-panin/tap/kx
```

This also sets up shell completion for cluster names.

With Go 1.26 or newer:

```bash
go install github.com/eugene-panin/kx@latest
```

Or from a checkout: `go install .` in the repo root. The binary ends up in
`$(go env GOPATH)/bin`, usually `~/go/bin`. Add it to your `PATH` if it isn't
there yet.

Shell completion when installed with Go (zsh):

```bash
kx completion zsh > "${fpath[1]}/_kx"
```

bash and fish work the same way, see `kx completion --help`.

## First run

If `~/.kube/config` already has clusters in it, hand them over to kx first:

```bash
kx import-current
```

Everything goes under a client called `unsorted`, and the old file is copied to
`~/.config/kx/backups`. Then sort things out:

```bash
kx mv unsorted/kubernetes-admin-kubernetes acme/prod
kx mv unsorted/shop acme
kx mv unsorted/gke_proj_europe-west1_main globex/main
```

`mv acme/x acme/y` renames a cluster, `mv unsorted/x acme` moves it to a client
and keeps the name, `mv acme acme-corp` renames the whole client.

## Day to day

The easiest way is to run `kx` with no arguments. That opens the interactive
view:

```
kx  4 clusters · 2 clients
      CLUSTER  SERVER                       NAMESPACE  VERSION  STATE  CHECK
  acme
   *  prod     https://10.0.0.1:6443        apps       v1.36.2  on     ok
      stage    https://10.0.0.2:6443                   v1.36.2  on     ok
  globex
      main     https://k8s.globex.io:6443   default    v1.33.4! off    unreachable
```

Someone pasted a kubeconfig in chat: copy it, press `p`, type the client name.
Someone sent a file: press `a` and drag the file from Finder into the terminal
window. kx checks the new cluster right away, so you see whether the config
actually works while the client is still around, and Lens picks it up on its
own.

| key | what it does |
|---|---|
| `p` | add a kubeconfig from the clipboard |
| `a` | add a kubeconfig from a file |
| `space` | turn a cluster on or off; on a client line, all of its clusters |
| `enter` | make the cluster current (`current-context`) |
| `n` | set the cluster's default namespace |
| `c` | check the clusters on screen |
| `r` | rename |
| `d` | delete, asks first |
| `/` | filter by name or server, `esc` clears it |
| `i` | take over contexts someone added to `~/.kube/config` behind kx's back |
| `q` | quit |

Everything is also available as plain commands.

Add:

```bash
kx add ~/Downloads/kubeconfig.yaml -c acme
pbpaste | kx add - -c acme --name prod
kx add - -c acme          # paste the config into the terminal, then Ctrl-D
kx add big.yaml -c acme --context ctx-a --context ctx-b
```

Nothing is written until every context in the input looks usable: it needs an
http(s) server address and some credentials (a token, a client certificate
with its key, an exec plugin and so on). Otherwise kx says what is missing and
in which context.

Add `--check` to talk to the new clusters straight away:

```
$ pbpaste | kx add - -c acme --name prod --check
added acme/prod  https://10.0.0.1:6443
check acme/prod  ok  v1.36.2  kubernetes-admin
```

If the check fails the cluster stays added and the exit code is 1.

If the file has several contexts, all of them are added and named after the
original contexts (for EKS only the part after the last `/` is kept).
Certificates the config points to on disk are copied into it, so you can delete
the downloaded file afterwards.

List:

```bash
kx ls
kx ls acme
```

A project is on hold, then comes back:

```bash
kx off globex
kx on globex
kx off acme/stage
```

A cluster that is off stays in `~/.config/kx` but disappears from
`~/.kube/config`, so Lens and k9s stop showing it.

A project is over:

```bash
kx rm globex        # asks first, -y to skip the question
kx rm acme/stage
```

Pick the cluster kubectl and helm use by default:

```bash
kx use acme/prod
kx use              # show the current one
```

Default namespace, so you don't have to type `-n` every time:

```bash
kx ns monitoring             # for the current cluster
kx ns acme/prod monitoring   # for a given one, even if it's off
kx ns                        # show it
```

kx doesn't ask the cluster whether that namespace exists, so this works
offline. The namespace sticks when you turn the cluster off and on again.

Send a config to a colleague or back to the client:

```bash
kx export acme/prod > acme-prod.yaml
```

## Checking on everything

```bash
kx check
```

```
CLUSTER        STATUS       HEALTH    VERSION   NODES  USER                    EXPIRES  LATENCY
acme/prod      ok           ok        v1.36.2   3/3    kubernetes-admin        212d     180ms
acme/stage     ok           degraded  v1.36.2   2/3    kubernetes-admin        12d!     240ms
globex/main    unreachable                                                              5000ms

acme/stage
  readyz failing: etcd
globex/main
  timeout
```

For every cluster kx checks that the API answers, that the credentials still
work and which user they map to, what `/readyz` says, and how many nodes are
Ready. When the client certificate or token expires is read from the config
itself, so you see it even for clusters that are down. `!` means less than 30
days left.

A version also gets a `!` when it is three or more minor releases behind the
newest one among your clusters. Upstream doesn't support it anymore.

If you're not allowed to read readyz or list nodes (common for a service
account that only got one namespace), those columns stay empty.

By default only clusters that are on get checked. `kx check --all` checks
everything, `kx check acme` just one client. The exit code is 1 if any cluster
is down or degraded, so you can run it from cron.

Results are saved. `kx ls` and the interactive view show the last known
version and status without asking the clusters again.

## What kx is careful about

kx owns `~/.kube/config` and rebuilds it from scratch, but:

- If something was written there without kx (`aws eks update-kubeconfig`,
  `yc managed-kubernetes cluster get-credentials` and so on), kx refuses to
  rebuild and tells you to run `kx import-current -c <client>`. It won't drop
  those contexts silently. `kx build --force` drops them on purpose.
- Before every rewrite the old file is copied to `~/.config/kx/backups`. The
  last ten are kept.
- If you edit `~/.kube/config` by hand, kx takes your edits instead of
  undoing them. Changed a token, a certificate or a server address of a
  cluster kx manages: kx copies it into its store and tells you so. Deleted
  one of its contexts: kx turns that cluster off (not removes it, `kx on`
  brings it back). This happens before every kx command, and the interactive
  view notices edits while it's open. `kx sync` runs it on its own.
- A namespace you switched in k9s or with kubens is not reset on the next
  rebuild.
- The current context is kept. It is only cleared when that cluster is turned
  off or removed, so kubectl doesn't end up talking to some random cluster.

## Scripts and agents

`kx ls --json` and `kx check --json` print the same data as JSON. No
credentials in there.

`kx exec` runs a command that can only see the clusters you name:

```bash
kx exec acme -- kubectl get nodes
kx exec acme/stage -- k9s
kx exec acme -- $SHELL
```

Inside, `KUBECONFIG` points to a temporary file with just those clusters, and
`KX_SCOPE` holds what you asked for (handy for a shell prompt). It's a good way
to run an agent or a script that must not wander into another client's cluster
by mistake. It protects against mistakes, not against a process that wants to
misbehave: it can still read `~/.kube/config` directly.

## Environment

| variable | default | |
|---|---|---|
| `KX_HOME` | `$XDG_CONFIG_HOME/kx` or `~/.config/kx` | where clusters, state and backups live |
| `KX_KUBECONFIG` | `~/.kube/config` | the file kx builds |
| `NO_COLOR` | | turn colors off |

## Storage

```
~/.config/kx/
  clusters/acme/prod.yaml   one file per cluster, each one works on its own
  state.yaml                what is turned off
  checks.json               last check results
  backups/                  previous versions of ~/.kube/config
```

Any file under `clusters/` can be used directly:
`KUBECONFIG=~/.config/kx/clusters/acme/prod.yaml kubectl get pods`.

Design notes are in [DESIGN.md](DESIGN.md).

## License

MIT, see [LICENSE](LICENSE).
