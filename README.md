# argocd-sync-explain

A read-only command that says what an Argo CD Application is waiting for, or why it failed or is out of sync.
It names the resource, hook or child Application, with the field it read that from. When the data is not there
it prints `unknown` and the reason instead of guessing.

This is a real stuck app-of-apps, produced on a kind cluster running Argo CD v3.5.3: a parent Application whose
wave 0 is a child Application that never becomes healthy, with a ConfigMap in wave 1 behind it. Argo CD assesses
Application health here, so the parent waits for the child. The tool names the child as the blocker, says why
it believes the parent waits (the evidence), and then explains the child on its own.

```
$ argocd-sync-explain app ase-lua-parent
argocd/ase-lua-parent  sync=OutOfSync health=Progressing  project=default
mode: api (Argo CD v3.5.3)

VERDICT: WAITING
  The sync operation is running and waits on: child Application ase-lua-child.

Operation: running (phase=Running, automated sync, started 2026-10-03T08:50:25Z)
  message: waiting for healthy state of argoproj.io/Application/ase-lua-child
  position (reconstructed): phase=Sync wave=0
    basis: reconstructed from .status.operationState.syncResult.resources (syncPhase of tasks with hookPhase=Running) and .status.resources[].syncWave; Argo CD publishes no phase/wave cursor

Blockers:
  [waiting-on] child-application Application/ase-lua-child (ns argocd)
      the operation applied this child Application and waits for its health to become Healthy before starting later tasks
      syncPhase=Sync hookType=- hookPhase=Running wave=0
      health: Progressing  [resource tree node health]
      message: application.argoproj.io/ase-lua-child created
      source: .status.operationState.syncResult.resources

Pending (no result yet, 1): ConfigMap/after-lua  waves=[1]
    resources without a result yet; prune tasks run in reverse wave order, PruneLast adds a final wave, and hooks are not listed with waves, so this is a reconstruction, not the controller's task list

OutOfSync resources:
  ConfigMap/after-lua (ns argocd)  missing: yes (resource health is Missing)

Auto-sync (enabled=true):
  verified: automated sync is enabled (an `automated` block without `enabled` means enabled)  [.spec.syncPolicy.automated]
  verified: an operation is already in progress; auto-sync does not start another  [.operation / .status.operationState.phase]

Notes:
  field-level differences are not shown; run `argocd app diff ase-lua-parent`  [-]

Child Application argocd/ase-lua-child: parent-waits
  the parent's running operation waits on this child Application (task hookPhase=Running) and its health is assessed: Progressing
  explained independently:
    | argocd/ase-lua-child  sync=Synced health=Progressing  project=default
    | 
    | VERDICT: PROGRESSING
    |   The last sync operation finished successfully; the rollout is still Progressing. That is not a blocked sync: the operation does not hold for the health of the last wave.
    | 
    | Operation: finished (phase=Succeeded, automated sync, started 2026-10-03T08:50:25Z, finished 2026-10-03T08:50:26Z)
    |   message: successfully synced (all tasks run)
    |   retry: limit=5, duration=5s factor=2 maxDuration=3m0s (.status.operationState.operation.retry)
    | 
    | Observations (context, not blockers):
    |   under application health: Deployment/web (ns ase-lua-child) health=Progressing: Waiting for rollout to finish: 0 of 1 updated replicas are available...
    |   under Deployment/web: ReplicaSet/web-957bd8ddf (ns ase-lua-child) health=Progressing: Waiting for rollout to finish: 0 out of 1 new replicas are available...
    |   under Deployment/web: Pod/web-957bd8ddf-gmbnj (ns ase-lua-child) health=Progressing Status Reason=Running Containers=0/1
```

Every output in this README is the unedited text file written by the e2e run (`ASE_E2E_RECORD=dir make e2e-full`
stores the text output and the collected snapshot of each scenario), shortened only where marked with `...`.
The same snapshots feed the golden unit tests.

Exit codes: `0` synced, healthy, nothing pending. `2` waiting, failed or out of sync (explained). `3` unknown
(not enough data, or an Application this token or kubeconfig is not allowed to read). `1` tool error (including a rejected token, HTTP 401).

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

## More examples

A Deployment in wave 0 that never becomes ready, with a ConfigMap in wave 1 behind it (Argo CD v3.5.3):

