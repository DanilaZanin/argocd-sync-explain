# argocd-sync-explain

A read-only command that says what an Argo CD Application is waiting for, or why it failed or is out of sync.
It names the resource, hook or child Application, with the field it read that from. When the data is not there
it prints `unknown` and the reason instead of guessing.

This is a real stuck Application, produced on a kind cluster running Argo CD v3.5.3: a Deployment in wave 0
that never becomes ready, and a ConfigMap in wave 1 behind it.

```
$ argocd-sync-explain app ase-wave
<<WAVE_OUTPUT>>
```

Exit codes: `0` synced, healthy, nothing pending. `2` waiting, failed or out of sync (explained). `3` unknown
(not enough data). `1` tool error.

## Why

"Syncing" and "Progressing" do not say what Argo CD is waiting for, and the UI needs a lot of clicking through
waves, hooks and child Applications to find out. Users have asked for this in
[argo-cd#10561](https://github.com/argoproj/argo-cd/issues/10561),
[argo-cd#7437](https://github.com/argoproj/argo-cd/issues/7437) and
[a r/kubernetes thread](https://www.reddit.com/r/kubernetes/comments/1gzjsxm/).
`argocd app get`, `argocd app wait` and the UI show state; this tool reads the same state and explains the wait.

It is a diagnostic. It never changes an Application and never changes sync behaviour.

## Install

Download an archive from the releases page, or build from source:

```
go install github.com/DanilaZanin/argocd-sync-explain/cmd/argocd-sync-explain@latest
```

The release archives also contain the same binary as `kubectl-argocd_sync_explain`. Put it on your `PATH` and
run `kubectl argocd-sync-explain app NAME`.

## Two modes

**Argo CD API mode** (the main one). Needs a server address and a token, with the same sources the `argocd`
CLI uses: `--server` / `ARGOCD_SERVER`, `--auth-token` / `ARGOCD_AUTH_TOKEN`, or the CLI config file
(`~/.config/argocd/config`, `--argocd-config`, `--argocd-context`). `--insecure` and `--plaintext` behave as in
the CLI. `--port-forward` reaches `argocd-server` through your kubeconfig, like `argocd --port-forward`, and
still needs a token. The tool only sends GET requests: application, resource tree, project, events, version.

```
export ARGOCD_AUTH_TOKEN=...
argocd-sync-explain app my-app --port-forward
argocd-sync-explain app my-app --server argocd.example.com -o json
```

**Kubeconfig-only mode.** No Argo CD API needed: it reads the `Application` and `AppProject` custom resources
with your kubeconfig. Everything that is not stored in the custom resource is printed as `unknown (needs the
Argo CD API)`, never as healthy. This is the mode when no server is configured (`--mode kubeconfig` forces it).

```
$ argocd-sync-explain app ase-wave --mode kubeconfig
<<KUBECONFIG_OUTPUT>>
```

Flags: `-n/--namespace` (Application namespace, default `argocd`), `--argocd-namespace` (where Argo CD runs, for
AppProjects), `-o text|json`, `--kubeconfig`, `--context`, `--timeout`.

## What it can and cannot know

| Fact | Where it comes from | Limit |
|---|---|---|
| Operation phase, message, retry count, revisions | `status.operationState` | exact |
| Tasks the operation is waiting on | `operationState.message` (the controller names the first task and counts the rest) and `syncResult.resources[].hookPhase` | exact for the first task; the rest are matched by phase and wave |
| Current sync phase and wave | reconstructed from running tasks and `status.resources[].syncWave` | **Argo CD publishes no phase or wave cursor**. The tool prints `reconstructed from: ...` and `unknown` when the basis is missing. Hook waves are manifest annotations and are not stored in the Application |
| Health of a resource | resource tree (API mode) | **Since Argo CD 3.0 per-resource health is not stored in the Application** (`status.resourceHealthSource: appTree`). Without the API it is `unknown`. Absence of health is never read as Healthy |
| Pods and ReplicaSets under a waiting Deployment | resource tree | shown as observations. The blocker is the resource the operation waits on, not its descendants, and the tool does not re-implement Argo CD health |
| Why OutOfSync | `status.resources` | a list of resources by status, no field-level diff (use `argocd app diff`). `OutOfSync` means missing or modified; the tool says which only when the tree shows `Missing` |
| Sync windows | `AppProject.spec.syncWindows` | computed for now with the rules of the server version (3.5 has `syncOverrun`, 3.4 does not), including timezone, selectors, `andOperator`, `manualSync`, deny priority and the missing-allow-window case |
| Auto-sync | `spec.syncPolicy`, `status` | two lists: verified constraints, and possible reasons the snapshot cannot prove (controller timings, self-heal backoff) |
| ApplicationSet / RollingSync | not read | only a note when `ownerReferences` show an ApplicationSet. `appset` explain is planned for v0.2 |

### Classification

The tool keeps three situations apart:

- **waiting**: the operation is Running and holds on tasks (running hooks, resources of an unfinished wave that
  are not Healthy while later tasks are pending, a sync window, a prune confirmation).
- **failed**: the operation ended Failed or Error. The failing task and message are listed. A failed dry-run
  ends the operation before SyncFail hooks run, and the tool says so.
- **progressing**: the operation finished Succeeded and the rollout is still Progressing. This is not a blocked
  sync: Argo CD does not hold the last wave of ordinary resources for their health.

Operation kinds: `queued` (`.operation` set, the controller has not started it), `running`, `retry-backoff`
(phase Running with `finishedAt` set; the retry policy is read from `operationState.operation.retry`, `limit: -1`
is unlimited, the next attempt is the earliest time), `terminating`, `finished`.

Hooks are identified by `hookType`. A hook that finished and was deleted by its delete policy is history, not a
missing blocker.

App-of-apps: a parent waits for a child Application only if Argo CD assesses Application health (a custom
Lua check in `argocd-cm`; the built-in check was removed in Argo CD 1.8). The tool shows which case applies with
the evidence: `parent-waits`, `health-assessed`, `parent-does-not-wait` (no health is assessed on this
installation), or `unknown`. It then explains each child independently.

## Supported Argo CD versions

| Argo CD | Kubernetes in e2e | Notes |
|---|---|---|
| v3.5.3 (current) | v1.35.8 | sync window `syncOverrun` rules |
| v3.4.9 (previous minor) | v1.35.8 | no `syncOverrun` |

Both versions list Kubernetes v1.35 in the Argo CD tested-versions table. The tool reads a minimal set of fields
(`internal/model`), checked against the Application CRD of both versions in a test.

## How it was verified

<<VERIFIED>>

## Develop

```
make vet lint
make test GOTESTFLAGS="-race -count=1"
make build
make e2e         # small scenario set, current Argo CD, one kind cluster
make e2e-full    # every scenario on both Argo CD versions, one clean cluster each
make snapshot    # goreleaser snapshot build
```

`make e2e` needs `docker`, `kind` and `kubectl`. It only creates and deletes clusters named `ase-*`.

## License

MIT, Copyright (c) 2026 Danila Zanin.
