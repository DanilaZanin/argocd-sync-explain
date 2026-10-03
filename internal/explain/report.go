// Package explain turns a collected Snapshot into a Report. It is made of pure
// functions: no clock, network or filesystem access (the clock is Snapshot.Now).
package explain

import "fmt"

// Verdicts.
const (
	VerdictHealthy     = "healthy"
	VerdictWaiting     = "waiting"
	VerdictRetrying    = "retrying"
	VerdictQueued      = "queued"
	VerdictTerminating = "terminating"
	VerdictFailed      = "failed"
	VerdictError       = "error"
	VerdictDeleting    = "deleting"
	VerdictOutOfSync   = "out-of-sync"
	VerdictProgressing = "progressing"
	VerdictDegraded    = "degraded"
	VerdictUnknown     = "unknown"
)

// Operation kinds (A9).
const (
	OpNone        = "none"
	OpQueued      = "queued"
	OpRunning     = "running"
	OpBackoff     = "retry-backoff"
	OpTerminating = "terminating"
	OpFinished    = "finished"
)

// Blocker roles (A3: never mix waiting, failed and merely-progressing).
const (
	RoleWaitingOn = "waiting-on"
	RoleFailed    = "failed"
	RolePending   = "pending"
)

// Unknown marks data the snapshot does not have.
const Unknown = "unknown"

// Report is the full explanation of one Application.
type Report struct {
	Application AppInfo        `json:"application"`
	Mode        string         `json:"mode"`
	ArgoVersion string         `json:"argoVersion,omitempty"`
	Verdict     string         `json:"verdict"`
	ExitCode    int            `json:"exitCode"`
	Summary     string         `json:"summary"`
	Operation   OperationInfo  `json:"operation"`
	Position    *Position      `json:"position,omitempty"`
	Blockers    []Blocker      `json:"blockers,omitempty"`
	Pending     *PendingInfo   `json:"pending,omitempty"`
	Observation []Observation  `json:"observations,omitempty"`
	OutOfSync   []OutOfSyncRes `json:"outOfSync,omitempty"`
	AutoSync    *AutoSyncInfo  `json:"autoSync,omitempty"`
	SyncWindows *WindowInfo    `json:"syncWindows,omitempty"`
	Conditions  []CondInfo     `json:"conditions,omitempty"`
	Notes       []Note         `json:"notes,omitempty"`
	Events      []EventInfo    `json:"events,omitempty"`
	Children    []ChildInfo    `json:"children,omitempty"`
	Deleting    *DeletingInfo  `json:"deleting,omitempty"`
	Unknowns    []UnknownItem  `json:"unknowns,omitempty"`

	waveOK  bool
	waveWhy string
}

// AppInfo identifies the application.
type AppInfo struct {
	Name      string   `json:"name"`
	Namespace string   `json:"namespace"`
	Project   string   `json:"project"`
	Sync      string   `json:"syncStatus"`
	Health    string   `json:"healthStatus"`
	Revisions []string `json:"comparedRevisions,omitempty"`
	ManagedBy string   `json:"managedBy,omitempty"`
	Source    string   `json:"source"`
}

// OperationInfo is the classified operation state (A9).
type OperationInfo struct {
	Kind       string   `json:"kind"`
	Phase      string   `json:"phase,omitempty"`
	Message    string   `json:"message,omitempty"`
	StartedAt  string   `json:"startedAt,omitempty"`
	FinishedAt string   `json:"finishedAt,omitempty"`
	Initiator  string   `json:"initiator,omitempty"`
	Revisions  []string `json:"revisions,omitempty"`
	RetryCount int64    `json:"retryCount"`
	// Retry is the effective policy from operationState.operation.retry.
	Retry *RetryInfo `json:"retry,omitempty"`
	// NextRetryNotBefore is the earliest time of the next attempt (backoff).
	NextRetryNotBefore string   `json:"nextRetryNotBefore,omitempty"`
	WaitReason         string   `json:"waitReason,omitempty"`
	Sources            []string `json:"sources"`
}

// RetryInfo is the effective retry strategy.
type RetryInfo struct {
	Limit     int64  `json:"limit"`
	Unlimited bool   `json:"unlimited"`
	Backoff   string `json:"backoff"`
	Source    string `json:"source"`
}

// Position is the reconstructed sync position (A2). There is no cursor in the API.
type Position struct {
	Phase string `json:"phase"`
	Wave  string `json:"wave"`
	Basis string `json:"basis"`
}

// Health is a (possibly unknown) health value with its provenance.
type Health struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Source  string `json:"source"`
}

