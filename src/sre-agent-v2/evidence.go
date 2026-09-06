package main

import "time"

// IncidentEvidence is the bounded, structured evidence supplied to the model.
// Every string originating from Kubernetes or Prometheus is untrusted data.
type IncidentEvidence struct {
	IncidentID string        `json:"incident_id"`
	Alert      AlertEvidence `json:"alert"`
	Pod        PodEvidence   `json:"pod"`
}

type AlertEvidence struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	State       string    `json:"state,omitempty"`
	ActiveAt    time.Time `json:"active_at,omitempty"`
}

type PodEvidence struct {
	Target       DecisionTarget      `json:"target"`
	Phase        string              `json:"phase,omitempty"`
	NodeName     string              `json:"node_name,omitempty"`
	Owner        OwnerEvidence       `json:"controller_owner,omitempty"`
	Containers   []ContainerEvidence `json:"containers,omitempty"`
	RecentLogs   []ContainerLog      `json:"recent_logs,omitempty"`
	RecentEvents []EventEvidence     `json:"recent_events,omitempty"`
}

type OwnerEvidence struct {
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
	UID  string `json:"uid,omitempty"`
}

type ContainerEvidence struct {
	Name                    string `json:"name"`
	Ready                   bool   `json:"ready"`
	RestartCount            int32  `json:"restart_count"`
	State                   string `json:"state,omitempty"`
	Reason                  string `json:"reason,omitempty"`
	LastTerminationReason   string `json:"last_termination_reason,omitempty"`
	LastTerminationExitCode int32  `json:"last_termination_exit_code,omitempty"`
}

type ContainerLog struct {
	Container string `json:"container"`
	Previous  bool   `json:"previous"`
	Content   string `json:"content"`
	Error     string `json:"error,omitempty"`
}

type EventEvidence struct {
	Type    string `json:"type,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	Count   int32  `json:"count,omitempty"`
}
