package controller

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vaulticv1 "github.com/vaultic/k8s-operator/api/v1"
)

// TestVaulticSecretReconciler_LiveServer tests against a running Vaultic instance on localhost:4000.
// Requires VAULTIC_LIVE_TOKEN to be set to a service token for a k8s-live-ws/k8s-live-proj/production
// environment seeded with LIVE_API_KEY=super-secret-live-token-456 and
// LIVE_DB_URL=postgres://prod:password@db.internal:5432/live. Never hardcode a real token here —
// it's a live credential and would otherwise end up committed to git history.
func TestVaulticSecretReconciler_LiveServer(t *testing.T) {
	liveToken := os.Getenv("VAULTIC_LIVE_TOKEN")
	if liveToken == "" {
		t.Skip("VAULTIC_LIVE_TOKEN not set, skipping live test")
		return
	}

	// Probe whether the live server is running
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://localhost:4000/ops/seal-status")
	if err != nil || resp.StatusCode != http.StatusUnauthorized { // 401 means server is running
		t.Skip("Live Vaultic server not running on localhost:4000, skipping live test")
		return
	}
	defer resp.Body.Close()

	scheme := newTestScheme(t)

	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vaultic-live-token",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"token": []byte(liveToken),
		},
	}

	vs := &vaulticv1.VaulticSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "k8s-live-app",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: vaulticv1.VaulticSecretSpec{
			ServerURL:   "http://localhost:4000",
			Workspace:   "k8s-live-ws",
			Project:     "k8s-live-proj",
			Environment: "production",
			TokenSecretRef: vaulticv1.SecretKeyRef{
				Name: "vaultic-live-token",
				Key:  "token",
			},
			RefreshIntervalSeconds: 60,
		},
	}

	fakeK8s := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&vaulticv1.VaulticSecret{}).
		WithObjects(tokenSecret, vs).
		Build()

	r := &VaulticSecretReconciler{
		Client:     fakeK8s,
		Scheme:     scheme,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}

	ctx := context.Background()
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "k8s-live-app",
		},
	}

	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Live reconcile failed: %v", err)
	}

	if res.RequeueAfter != 60*time.Second {
		t.Errorf("expected 60s requeue, got %v", res.RequeueAfter)
	}

	var target corev1.Secret
	if err := fakeK8s.Get(ctx, req.NamespacedName, &target); err != nil {
		t.Fatalf("failed to retrieve materialized secret: %v", err)
	}

	if string(target.Data["LIVE_API_KEY"]) != "super-secret-live-token-456" {
		t.Errorf("LIVE_API_KEY mismatch: got %q", string(target.Data["LIVE_API_KEY"]))
	}
	if string(target.Data["LIVE_DB_URL"]) != "postgres://prod:password@db.internal:5432/live" {
		t.Errorf("LIVE_DB_URL mismatch: got %q", string(target.Data["LIVE_DB_URL"]))
	}

	var updatedVS vaulticv1.VaulticSecret
	if err := fakeK8s.Get(ctx, req.NamespacedName, &updatedVS); err != nil {
		t.Fatalf("failed to retrieve updated CR: %v", err)
	}

	if updatedVS.Status.SyncedKeyCount != 2 {
		t.Errorf("expected SyncedKeyCount 2, got %d", updatedVS.Status.SyncedKeyCount)
	}
	readyCond := findCondition(updatedVS.Status.Conditions, "Ready")
	if readyCond == nil || readyCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Ready condition True, got %+v", readyCond)
	}
}
