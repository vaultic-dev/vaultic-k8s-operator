package v1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestVaulticSecret_TargetSecretNameOrDefault(t *testing.T) {
	tests := []struct {
		name     string
		vs       VaulticSecret
		expected string
	}{
		{
			name: "uses TargetSecretName when specified",
			vs: VaulticSecret{
				ObjectMeta: metav1.ObjectMeta{Name: "my-cr-name"},
				Spec:       VaulticSecretSpec{TargetSecretName: "custom-secret-name"},
			},
			expected: "custom-secret-name",
		},
		{
			name: "falls back to CR name when TargetSecretName is empty",
			vs: VaulticSecret{
				ObjectMeta: metav1.ObjectMeta{Name: "my-cr-name"},
				Spec:       VaulticSecretSpec{TargetSecretName: ""},
			},
			expected: "my-cr-name",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.vs.TargetSecretNameOrDefault()
			if got != tc.expected {
				t.Fatalf("expected %q, got %q", tc.expected, got)
			}
		})
	}
}

func TestVaulticSecret_RefreshIntervalSecondsOrDefault(t *testing.T) {
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
			interval: -10,
			expected: 60,
		},
		{
			name:     "clamped to minimum 5 when 1",
			interval: 1,
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
			name:     "custom value when 120",
			interval: 120,
			expected: 120,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vs := VaulticSecret{
				Spec: VaulticSecretSpec{RefreshIntervalSeconds: tc.interval},
			}
			got := vs.RefreshIntervalSecondsOrDefault()
			if got != tc.expected {
				t.Fatalf("expected %d, got %d", tc.expected, got)
			}
		})
	}
}
