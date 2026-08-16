package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vaulticv1 "github.com/vaultic/k8s-operator/api/v1"
)

func TestVaulticCertificateReconciler_Success(t *testing.T) {
	mockCertPEM := "-----BEGIN CERTIFICATE-----\nMIIC...mock-cert\n-----END CERTIFICATE-----"
	mockChainPEM := "-----BEGIN CERTIFICATE-----\nMIIC...mock-ca\n-----END CERTIFICATE-----"
	mockKeyPEM := "-----BEGIN PRIVATE KEY-----\nMIIE...mock-key\n-----END PRIVATE KEY-----"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/workspaces/acme/certificate-manager/applications/backend-api/certificates/api.internal/fetch" {
			t.Errorf("unexpected URL path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("authorization") != "Bearer mock-token-value" {
			t.Errorf("unexpected authorization: %s", r.Header.Get("authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("x-vaultic-source") != "api" {
			t.Errorf("unexpected x-vaultic-source header: %s", r.Header.Get("x-vaultic-source"))
		}

		resp := fetchedCertificate{
			CommonName:     "api.internal",
			Version:        3,
			CertificatePEM: mockCertPEM,
			ChainPEM:       mockChainPEM,
			PrivateKeyPEM:  mockKeyPEM,
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

	vc := &vaulticv1.VaulticCertificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "backend-api-tls",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: vaulticv1.VaulticCertificateSpec{
			ServerURL:   server.URL,
			Workspace:   "acme",
			Application: "backend-api",
			CommonName:  "api.internal",
			TokenSecretRef: vaulticv1.SecretKeyRef{
				Name: "vaultic-token",
				Key:  "token",
			},
			RefreshIntervalSeconds: 60,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&vaulticv1.VaulticCertificate{}).
		WithObjects(tokenSecret, vc).
		Build()

	r := &VaulticCertificateReconciler{
		Client:     client,
		Scheme:     scheme,
		HTTPClient: server.Client(),
	}

	ctx := context.Background()
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "backend-api-tls",
		},
	}

	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error during Reconcile: %v", err)
	}

	if res.RequeueAfter != 60*time.Second {
		t.Fatalf("expected RequeueAfter 60s, got %v", res.RequeueAfter)
	}

	var targetSecret corev1.Secret
	if err := client.Get(ctx, req.NamespacedName, &targetSecret); err != nil {
		t.Fatalf("failed to get TLS Secret: %v", err)
	}

	if targetSecret.Type != corev1.SecretTypeTLS {
		t.Errorf("expected SecretTypeTLS, got %s", targetSecret.Type)
	}

	expectedCert := strings.TrimSpace(mockCertPEM+"\n"+mockChainPEM) + "\n"
	if string(targetSecret.Data[corev1.TLSCertKey]) != expectedCert {
		t.Errorf("tls.crt mismatch: got %q, expected %q", string(targetSecret.Data[corev1.TLSCertKey]), expectedCert)
	}

	if string(targetSecret.Data[corev1.TLSPrivateKeyKey]) != mockKeyPEM {
		t.Errorf("tls.key mismatch: got %q, expected %q", string(targetSecret.Data[corev1.TLSPrivateKeyKey]), mockKeyPEM)
	}

	if len(targetSecret.OwnerReferences) != 1 || targetSecret.OwnerReferences[0].Name != "backend-api-tls" {
		t.Errorf("owner reference mismatch: %+v", targetSecret.OwnerReferences)
	}

	var updatedVC vaulticv1.VaulticCertificate
	if err := client.Get(ctx, req.NamespacedName, &updatedVC); err != nil {
		t.Fatalf("failed to get updated VaulticCertificate: %v", err)
	}

	if updatedVC.Status.Version != 3 {
		t.Errorf("expected Status.Version 3, got %d", updatedVC.Status.Version)
	}
	if updatedVC.Status.LastSyncTime == nil {
		t.Error("expected LastSyncTime to be set")
	}

	readyCond := findCondition(updatedVC.Status.Conditions, "Ready")
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

func TestVaulticCertificateReconciler_UpdateAndRotation(t *testing.T) {
	newCertPEM := "-----BEGIN CERTIFICATE-----\nROTATED_CERT\n-----END CERTIFICATE-----"
	newKeyPEM := "-----BEGIN PRIVATE KEY-----\nROTATED_KEY\n-----END PRIVATE KEY-----"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := fetchedCertificate{
			CommonName:     "api.internal",
			Version:        4,
			CertificatePEM: newCertPEM,
			ChainPEM:       "",
			PrivateKeyPEM:  newKeyPEM,
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

	existingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "backend-tls", Namespace: "default"},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte("old-cert"),
			corev1.TLSPrivateKeyKey: []byte("old-key"),
		},
	}

	vc := &vaulticv1.VaulticCertificate{
		ObjectMeta: metav1.ObjectMeta{Name: "backend-tls", Namespace: "default"},
		Spec: vaulticv1.VaulticCertificateSpec{
			ServerURL:   server.URL,
			Workspace:   "acme",
			Application: "backend",
			CommonName:  "api.internal",
			TokenSecretRef: vaulticv1.SecretKeyRef{
				Name: "vaultic-token",
				Key:  "token",
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&vaulticv1.VaulticCertificate{}).
		WithObjects(tokenSecret, existingSecret, vc).
		Build()

	r := &VaulticCertificateReconciler{
		Client:     client,
		Scheme:     scheme,
		HTTPClient: server.Client(),
	}

	ctx := context.Background()
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "backend-tls"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var targetSecret corev1.Secret
	if err := client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "backend-tls"}, &targetSecret); err != nil {
		t.Fatalf("failed to get target Secret: %v", err)
	}

	if string(targetSecret.Data[corev1.TLSCertKey]) != strings.TrimSpace(newCertPEM)+"\n" {
		t.Errorf("cert not updated: got %q", string(targetSecret.Data[corev1.TLSCertKey]))
	}
	if string(targetSecret.Data[corev1.TLSPrivateKeyKey]) != newKeyPEM {
		t.Errorf("key not updated: got %q", string(targetSecret.Data[corev1.TLSPrivateKeyKey]))
	}

	var updatedVC vaulticv1.VaulticCertificate
	_ = client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "backend-tls"}, &updatedVC)
	if updatedVC.Status.Version != 4 {
		t.Errorf("expected version 4, got %d", updatedVC.Status.Version)
	}
}

