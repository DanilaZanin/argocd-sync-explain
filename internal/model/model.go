// Package model holds the minimal local copies of the Argo CD types this tool
// reads, plus the collected Snapshot that the explain engine consumes.
//
// Only fields that the tool uses are declared. Field names and JSON tags follow
// pkg/apis/application/v1alpha1/types.go in Argo CD v3.4 and v3.5.
package model

import (
	"encoding/json"
	"time"
)

// Operation phases (gitops-engine pkg/sync/common).
const (
	PhaseRunning     = "Running"
	PhaseTerminating = "Terminating"
	PhaseFailed      = "Failed"
	PhaseError       = "Error"
	PhaseSucceeded   = "Succeeded"
)

// Completed mirrors OperationPhase.Completed().
func Completed(phase string) bool {
	return phase == PhaseFailed || phase == PhaseError || phase == PhaseSucceeded
}

// Source is an application source. Raw keeps the decoded JSON so that two
// sources can be compared the way the controller compares them (deep equality).
type Source struct {
	RepoURL        string         `json:"repoURL,omitempty"`
	Path           string         `json:"path,omitempty"`
	Chart          string         `json:"chart,omitempty"`
	TargetRevision string         `json:"targetRevision,omitempty"`
	Raw            map[string]any `json:"-"`
}

// UnmarshalJSON decodes the typed fields and keeps the raw object.
func (s *Source) UnmarshalJSON(b []byte) error {
	type plain Source
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*s = Source(p)
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err == nil {
		s.Raw = raw
	}
	return nil
}

// OwnerRef is a metadata.ownerReferences entry.
type OwnerRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// Metadata is the subset of ObjectMeta that is used.
type Metadata struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	OwnerReferences   []OwnerRef        `json:"ownerReferences,omitempty"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty"`
}

