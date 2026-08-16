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

// VaulticSecretReconciler materializes each VaulticSecret's referenced Vaultic environment as
// a native Kubernetes Secret, refreshing on both Kubernetes-side changes to the CR and a
// periodic timer (spec.RefreshIntervalSeconds) so it also picks up secret changes made purely
// on the Vaultic side, which the Kubernetes API server has no way to notify us about.
type VaulticSecretReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	HTTPClient *http.Client
}

// +kubebuilder:rbac:groups=vaultic.dev,resources=vaulticsecrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vaultic.dev,resources=vaulticsecrets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vaultic.dev,resources=vaulticsecrets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *VaulticSecretReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var vs vaulticv1.VaulticSecret
	if err := r.Get(ctx, req.NamespacedName, &vs); err != nil {
		// object deleted: the target Secret is garbage-collected via its OwnerReference,
		// nothing else to do here.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	refreshInterval := time.Duration(vs.RefreshIntervalSecondsOrDefault()) * time.Second

	values, err := r.fetchEnvironment(ctx, req.Namespace, &vs)
	if err != nil {
		logger.Error(err, "failed to fetch secrets from Vaultic")
		if statusErr := r.setReady(ctx, &vs, false, "FetchFailed", err.Error()); statusErr != nil {
			logger.Error(statusErr, "failed to update VaulticSecret status")
		}
		// Return the error (rather than a fixed RequeueAfter) so controller-runtime's
		// workqueue rate limiter applies exponential backoff: fast retry on a transient
		// blip, without hammering Vaultic during a sustained outage.
		return ctrl.Result{}, err
	}

	if err := r.applyTargetSecret(ctx, &vs, values); err != nil {
		logger.Error(err, "failed to apply target Secret")
		if statusErr := r.setReady(ctx, &vs, false, "ApplyFailed", err.Error()); statusErr != nil {
			logger.Error(statusErr, "failed to update VaulticSecret status")
		}
		return ctrl.Result{}, err
	}

	vs.Status.SyncedKeyCount = int32(len(values))
	now := metav1.Now()
	vs.Status.LastSyncTime = &now
	if err := r.setReady(ctx, &vs, true, "Synced", fmt.Sprintf("synced %d key(s)", len(values))); err != nil {
		logger.Error(err, "failed to update VaulticSecret status")
	}

	return ctrl.Result{RequeueAfter: refreshInterval}, nil
}

// fetchEnvironment resolves the service token from the referenced Secret and calls Vaultic's
// environment export endpoint, which returns the fully inheritance- and reference-resolved
// key/value map for that environment (the same data `vaultic export`/`vaultic run` use) in one request.
func (r *VaulticSecretReconciler) fetchEnvironment(ctx context.Context, namespace string, vs *vaulticv1.VaulticSecret) (map[string]string, error) {
	tokenSecret := &corev1.Secret{}
	ref := vs.Spec.TokenSecretRef
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
		"%s/workspaces/%s/projects/%s/environments/%s/export",
		strings.TrimRight(vs.Spec.ServerURL, "/"), vs.Spec.Workspace, vs.Spec.Project, vs.Spec.Environment,
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
		return nil, fmt.Errorf("calling vaultic at %s: %w", vs.Spec.ServerURL, err)
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

	var values map[string]string
	if err := json.Unmarshal(body, &values); err != nil {
		return nil, fmt.Errorf("decoding vaultic response: %w", err)
	}
	return values, nil
}

// applyTargetSecret creates or updates the native v1.Secret, owned by the VaulticSecret so it's
// garbage-collected automatically when the VaulticSecret is deleted.
func (r *VaulticSecretReconciler) applyTargetSecret(ctx context.Context, vs *vaulticv1.VaulticSecret, values map[string]string) error {
	target := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vs.TargetSecretNameOrDefault(),
			Namespace: vs.Namespace,
		},
	}

	data := make(map[string][]byte, len(values))
	for k, v := range values {
		data[k] = []byte(v)
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, target, func() error {
		if target.CreationTimestamp.IsZero() {
			target.Type = corev1.SecretTypeOpaque
		}
		target.Data = data
		target.StringData = nil
		return controllerutil.SetControllerReference(vs, target, r.Scheme)
	})
	if err != nil {
		return err
	}
	return nil
}

// setReady updates the VaulticSecret's status subresource; NotFound (CR deleted mid-reconcile)
// and Conflict (a concurrent update lost the race) are both fine to swallow — the next
// reconcile picks it back up.
func (r *VaulticSecretReconciler) setReady(ctx context.Context, vs *vaulticv1.VaulticSecret, ready bool, reason, message string) error {
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
		ObservedGeneration: vs.Generation,
	}
	setCondition(&vs.Status.Conditions, meta)

	if err := r.Status().Update(ctx, vs); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return nil
		}
		return err
	}
	return nil
}

func setCondition(conditions *[]metav1.Condition, next metav1.Condition) {
	for i, c := range *conditions {
		if c.Type == next.Type {
			if c.Status == next.Status {
				next.LastTransitionTime = c.LastTransitionTime
			}
			(*conditions)[i] = next
			return
		}
	}
	*conditions = append(*conditions, next)
}

func (r *VaulticSecretReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vaulticv1.VaulticSecret{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