```
$ argocd-sync-explain app ase-wave
argocd/ase-wave  sync=OutOfSync health=Progressing  project=default
mode: api (Argo CD v3.5.3)

VERDICT: WAITING
  The sync operation is running and waits on: Deployment/web.

Operation: running (phase=Running, automated sync, started 2026-10-03T08:47:59Z)
  message: waiting for healthy state of apps/Deployment/web
  position (reconstructed): phase=Sync wave=0
    basis: reconstructed from .status.operationState.syncResult.resources (syncPhase of tasks with hookPhase=Running) and .status.resources[].syncWave; Argo CD publishes no phase/wave cursor

Blockers:
  [waiting-on] resource Deployment/web (ns ase-wave)
      applied by the operation; Argo CD now waits for its health to become Healthy before starting later waves or phases
      syncPhase=Sync hookType=- hookPhase=Running wave=0
      health: Progressing: Waiting for rollout to finish: 0 of 1 updated replicas are available...  [resource tree node health]
      message: deployment.apps/web created
      source: .status.operationState.syncResult.resources

Pending (no result yet, 1): ConfigMap/later  waves=[1]
    resources without a result yet; prune tasks run in reverse wave order, PruneLast adds a final wave, and hooks are not listed with waves, so this is a reconstruction, not the controller's task list

Observations (context, not blockers):
  under Deployment/web: ReplicaSet/web-957bd8ddf (ns ase-wave) health=Progressing: Waiting for rollout to finish: 0 out of 1 new replicas are available...
  under Deployment/web: Pod/web-957bd8ddf-p5nmc (ns ase-wave) health=Progressing Status Reason=Running Containers=0/1

OutOfSync resources:
  ConfigMap/later (ns ase-wave)  missing: yes (resource health is Missing)

Auto-sync (enabled=true):
  verified: automated sync is enabled (an `automated` block without `enabled` means enabled)  [.spec.syncPolicy.automated]
  verified: an operation is already in progress; auto-sync does not start another  [.operation / .status.operationState.phase]

Notes:
  field-level differences are not shown; run `argocd app diff ase-wave`  [-]
```

An Application that is being deleted but still exists, because a managed resource holds a finalizer:

```
$ argocd-sync-explain app ase-deleting
argocd/ase-deleting  sync=Synced health=Healthy  project=default
mode: api (Argo CD v3.5.3)

VERDICT: DELETING
  The Application is being deleted (deletionTimestamp 2026-10-03T08:50:08Z) but still exists. Held by: ConfigMap/held.

Operation: finished (phase=Succeeded, automated sync, started 2026-10-03T08:50:07Z, finished 2026-10-03T08:50:08Z)
  message: successfully synced (all tasks run)
  retry: limit=0, duration=5s factor=2 maxDuration=3m0s (.status.operationState.operation.retry)

Blockers:
  [waiting-on] resource ConfigMap/held (ns ase-deleting)
      deletion of the Application waits for this resource, which has a deletionTimestamp and is held by finalizers: ase/hold
      source: live object .metadata.finalizers

Deletion (since 2026-10-03T08:50:08Z): cascade (default propagation): Argo CD deletes the managed resources and removes the finalizer only when none remain
  finalizers on the Application: resources-finalizer.argocd.argoproj.io
  remaining: ConfigMap/held (ns ase-deleting) being deleted since 2026-10-03T08:50:08Z, held by finalizers ase/hold  [live object]
```

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
with your kubeconfig. Events are read through the kubeconfig too. Everything else that is not stored in the custom resource is
printed as `unknown (needs the Argo CD API)`, never as healthy. This is the mode when no server is configured (`--mode kubeconfig` forces it).

