package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vaulticv1 "github.com/vaultic/k8s-operator/api/v1"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("failed to add client-go to scheme: %v", err)
	}
	if err := vaulticv1.AddToScheme(s); err != nil {
		t.Fatalf("failed to add vaulticv1 to scheme: %v", err)
	}
	return s
}

func TestVaulticSecretReconciler_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/workspaces/acme/projects/backend-api/environments/production/export" {
			t.Errorf("unexpected URL path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("authorization") != "Bearer mock-token-value" {
			t.Errorf("unexpected authorization header: %s", r.Header.Get("authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("x-vaultic-source") != "api" {
			t.Errorf("unexpected x-vaultic-source header: %s", r.Header.Get("x-vaultic-source"))
		}

		resp := map[string]string{
			"DATABASE_URL": "postgres://user:pass@host:5432/db",
			"API_KEY":      "super-secret-key",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	scheme := newTestScheme(t)

	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "vaultic-token",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"token": []byte("mock-token-value"),
		},
	}

	vs := &vaulticv1.VaulticSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "backend-api-production",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: vaulticv1.VaulticSecretSpec{
			ServerURL:   server.URL,
			Workspace:   "acme",
			Project:     "backend-api",
			Environment: "production",
			TokenSecretRef: vaulticv1.SecretKeyRef{
				Name: "vaultic-token",
				Key:  "token",
			},
			RefreshIntervalSeconds: 60,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&vaulticv1.VaulticSecret{}).
		WithObjects(tokenSecret, vs).
		Build()

	r := &VaulticSecretReconciler{
		Client:     client,
		Scheme:     scheme,
		HTTPClient: server.Client(),
	}

	ctx := context.Background()
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "backend-api-production",
		},
	}

	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error during Reconcile: %v", err)
	}

	if res.RequeueAfter != 60*time.Second {
		t.Fatalf("expected RequeueAfter 60s, got %v", res.RequeueAfter)
	}

	// Verify target Secret
	var targetSecret corev1.Secret
	if err := client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "backend-api-production"}, &targetSecret); err != nil {
		t.Fatalf("failed to get target Secret: %v", err)
	}

	if string(targetSecret.Data["DATABASE_URL"]) != "postgres://user:pass@host:5432/db" {
		t.Errorf("DATABASE_URL mismatch: got %q", string(targetSecret.Data["DATABASE_URL"]))
	}
	if string(targetSecret.Data["API_KEY"]) != "super-secret-key" {
		t.Errorf("API_KEY mismatch: got %q", string(targetSecret.Data["API_KEY"]))
	}

	// Verify owner reference
	if len(targetSecret.OwnerReferences) != 1 {
		t.Fatalf("expected 1 owner reference, got %d", len(targetSecret.OwnerReferences))
	}
	if targetSecret.OwnerReferences[0].Name != "backend-api-production" {
		t.Errorf("owner reference name mismatch: got %s", targetSecret.OwnerReferences[0].Name)
	}

	// Verify status
	var updatedVS vaulticv1.VaulticSecret
	if err := client.Get(ctx, req.NamespacedName, &updatedVS); err != nil {
		t.Fatalf("failed to get updated VaulticSecret: %v", err)
	}

	if updatedVS.Status.SyncedKeyCount != 2 {
		t.Errorf("expected SyncedKeyCount 2, got %d", updatedVS.Status.SyncedKeyCount)
	}
	if updatedVS.Status.LastSyncTime == nil {
		t.Error("expected LastSyncTime to be set")
	}

	readyCond := findCondition(updatedVS.Status.Conditions, "Ready")
	if readyCond == nil {
		t.Fatal("expected Ready condition")
	}
	if readyCond.Status != metav1.ConditionTrue {
		t.Errorf("expected Ready condition True, got %s", readyCond.Status)
	}
	if readyCond.Reason != "Synced" {
		t.Errorf("expected Ready reason 'Synced', got %s", readyCond.Reason)
	}
	if readyCond.ObservedGeneration != 1 {
		t.Errorf("expected ObservedGeneration 1, got %d", readyCond.ObservedGeneration)
	}
}

