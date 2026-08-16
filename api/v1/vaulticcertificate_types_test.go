package v1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestVaulticCertificate_TargetSecretNameOrDefault(t *testing.T) {
	tests := []struct {
		name     string
		vc       VaulticCertificate
		expected string
	}{
		{
			name: "uses TargetSecretName when specified",
			vc: VaulticCertificate{
				ObjectMeta: metav1.ObjectMeta{Name: "my-cert-cr"},
				Spec:       VaulticCertificateSpec{TargetSecretName: "custom-tls-secret"},
			},
			expected: "custom-tls-secret",
		},
		{
			name: "falls back to CR name when TargetSecretName is empty",
			vc: VaulticCertificate{
				ObjectMeta: metav1.ObjectMeta{Name: "my-cert-cr"},
				Spec:       VaulticCertificateSpec{TargetSecretName: ""},
			},
			expected: "my-cert-cr",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.vc.TargetSecretNameOrDefault()
			if got != tc.expected {
				t.Fatalf("expected %q, got %q", tc.expected, got)
			}
		})
	}
}

func TestVaulticCertificate_RefreshIntervalSecondsOrDefault(t *testing.T) {
	tests := []struct {
		name     string
		interval int32
		expected int32
	}{
		{
			name:     "default 60 when unset (0)",
			interval: 0,
			expected: 60,
		},
		{
			name:     "default 60 when negative",
			interval: -5,
			expected: 60,
		},
		{
			name:     "clamped to minimum 5 when 2",
			interval: 2,
			expected: 5,
		},
		{
			name:     "clamped to minimum 5 when 4",
			interval: 4,
			expected: 5,
		},
		{
			name:     "exact value when 5",
			interval: 5,
			expected: 5,
		},
		{
			name:     "custom value when 300",
			interval: 300,
			expected: 300,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vc := VaulticCertificate{
				Spec: VaulticCertificateSpec{RefreshIntervalSeconds: tc.interval},
			}
			got := vc.RefreshIntervalSecondsOrDefault()
			if got != tc.expected {
				t.Fatalf("expected %d, got %d", tc.expected, got)
			}
		})
	}
}
