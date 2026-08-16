package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// VaulticCertificateSpec describes which Vaultic certificate to sync and where to materialize it.
type VaulticCertificateSpec struct {
	// ServerURL is the Vaultic API base URL, e.g. "https://vaultic.example.com".
	// +kubebuilder:validation:Pattern=`^https?://\S+$`
	ServerURL string `json:"serverURL"`

	// Workspace slug.
	// +kubebuilder:validation:MinLength=1
	Workspace string `json:"workspace"`
	// Application slug. Certificate Manager's Application is workspace-scoped, not nested
	// under a Secrets Manager Project/Environment — see docs/certificate-manager-plan.md's
	// "Cert scoping" addendum.
	// +kubebuilder:validation:MinLength=1
	Application string `json:"application"`
	// CommonName identifies the Vaultic certificate to fetch from the application.
	// +kubebuilder:validation:MinLength=1
	CommonName string `json:"commonName"`

	// TokenSecretRef points at the Kubernetes Secret holding a Vaultic service token.
	TokenSecretRef SecretKeyRef `json:"tokenSecretRef"`

	// TargetSecretName is the native TLS Secret this controller creates/updates. Defaults to the CR name.
	TargetSecretName string `json:"targetSecretName,omitempty"`

	// RefreshIntervalSeconds controls how often the controller re-polls Vaultic. Defaults to 60.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=5
	RefreshIntervalSeconds int32 `json:"refreshIntervalSeconds,omitempty"`
}

// VaulticCertificateStatus reports the outcome of the most recent sync.
type VaulticCertificateStatus struct {
	// LastSyncTime is when the target Secret was last successfully refreshed.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
	// Version is the Vaultic certificate version written at last sync.
	// +optional
	Version int32 `json:"version,omitempty"`
	// Conditions follow the standard Kubernetes condition convention.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Workspace",type=string,JSONPath=`.spec.workspace`
// +kubebuilder:printcolumn:name="Application",type=string,JSONPath=`.spec.application`
// +kubebuilder:printcolumn:name="Common Name",type=string,JSONPath=`.spec.commonName`
// +kubebuilder:printcolumn:name="Version",type=integer,JSONPath=`.status.version`
// +kubebuilder:printcolumn:name="Last Sync",type=date,JSONPath=`.status.lastSyncTime`

// VaulticCertificate materializes one Vaultic certificate as a Kubernetes TLS Secret.
type VaulticCertificate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VaulticCertificateSpec   `json:"spec,omitempty"`
	Status VaulticCertificateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VaulticCertificateList contains a list of VaulticCertificate.
type VaulticCertificateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VaulticCertificate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VaulticCertificate{}, &VaulticCertificateList{})
}

// TargetSecretNameOrDefault returns spec.TargetSecretName, falling back to the CR's own name.
func (v *VaulticCertificate) TargetSecretNameOrDefault() string {
	if v.Spec.TargetSecretName != "" {
		return v.Spec.TargetSecretName
	}
	return v.Name
}

// RefreshIntervalSecondsOrDefault returns spec.RefreshIntervalSeconds, defaulting to 60 (minimum 5).
func (v *VaulticCertificate) RefreshIntervalSecondsOrDefault() int32 {
	if v.Spec.RefreshIntervalSeconds >= 5 {
		return v.Spec.RefreshIntervalSeconds
	}
	if v.Spec.RefreshIntervalSeconds > 0 {
		return 5
	}
	return 60
}