func TestVaulticSecretReconciler_KeyDeletion_StaleKeysRemoved(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Vaultic environment only contains NEW_KEY now (OLD_KEY was deleted)
		resp := map[string]string{
			"NEW_KEY": "new-value",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	scheme := newTestScheme(t)

	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vaultic-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("mock-token")},
	}

	// Existing target secret in cluster that currently contains a stale key from a prior sync
	existingTarget := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-secrets", Namespace: "default"},
		Data: map[string][]byte{
			"OLD_STALE_KEY": []byte("should-be-removed"),
			"NEW_KEY":       []byte("old-value"),
		},
	}

	vs := &vaulticv1.VaulticSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-secrets", Namespace: "default"},
		Spec: vaulticv1.VaulticSecretSpec{
			ServerURL:   server.URL,
			Workspace:   "acme",
			Project:     "app",
			Environment: "prod",
			TokenSecretRef: vaulticv1.SecretKeyRef{
				Name: "vaultic-token",
				Key:  "token",
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&vaulticv1.VaulticSecret{}).
		WithObjects(tokenSecret, existingTarget, vs).
		Build()

	r := &VaulticSecretReconciler{
		Client:     client,
		Scheme:     scheme,
		HTTPClient: server.Client(),
	}

	ctx := context.Background()
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "app-secrets"}})
	if err != nil {
		t.Fatalf("unexpected error during Reconcile: %v", err)
	}

	var targetSecret corev1.Secret
	if err := client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "app-secrets"}, &targetSecret); err != nil {
		t.Fatalf("failed to get target Secret: %v", err)
	}

	if _, exists := targetSecret.Data["OLD_STALE_KEY"]; exists {
		t.Errorf("OLD_STALE_KEY should have been deleted from Secret Data")
	}
	if string(targetSecret.Data["NEW_KEY"]) != "new-value" {
		t.Errorf("NEW_KEY mismatch: got %q", string(targetSecret.Data["NEW_KEY"]))
	}
	if len(targetSecret.Data) != 1 {
		t.Errorf("expected 1 key in Secret Data, got %d", len(targetSecret.Data))
	}
}

func TestVaulticSecretReconciler_CustomTargetSecretNameAndKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"K": "V"})
	}))
	defer server.Close()

	scheme := newTestScheme(t)

	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-token-secret", Namespace: "default"},
		Data:       map[string][]byte{"auth-token": []byte("mock-token")},
	}

	vs := &vaulticv1.VaulticSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-cr", Namespace: "default"},
		Spec: vaulticv1.VaulticSecretSpec{
			ServerURL:        server.URL,
			Workspace:        "acme",
			Project:          "app",
			Environment:      "prod",
			TargetSecretName: "custom-target-secret",
			TokenSecretRef: vaulticv1.SecretKeyRef{
				Name: "custom-token-secret",
				Key:  "auth-token",
			},
			RefreshIntervalSeconds: 15,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&vaulticv1.VaulticSecret{}).
		WithObjects(tokenSecret, vs).
		Build()

	r := &VaulticSecretReconciler{
		Client:     client,
		Scheme:     scheme,
		HTTPClient: server.Client(),
	}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "my-cr"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.RequeueAfter != 15*time.Second {
		t.Fatalf("expected RequeueAfter 15s, got %v", res.RequeueAfter)
	}

	var targetSecret corev1.Secret
	if err := client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "custom-target-secret"}, &targetSecret); err != nil {
		t.Fatalf("failed to get target Secret: %v", err)
	}
	if string(targetSecret.Data["K"]) != "V" {
		t.Errorf("expected value 'V', got %q", string(targetSecret.Data["K"]))
	}
}