// Destination is spec.destination.
type Destination struct {
	Server    string `json:"server,omitempty"`
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// Backoff is the retry backoff.
type Backoff struct {
	Duration    string `json:"duration,omitempty"`
	Factor      *int64 `json:"factor,omitempty"`
	MaxDuration string `json:"maxDuration,omitempty"`
}

// RetryStrategy is spec.syncPolicy.retry and operation.retry.
type RetryStrategy struct {
	Limit   int64    `json:"limit,omitempty"`
	Backoff *Backoff `json:"backoff,omitempty"`
	Refresh bool     `json:"refresh,omitempty"`
}

// Automated is spec.syncPolicy.automated. A nil pointer to Automated means absent.
type Automated struct {
	Prune      *bool `json:"prune,omitempty"`
	SelfHeal   *bool `json:"selfHeal,omitempty"`
	AllowEmpty *bool `json:"allowEmpty,omitempty"`
	Enabled    *bool `json:"enabled,omitempty"`
}

// SyncPolicy is spec.syncPolicy.
type SyncPolicy struct {
	Automated   *Automated     `json:"automated,omitempty"`
	SyncOptions []string       `json:"syncOptions,omitempty"`
	Retry       *RetryStrategy `json:"retry,omitempty"`
}

// Spec is the Application spec subset.
type Spec struct {
	Project     string      `json:"project"`
	Source      *Source     `json:"source,omitempty"`
	Sources     []Source    `json:"sources,omitempty"`
	Destination Destination `json:"destination"`
	SyncPolicy  *SyncPolicy `json:"syncPolicy,omitempty"`
}

// SyncOperation is operation.sync.
type SyncOperation struct {
	Revision    string   `json:"revision,omitempty"`
	Revisions   []string `json:"revisions,omitempty"`
	Prune       bool     `json:"prune,omitempty"`
	DryRun      bool     `json:"dryRun,omitempty"`
	SyncOptions []string `json:"syncOptions,omitempty"`
	// Resources is the selective sync list; non-empty means selective sync.
	Resources []map[string]any `json:"resources,omitempty"`
}

// Initiator is operation.initiatedBy.
type Initiator struct {
	Username  string `json:"username,omitempty"`
	Automated bool   `json:"automated,omitempty"`
}

// Operation is a requested operation (.operation or operationState.operation).
type Operation struct {
	Sync        *SyncOperation `json:"sync,omitempty"`
	InitiatedBy Initiator      `json:"initiatedBy,omitempty"`
	Retry       RetryStrategy  `json:"retry,omitempty"`
}

// ResourceResult is one entry of operationState.syncResult.resources.
type ResourceResult struct {
	Group     string `json:"group"`
	Version   string `json:"version"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Status is the apply result. Empty for hooks.
	Status    string `json:"status,omitempty"`
	Message   string `json:"message,omitempty"`
	HookType  string `json:"hookType,omitempty"`
	HookPhase string `json:"hookPhase,omitempty"`
	SyncPhase string `json:"syncPhase,omitempty"`
}

// SyncResult is operationState.syncResult.
type SyncResult struct {
	Resources []ResourceResult `json:"resources,omitempty"`
	Revision  string           `json:"revision"`
	Revisions []string         `json:"revisions,omitempty"`
	Source    *Source          `json:"source,omitempty"`
	Sources   []Source         `json:"sources,omitempty"`
}

// OperationState is status.operationState.
type OperationState struct {
	Operation  Operation   `json:"operation"`
	Phase      string      `json:"phase"`
	Message    string      `json:"message,omitempty"`
	SyncResult *SyncResult `json:"syncResult,omitempty"`
	StartedAt  time.Time   `json:"startedAt"`
	FinishedAt *time.Time  `json:"finishedAt,omitempty"`
	RetryCount int64       `json:"retryCount,omitempty"`
}

// Health is a health status block.
type Health struct {
	Status  string `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
}

// ResourceStatus is an entry of status.resources.
type ResourceStatus struct {
	Group                        string  `json:"group,omitempty"`
	Version                      string  `json:"version,omitempty"`
	Kind                         string  `json:"kind,omitempty"`
	Namespace                    string  `json:"namespace,omitempty"`
	Name                         string  `json:"name,omitempty"`
	Status                       string  `json:"status,omitempty"`
	Health                       *Health `json:"health,omitempty"`
	Hook                         bool    `json:"hook,omitempty"`
	RequiresPruning              bool    `json:"requiresPruning,omitempty"`
	SyncWave                     int64   `json:"syncWave,omitempty"`
	RequiresDeletionConfirmation bool    `json:"requiresDeletionConfirmation,omitempty"`
}

// SyncStatus is status.sync.
type SyncStatus struct {
	Status    string   `json:"status,omitempty"`
	Revision  string   `json:"revision,omitempty"`
	Revisions []string `json:"revisions,omitempty"`
}

// Condition is a status.conditions entry.
type Condition struct {
	Type               string     `json:"type"`
	Message            string     `json:"message"`
	LastTransitionTime *time.Time `json:"lastTransitionTime,omitempty"`
}

// Status is the Application status subset.
type Status struct {
	Resources            []ResourceStatus `json:"resources,omitempty"`
	Sync                 SyncStatus       `json:"sync,omitempty"`
	Health               Health           `json:"health,omitempty"`
	Conditions           []Condition      `json:"conditions,omitempty"`
	ReconciledAt         *time.Time       `json:"reconciledAt,omitempty"`
	OperationState       *OperationState  `json:"operationState,omitempty"`
	ResourceHealthSource string           `json:"resourceHealthSource,omitempty"`
}

// Application is the Argo CD Application subset.
type Application struct {
	Metadata  Metadata   `json:"metadata"`
	Spec      Spec       `json:"spec"`
	Operation *Operation `json:"operation,omitempty"`
	Status    Status     `json:"status,omitempty"`
}

// SyncWindow is an AppProject spec.syncWindows entry.
type SyncWindow struct {
	Kind           string   `json:"kind,omitempty"`
	Schedule       string   `json:"schedule,omitempty"`
	Duration       string   `json:"duration,omitempty"`
	Applications   []string `json:"applications,omitempty"`
	Namespaces     []string `json:"namespaces,omitempty"`
	Clusters       []string `json:"clusters,omitempty"`
	ManualSync     bool     `json:"manualSync,omitempty"`
	TimeZone       string   `json:"timeZone,omitempty"`
	UseAndOperator bool     `json:"andOperator,omitempty"`
	Description    string   `json:"description,omitempty"`
	SyncOverrun    bool     `json:"syncOverrun,omitempty"`
}

// AppProject is the AppProject subset.
type AppProject struct {
	Metadata Metadata `json:"metadata"`
	Spec     struct {
		SyncWindows []SyncWindow `json:"syncWindows,omitempty"`
	} `json:"spec"`
}

// NodeRef identifies a resource tree node.
type NodeRef struct {
	Group     string `json:"group,omitempty"`
	Version   string `json:"version,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	UID       string `json:"uid,omitempty"`
}

// InfoItem is a resource tree node info entry.
type InfoItem struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// TreeNode is a node of the application resource tree (API mode only).
type TreeNode struct {
	NodeRef
	ParentRefs []NodeRef  `json:"parentRefs,omitempty"`
	Health     *Health    `json:"health,omitempty"`
	Info       []InfoItem `json:"info,omitempty"`
}

// ResourceTree is /api/v1/applications/{name}/resource-tree.
type ResourceTree struct {
	Nodes []TreeNode `json:"nodes,omitempty"`
}

// Event is a Kubernetes event about the application (API mode only).
type Event struct {
	Type          string    `json:"type"`
	Reason        string    `json:"reason"`
	Message       string    `json:"message"`
	LastTimestamp time.Time `json:"lastTimestamp"`
	Count         int       `json:"count"`
}

// Snapshot is everything the collectors could read about one Application.
// Anything the collector could not read is recorded in Unavailable with the
// reason, keyed by data kind. The engine never invents data that is not here.
type Snapshot struct {
	// Mode is "api" or "kubeconfig".
	Mode string
	// Now is the evaluation time (sync windows, backoff).
	Now time.Time
	// ArgoVersion is the Argo CD server version, or "" when unknown.
	ArgoVersion string
	App         Application
	Project     *AppProject
	Tree        *ResourceTree
	Events      []Event
	// Children are explained independently, keyed by "namespace/name".
	Children map[string]*Snapshot
	// Unavailable maps a data kind (tree, project, events, version,
	// child:<ns>/<name>) to the reason it is missing.
	Unavailable map[string]string
}

// Key returns "namespace/name".
func (a Application) Key() string { return a.Metadata.Namespace + "/" + a.Metadata.Name }