func TestVaulticCertificateReconciler_TokenSecretNotFound(t *testing.T) {
	scheme := newTestScheme(t)

	vc := &vaulticv1.VaulticCertificate{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-token-cert", Namespace: "default"},
		Spec: vaulticv1.VaulticCertificateSpec{
			ServerURL:   "http://localhost:4000",
			Workspace:   "acme",
			Application: "backend",
			CommonName:  "api.internal",
			TokenSecretRef: vaulticv1.SecretKeyRef{
				Name: "non-existent-secret",
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&vaulticv1.VaulticCertificate{}).
		WithObjects(vc).
		Build()

	r := &VaulticCertificateReconciler{
		Client: client,
		Scheme: scheme,
	}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "missing-token-cert"}})
	if err == nil {
		t.Fatal("expected an error so controller-runtime applies exponential backoff")
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no explicit RequeueAfter (rate limiter handles backoff), got %v", res.RequeueAfter)
	}

	var updatedVC vaulticv1.VaulticCertificate
	_ = client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "missing-token-cert"}, &updatedVC)
	readyCond := findCondition(updatedVC.Status.Conditions, "Ready")
	if readyCond == nil || readyCond.Status != metav1.ConditionFalse || readyCond.Reason != "FetchFailed" {
		t.Errorf("expected FetchFailed condition False, got %+v", readyCond)
	}
}

func TestVaulticCertificateReconciler_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Certificate not found"})
	}))
	defer server.Close()

	scheme := newTestScheme(t)

	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vaultic-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("token")},
	}

	vc := &vaulticv1.VaulticCertificate{
		ObjectMeta: metav1.ObjectMeta{Name: "not-found-cert", Namespace: "default"},
		Spec: vaulticv1.VaulticCertificateSpec{
			ServerURL:   server.URL,
			Workspace:   "acme",
			Application: "backend",
			CommonName:  "missing.internal",
			TokenSecretRef: vaulticv1.SecretKeyRef{
				Name: "vaultic-token",
				Key:  "token",
			},
			RefreshIntervalSeconds: 10,
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&vaulticv1.VaulticCertificate{}).
		WithObjects(tokenSecret, vc).
		Build()

	r := &VaulticCertificateReconciler{
		Client:     client,
		Scheme:     scheme,
		HTTPClient: server.Client(),
	}

	ctx := context.Background()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "not-found-cert"}})
	if err == nil {
		t.Fatal("expected an error so controller-runtime applies exponential backoff")
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no explicit RequeueAfter (rate limiter handles backoff), got %v", res.RequeueAfter)
	}

	var updatedVC vaulticv1.VaulticCertificate
	_ = client.Get(ctx, types.NamespacedName{Namespace: "default", Name: "not-found-cert"}, &updatedVC)
	readyCond := findCondition(updatedVC.Status.Conditions, "Ready")
	if readyCond == nil || readyCond.Status != metav1.ConditionFalse || readyCond.Reason != "FetchFailed" {
		t.Errorf("expected FetchFailed condition False, got %+v", readyCond)
	}
}

func TestVaulticCertificateReconciler_DeletedCR(t *testing.T) {
	scheme := newTestScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := &VaulticCertificateReconciler{
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