func TestVaulticSecretReconciler_TokenSecretNotFound(t *testing.T) {
	scheme := newTestScheme(t)

	vs := &vaulticv1.VaulticSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-token-cr", Namespace: "default"},
		Spec: vaulticv1.VaulticSecretSpec{
			ServerURL:   "http://localhost:4000",
			Workspace:   "acme",
			Project:     "app",
			Environment: "prod",
			TokenSecretRef: vaulticv1.SecretKeyRef{
				Name: "non-existent-token-secret",
			},
			RefreshIntervalSeconds: 60,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&vaulticv1.VaulticSecret{}).
		WithObjects(vs).
		Build()

	r := &VaulticSecretReconciler{
		Client: client,
		Scheme: scheme,
	}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "missing-token-cr"}})
	if err == nil {
		t.Fatal("expected an error so controller-runtime applies exponential backoff")
	}

	if res.RequeueAfter != 0 {
		t.Fatalf("expected no explicit RequeueAfter (rate limiter handles backoff), got %v", res.RequeueAfter)
	}

	var updatedVS vaulticv1.VaulticSecret
	if err := client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "missing-token-cr"}, &updatedVS); err != nil {
		t.Fatalf("failed to get updated VaulticSecret: %v", err)
	}

	readyCond := findCondition(updatedVS.Status.Conditions, "Ready")
	if readyCond == nil {
		t.Fatal("expected Ready condition")
	}
	if readyCond.Status != metav1.ConditionFalse {
		t.Errorf("expected Ready condition False, got %s", readyCond.Status)
	}
	if readyCond.Reason != "FetchFailed" {
		t.Errorf("expected reason 'FetchFailed', got %s", readyCond.Reason)
	}
}

func TestVaulticSecretReconciler_TokenKeyMissing(t *testing.T) {
	scheme := newTestScheme(t)

	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "token-secret", Namespace: "default"},
		Data:       map[string][]byte{"other-key": []byte("value")},
	}

	vs := &vaulticv1.VaulticSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-key-cr", Namespace: "default"},
		Spec: vaulticv1.VaulticSecretSpec{
			ServerURL:   "http://localhost:4000",
			Workspace:   "acme",
			Project:     "app",
			Environment: "prod",
			TokenSecretRef: vaulticv1.SecretKeyRef{
				Name: "token-secret",
				Key:  "token",
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&vaulticv1.VaulticSecret{}).
		WithObjects(tokenSecret, vs).
		Build()

	r := &VaulticSecretReconciler{
		Client: client,
		Scheme: scheme,
	}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "missing-key-cr"}})
	if err == nil {
		t.Fatal("expected an error so controller-runtime applies exponential backoff")
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no explicit RequeueAfter (rate limiter handles backoff), got %v", res.RequeueAfter)
	}

	var updatedVS vaulticv1.VaulticSecret
	_ = client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "missing-key-cr"}, &updatedVS)
	readyCond := findCondition(updatedVS.Status.Conditions, "Ready")
	if readyCond == nil || readyCond.Status != metav1.ConditionFalse || readyCond.Reason != "FetchFailed" {
		t.Errorf("expected FetchFailed condition False, got %+v", readyCond)
	}
}

func TestVaulticSecretReconciler_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Invalid service token"})
	}))
	defer server.Close()

	scheme := newTestScheme(t)

	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vaultic-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("invalid-token")},
	}

	vs := &vaulticv1.VaulticSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "auth-err-cr", Namespace: "default"},
		Spec: vaulticv1.VaulticSecretSpec{
			ServerURL:   server.URL,
			Workspace:   "acme",
			Project:     "app",
			Environment: "prod",
			TokenSecretRef: vaulticv1.SecretKeyRef{
				Name: "vaultic-token",
				Key:  "token",
			},
			RefreshIntervalSeconds: 10,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&vaulticv1.VaulticSecret{}).
		WithObjects(tokenSecret, vs).
		Build()

	r := &VaulticSecretReconciler{
		Client:     client,
		Scheme:     scheme,
		HTTPClient: server.Client(),
	}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "auth-err-cr"}})
	if err == nil {
		t.Fatal("expected an error so controller-runtime applies exponential backoff")
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no explicit RequeueAfter (rate limiter handles backoff), got %v", res.RequeueAfter)
	}

	var updatedVS vaulticv1.VaulticSecret
	_ = client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "auth-err-cr"}, &updatedVS)
	readyCond := findCondition(updatedVS.Status.Conditions, "Ready")
	if readyCond == nil || readyCond.Status != metav1.ConditionFalse || readyCond.Reason != "FetchFailed" {
		t.Errorf("expected FetchFailed condition False, got %+v", readyCond)
	}
}

func TestVaulticSecretReconciler_DeletedCR(t *testing.T) {
	scheme := newTestScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := &VaulticSecretReconciler{
		Client: client,
		Scheme: scheme,
	}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "non-existent"}})
	if err != nil {
		t.Fatalf("expected nil error for deleted object, got: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("expected no requeue for deleted object, got %v", res.RequeueAfter)
	}
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}