// Ref identifies a resource.
type Ref struct {
	Group     string `json:"group,omitempty"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// ID is a stable identity string: group/kind/namespace/name.
func (r Ref) ID() string { return fmt.Sprintf("%s/%s/%s/%s", r.Group, r.Kind, r.Namespace, r.Name) }

// Blocker is something the operation is waiting on, has failed on, or has
// still pending. Role says which; they are never mixed.
type Blocker struct {
	Role      string  `json:"role"`
	Type      string  `json:"type"` // resource | hook | child-application | sync-window | prune-confirmation
	Ref       Ref     `json:"ref"`
	ID        string  `json:"id"`
	SyncPhase string  `json:"syncPhase,omitempty"`
	HookType  string  `json:"hookType,omitempty"`
	HookPhase string  `json:"hookPhase,omitempty"`
	Result    string  `json:"applyResult,omitempty"`
	Wave      string  `json:"wave"`
	Message   string  `json:"message,omitempty"`
	Health    *Health `json:"health,omitempty"`
	Why       string  `json:"why"`
	Source    string  `json:"source"`
}

// PendingInfo lists tasks that have no result yet (reconstructed).
type PendingInfo struct {
	Count     int      `json:"count"`
	Waves     []int64  `json:"waves"`
	Resources []string `json:"resources"`
	Basis     string   `json:"basis"`
	Caveat    string   `json:"caveat"`
}

// Observation is a descendant of a blocker (ReplicaSet, Pod...). It is context,
// never the blocker (A6).
type Observation struct {
	Under  string  `json:"under"`
	Ref    Ref     `json:"ref"`
	Health *Health `json:"health,omitempty"`
	Info   string  `json:"info,omitempty"`
	Source string  `json:"source"`
}

// OutOfSyncRes is one OutOfSync resource (A7). No field-level diff.
type OutOfSyncRes struct {
	Ref             Ref    `json:"ref"`
	Status          string `json:"status"`
	RequiresPruning bool   `json:"requiresPruning,omitempty"`
	Missing         string `json:"missing"`
	Source          string `json:"source"`
}

// Finding is one auto-sync fact with the field it came from.
type Finding struct {
	Text string `json:"text"`
	// Blocks is true when the finding explains why no sync is started.
	Blocks bool   `json:"blocks,omitempty"`
	Source string `json:"source"`
}

// AutoSyncInfo separates verified constraints from possible reasons (A8).
type AutoSyncInfo struct {
	Enabled  bool      `json:"enabled"`
	Verified []Finding `json:"verified"`
	Possible []Finding `json:"possible"`
}

// WindowInfo is the sync window evaluation for now.
type WindowInfo struct {
	Evaluated bool     `json:"evaluated"`
	CanSync   bool     `json:"canSync"`
	Reason    string   `json:"reason"`
	Rules     string   `json:"rules"`
	Matching  []string `json:"matching,omitempty"`
	Manual    bool     `json:"assumedManual"`
	Source    string   `json:"source"`
}

// CondInfo is a status.conditions entry.
type CondInfo struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Source  string `json:"source"`
}

// Note is a short factual remark with its source.
type Note struct {
	Code   string `json:"code"`
	Text   string `json:"text"`
	Source string `json:"source"`
}

// EventInfo is a recent warning event.
type EventInfo struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Count   int    `json:"count"`
}

// ChildInfo is a child Application of an app-of-apps (A5).
type ChildInfo struct {
	Ref      Ref     `json:"ref"`
	Relation string  `json:"relation"` // parent-waits | parent-does-not-wait | unknown
	Evidence string  `json:"evidence"`
	Health   *Health `json:"health,omitempty"`
	Report   *Report `json:"report,omitempty"`
	Source   string  `json:"source"`
}

// Child relations.
const (
	RelWaits      = "parent-waits"
	RelNotWaiting = "parent-does-not-wait"
	RelUnknown    = "unknown"
)

// UnknownItem explains why something is not printed.
type UnknownItem struct {
	What   string `json:"what"`
	Reason string `json:"reason"`
}

// DeletingInfo explains an Application that has a deletionTimestamp but still exists.
type DeletingInfo struct {
	Since      string         `json:"since"`
	Finalizers []string       `json:"finalizers"`
	Cascade    string         `json:"cascade"`
	Remaining  []RemainingRes `json:"remaining"`
	Basis      string         `json:"basis"`
}

// RemainingRes is a managed resource that may still hold the deletion.
type RemainingRes struct {
	Ref        Ref      `json:"ref"`
	State      string   `json:"state"`
	Finalizers []string `json:"finalizers,omitempty"`
	Source     string   `json:"source"`
}
