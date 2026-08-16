package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretKeyRef points at a single key inside an existing Kubernetes Secret, e.g. for the
// Vaultic service token — the token itself is never stored inline in the CR spec.
type SecretKeyRef struct {
	// Name of the Secret in the same namespace as the VaulticSecret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Key within that Secret's data. Defaults to "token".
	// +kubebuilder:default=token
	Key string `json:"key,omitempty"`
}

// VaulticSecretSpec describes which Vaultic environment to sync and where to materialize it.
type VaulticSecretSpec struct {
	// ServerURL is the Vaultic API base URL, e.g. "https://vaultic.example.com".
	// +kubebuilder:validation:Pattern=`^https?://\S+$`
	ServerURL string `json:"serverURL"`

	// Workspace slug.
	// +kubebuilder:validation:MinLength=1
	Workspace string `json:"workspace"`
	// Project slug.
	// +kubebuilder:validation:MinLength=1
	Project string `json:"project"`
	// Environment slug.
	// +kubebuilder:validation:MinLength=1
	Environment string `json:"environment"`

	// TokenSecretRef points at the Kubernetes Secret holding a Vaultic service token
	// (read scope is sufficient) — never put the token directly in the spec.
	TokenSecretRef SecretKeyRef `json:"tokenSecretRef"`

	// TargetSecretName is the name of the native v1.Secret this controller creates/updates
	// with the resolved key/value pairs. Defaults to the VaulticSecret's own name.
	TargetSecretName string `json:"targetSecretName,omitempty"`

	// RefreshIntervalSeconds controls how often the controller re-polls Vaultic and refreshes
	// the target Secret, even with no incoming Kubernetes events. Defaults to 60.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=5
	RefreshIntervalSeconds int32 `json:"refreshIntervalSeconds,omitempty"`
}

// VaulticSecretStatus reports the outcome of the most recent sync.
type VaulticSecretStatus struct {
	// LastSyncTime is when the target Secret was last successfully refreshed.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
	// SyncedKeyCount is how many keys were written into the target Secret at last sync.
	// +optional
	SyncedKeyCount int32 `json:"syncedKeyCount,omitempty"`
	// Conditions follow the standard Kubernetes condition convention; this controller sets a
	// single "Ready" condition.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Workspace",type=string,JSONPath=`.spec.workspace`
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.project`
// +kubebuilder:printcolumn:name="Environment",type=string,JSONPath=`.spec.environment`
// +kubebuilder:printcolumn:name="Keys",type=integer,JSONPath=`.status.syncedKeyCount`
// +kubebuilder:printcolumn:name="Last Sync",type=date,JSONPath=`.status.lastSyncTime`

// VaulticSecret materializes a Vaultic environment's resolved secrets as a native Kubernetes
// Secret, refreshed on an interval.
type VaulticSecret struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VaulticSecretSpec   `json:"spec,omitempty"`
	Status VaulticSecretStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VaulticSecretList contains a list of VaulticSecret.
type VaulticSecretList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VaulticSecret `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VaulticSecret{}, &VaulticSecretList{})
}

// TargetSecretNameOrDefault returns spec.TargetSecretName, falling back to the CR's own name.
func (v *VaulticSecret) TargetSecretNameOrDefault() string {
	if v.Spec.TargetSecretName != "" {
		return v.Spec.TargetSecretName
	}
	return v.Name
}

// RefreshIntervalSecondsOrDefault returns spec.RefreshIntervalSeconds, defaulting to 60 (minimum 5).
func (v *VaulticSecret) RefreshIntervalSecondsOrDefault() int32 {
	if v.Spec.RefreshIntervalSeconds >= 5 {
		return v.Spec.RefreshIntervalSeconds
	}
	if v.Spec.RefreshIntervalSeconds > 0 {
		return 5
	}
	return 60
}
