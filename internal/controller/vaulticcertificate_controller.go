package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	vaulticv1 "github.com/vaultic/k8s-operator/api/v1"
)

type VaulticCertificateReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	HTTPClient *http.Client
}

type fetchedCertificate struct {
	CommonName     string `json:"commonName"`
	Version        int32  `json:"version"`
	CertificatePEM string `json:"certificatePem"`
	ChainPEM       string `json:"chainPem"`
	PrivateKeyPEM  string `json:"privateKeyPem"`
}

// +kubebuilder:rbac:groups=vaultic.dev,resources=vaulticcertificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vaultic.dev,resources=vaulticcertificates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vaultic.dev,resources=vaulticcertificates/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *VaulticCertificateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var vc vaulticv1.VaulticCertificate
	if err := r.Get(ctx, req.NamespacedName, &vc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	refreshInterval := time.Duration(vc.RefreshIntervalSecondsOrDefault()) * time.Second
	cert, err := r.fetchCertificate(ctx, req.Namespace, &vc)
	if err != nil {
		logger.Error(err, "failed to fetch certificate from Vaultic")
		if statusErr := r.setCertificateReady(ctx, &vc, false, "FetchFailed", err.Error()); statusErr != nil {
			logger.Error(statusErr, "failed to update VaulticCertificate status")
		}
		// Return the error so controller-runtime's workqueue rate limiter applies
		// exponential backoff instead of hammering Vaultic on a sustained outage.
		return ctrl.Result{}, err
	}

	if err := r.applyTLSSecret(ctx, &vc, cert); err != nil {
		logger.Error(err, "failed to apply TLS Secret")
		if statusErr := r.setCertificateReady(ctx, &vc, false, "ApplyFailed", err.Error()); statusErr != nil {
			logger.Error(statusErr, "failed to update VaulticCertificate status")
		}
		return ctrl.Result{}, err
	}

	vc.Status.Version = cert.Version
	now := metav1.Now()
	vc.Status.LastSyncTime = &now
	if err := r.setCertificateReady(ctx, &vc, true, "Synced", fmt.Sprintf("synced certificate version %d", cert.Version)); err != nil {
		logger.Error(err, "failed to update VaulticCertificate status")
	}
	return ctrl.Result{RequeueAfter: refreshInterval}, nil
}

func (r *VaulticCertificateReconciler) fetchCertificate(ctx context.Context, namespace string, vc *vaulticv1.VaulticCertificate) (*fetchedCertificate, error) {
	tokenSecret := &corev1.Secret{}
	ref := vc.Spec.TokenSecretRef
	key := ref.Key
	if key == "" {
		key = "token"
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, tokenSecret); err != nil {
		return nil, fmt.Errorf("reading tokenSecretRef %s/%s: %w", namespace, ref.Name, err)
	}
	tokenBytes, ok := tokenSecret.Data[key]
	if !ok {
		return nil, fmt.Errorf("secret %s/%s has no key %q", namespace, ref.Name, key)
	}

	url := fmt.Sprintf(
		"%s/workspaces/%s/certificate-manager/applications/%s/certificates/%s/fetch",
		strings.TrimRight(vc.Spec.ServerURL, "/"), vc.Spec.Workspace, vc.Spec.Application, vc.Spec.CommonName,
	)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("authorization", "Bearer "+string(tokenBytes))
	httpReq.Header.Set("x-vaultic-source", "api")

	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	res, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("calling vaultic at %s: %w", vc.Spec.ServerURL, err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		var errBody struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &errBody)
		msg := errBody.Error
		if msg == "" {
			msg = string(body)
		}
		return nil, fmt.Errorf("vaultic returned HTTP %d: %s", res.StatusCode, msg)
	}

	var cert fetchedCertificate
	if err := json.Unmarshal(body, &cert); err != nil {
		return nil, fmt.Errorf("decoding vaultic response: %w", err)
	}
	return &cert, nil
}

func (r *VaulticCertificateReconciler) applyTLSSecret(ctx context.Context, vc *vaulticv1.VaulticCertificate, cert *fetchedCertificate) error {
	target := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: vc.TargetSecretNameOrDefault(), Namespace: vc.Namespace}}
	certData := []byte(strings.TrimSpace(cert.CertificatePEM+"\n"+cert.ChainPEM) + "\n")
	keyData := []byte(cert.PrivateKeyPEM)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, target, func() error {
		target.Type = corev1.SecretTypeTLS
		target.Data = map[string][]byte{
			corev1.TLSCertKey:       certData,
			corev1.TLSPrivateKeyKey: keyData,
		}
		target.StringData = nil
		return controllerutil.SetControllerReference(vc, target, r.Scheme)
	})
	return err
}

func (r *VaulticCertificateReconciler) setCertificateReady(ctx context.Context, vc *vaulticv1.VaulticCertificate, ready bool, reason, message string) error {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	meta := metav1.Condition{
		Type:               "Ready",
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: vc.Generation,
	}
	setCondition(&vc.Status.Conditions, meta)
	if err := r.Status().Update(ctx, vc); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return nil
		}
		return err
	}
	return nil
}

func (r *VaulticCertificateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vaulticv1.VaulticCertificate{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