```
$ argocd-sync-explain app ase-wave --mode kubeconfig
argocd/ase-wave  sync=OutOfSync health=Progressing  project=default
mode: kubeconfig (Argo CD v3.5.3)

VERDICT: WAITING
...
Blockers:
  [waiting-on] resource Deployment/web (ns ase-wave)
      applied by the operation; Argo CD now waits for its health to become Healthy before starting later waves or phases
      syncPhase=Sync hookType=- hookPhase=Running wave=0
      health: unknown: needs the Argo CD API (status.resourceHealthSource=appTree: health is not stored in the Application)  [-]
      message: deployment.apps/web created
      source: .status.operationState.syncResult.resources

Pending (no result yet, 1): ConfigMap/later  waves=[1]
    resources without a result yet; prune tasks run in reverse wave order, PruneLast adds a final wave, and hooks are not listed with waves, so this is a reconstruction, not the controller's task list

OutOfSync resources:
  ConfigMap/later (ns ase-wave)  missing: unknown (OutOfSync covers missing and modified; the Application does not say which: needs the Argo CD API (status.resourceHealthSource=appTree: health is not stored in the Application))

Auto-sync (enabled=true):
  verified: automated sync is enabled (an `automated` block without `enabled` means enabled)  [.spec.syncPolicy.automated]
  verified: an operation is already in progress; auto-sync does not start another  [.operation / .status.operationState.phase]

Notes:
  field-level differences are not shown; run `argocd app diff ase-wave`  [-]

Unknown (not guessed):
  events: unknown (needs the Argo CD API)
  tree: unknown (needs the Argo CD API (the resource tree is served by argocd-server, not stored in the Application))
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
| Deletion in progress | `metadata.deletionTimestamp`, finalizers, live managed objects (API mode) | verdict `deleting`: cascade or not, which resource still holds a finalizer. Without the API the remaining resources are `unknown` |
| Child Applications | the parent's `status.resources` | read up to 5 levels, only from the cluster Argo CD runs in. A deeper level, a cycle, or a parent whose destination is another cluster is reported as `unknown`, so a healthy-looking chain never hides it and exits 0 |
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

All numbers below were measured on a Mac (arm64, Docker through colima, 4 CPU and 8 GB shared with other work),
`make e2e-full`, one clean kind cluster per Argo CD version, Kubernetes v1.35.8 (kind node image pinned by
digest, Argo CD `install.yaml` pinned by tag and sha256, an in-cluster Gitea holds the fixture repositories).

| Argo CD | e2e scenarios passed | Wall time including cluster create and delete | Peak memory of the kind node container |
|---|---|---|---|
| v3.4.9 | 30 of 30 | 227 s | 1570 MiB |
| v3.5.3 | 30 of 30 | 224 s | 1404 MiB |

Every scenario is produced for real: a git commit, an Application, and the real controller doing what it does.
The assertions are on the JSON output (identity and classification of the blocker), after explicit waits on
Application states. The only fixed sleeps are 10 s twice, to prove that a window keeps auto-sync from starting,
and 5 s after changing `argocd-cm`. The scenarios: wave waiting on an unhealthy Deployment with later tasks pending, last wave Progressing
(not blocked), PreSync hook running, PreSync and PostSync hooks failed, hook deleted after success (not a
blocker), prune confirmation, deny window now, no active allow window, deny window holding a manual operation,
auto-sync absent and `enabled: false`, retry backoff and its timing, comparison error from an invalid manifest,
unreachable repository, missing namespace, app-of-apps without and with the Application health check (child
Progressing, child Degraded), kubeconfig-only fallback, a token that can read the parent but not the child
Application or the project (and an unreadable root Application gives exit 3), a revision mismatch where the waiting resource changes wave in a newer commit (wave and pending become unknown), `CreateNamespace=true` next to a PreSync hook, a SyncFail hook running after a failed apply, a PruneLast task still ahead, a hook held back from deletion, and an Application being deleted while a ConfigMap holds a finalizer.

Other checks: 135 unit tests (subtests included) under `-race`, golden text and JSON files for 16 snapshots captured from real
Argo CD v3.5.3 controllers, a test that every field the tool declares exists in the Application and AppProject
CRDs of both versions, `golangci-lint`, `actionlint`, `shellcheck`, and a goreleaser snapshot build.

Things the real runs showed, which shaped the tool:

- With `CreateNamespace=true` the namespace result stays `hookPhase: Running` after its step ended. The tool
  takes the first waiting task from the controller's own operation message and treats the other `Running`
  results as stale.
- A deny sync window keeps auto-sync from creating an operation at all and leaves no trace in the Application.
  The tool computes the window itself. A manual operation in a deny window is created and held with the message
  `Sync operation blocked by sync window`.
- The retry time in Argo CD's own message is computed with the old retry count; the controller waits for
  `duration * factor^retryCount` with the incremented count. The scenario observed no retry earlier than that
  time (the second attempt started after 44 s with a 20 s base and factor 2), so the tool reports it as `not before`.
- Without a custom health check for `argoproj.io/Application`, a parent Application finished Succeeded and
  Healthy while its child was still Progressing.
- A child manifest with `retry: {limit: 0}` is normalized to `retry: {}` by Argo CD and keeps the parent
  OutOfSync.

Not verified: ApplicationSet, Argo CD versions other than the two above, Kubernetes versions other than v1.35, an
Argo CD that persists resource health in the Application (`controller.resource.health.persist: "true"`),
Argo CD installed with a custom `application.namespaces`, and Windows.

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
