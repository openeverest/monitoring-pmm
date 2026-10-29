// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	common "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

const (
	// authMode is the pmm class's only authentication mode.
	authMode = "ServiceAccountToken"
	// credentialsKey is the credentials Secret key holding the PMM 3
	// service-account token.
	credentialsKey = "token"
)

const (
	recheckInterval = 5 * time.Minute
	retryInterval   = time.Minute
	// maxMessageBytes bounds the Ready condition message.
	maxMessageBytes = 1 << 10
)

// readiness is the Ready condition and serverVersion of a destination.
type readiness struct {
	reason  monitoringv1alpha1.ReadyReason
	message string
	version string
}

// DestinationReconciler probes the PMM server behind every destination of
// the class this controller serves and reports the result as the
// destination's Ready condition and serverVersion.
type DestinationReconciler struct {
	client        client.Client
	secrets       client.Reader
	classes       monitoring.ClassController
	allowLoopback bool
}

// NewDestinationReconciler reads destination Secrets through secrets, so the
// manager need not cache or watch any Secret.
func NewDestinationReconciler(c client.Client, secrets client.Reader, opts Options) *DestinationReconciler {
	return &DestinationReconciler{
		client:        c,
		secrets:       secrets,
		classes:       monitoring.ClassController{Client: c, FieldOwner: FieldOwner, ClassName: opts.ClassName},
		allowLoopback: opts.AllowLoopback,
	}
}

// SetupWithManager re-probes a destination when its spec or its class
// changes, and periodically after that, backing off while it keeps failing;
// the periodic probe also picks up a rotated token or CA bundle.
func (r *DestinationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-pmm-destination").
		For(&monitoringv1alpha1.MonitoringDestination{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&monitoringv1alpha1.MonitoringClass{}, handler.EnqueueRequestsFromMapFunc(r.destinationsOfClass),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 4}).
		Complete(r)
}

// Reconcile writes Ready and serverVersion; it never touches core's Accepted
// condition or the bindings, whose Configured is provider-runtime's.
func (r *DestinationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	dst := &monitoringv1alpha1.MonitoringDestination{}
	if err := r.client.Get(ctx, req.NamespacedName, dst); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if dst.Spec.ClassRef.Name != r.classes.ClassName {
		return ctrl.Result{}, nil
	}

	ready, err := r.check(ctx, dst)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.classes.SetDestinationReady(ctx, dst, ready.reason, truncate(ready.message), ready.version); err != nil {
		return ctrl.Result{}, err
	}
	if ready.reason != monitoringv1alpha1.ReasonReady {
		return ctrl.Result{RequeueAfter: retryAfter(dst)}, nil
	}
	return ctrl.Result{RequeueAfter: recheckInterval}, nil
}

// retryAfter waits as long as dst has been failing before this probe, from
// retryInterval up to recheckInterval.
func retryAfter(dst *monitoringv1alpha1.MonitoringDestination) time.Duration {
	ready := meta.FindStatusCondition(dst.Status.Conditions, monitoringv1alpha1.MonitoringDestinationConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		return retryInterval
	}
	return min(max(time.Since(ready.LastTransitionTime.Time), retryInterval), recheckInterval)
}

func (r *DestinationReconciler) check(ctx context.Context, dst *monitoringv1alpha1.MonitoringDestination) (readiness, error) {
	base, err := baseURL(dst.Spec.URL)
	if err != nil {
		return invalidParameters(err.Error()), nil
	}
	if mode := dst.Spec.Authentication.Mode; mode != authMode {
		return invalidParameters(fmt.Sprintf("authentication mode %q is not supported: the PMM server check authenticates with %s only", mode, authMode)), nil
	}
	tlsConfig, invalid, err := r.tlsConfig(ctx, dst)
	if err != nil {
		return readiness{}, err
	}
	if invalid != "" {
		return invalidParameters(invalid), nil
	}

	// Without a token there is nothing to authenticate with: Ready=False,
	// Unauthorized, next to core's Accepted=False, CredentialsNotFound.
	ref := dst.Spec.Authentication.CredentialsSecretRef
	if ref == nil {
		return unauthorized("spec.authentication.credentialsSecretRef is not set"), nil
	}
	secret := &corev1.Secret{}
	if err := r.secrets.Get(ctx, client.ObjectKey{Namespace: dst.Namespace, Name: ref.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return unauthorized(fmt.Sprintf("credentials Secret %q not found", ref.Name)), nil
		}
		return readiness{}, fmt.Errorf("get credentials Secret %q: %w", ref.Name, err)
	}
	token := strings.TrimSpace(string(secret.Data[credentialsKey]))
	if token == "" {
		return unauthorized(fmt.Sprintf("credentials Secret %q has no %q key", secret.Name, credentialsKey)), nil
	}

	httpClient := newHTTPClient(tlsConfig, r.allowLoopback)
	defer httpClient.CloseIdleConnections()
	return probe(ctx, httpClient, base, token), nil
}

// tlsConfig is how the probe verifies the server, from spec.tls. The string
// says why spec.tls cannot be honoured; the probe then does not run.
func (r *DestinationReconciler) tlsConfig(ctx context.Context, dst *monitoringv1alpha1.MonitoringDestination) (*tls.Config, string, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if dst.Spec.TLS == nil {
		return cfg, "", nil
	}
	cfg.InsecureSkipVerify = dst.Spec.TLS.InsecureSkipVerify
	if dst.Spec.TLS.CA == nil || dst.Spec.TLS.CA.SecretKeyRef == nil {
		return cfg, "", nil
	}

	ref := dst.Spec.TLS.CA.SecretKeyRef
	secret := &corev1.Secret{}
	if err := r.secrets.Get(ctx, client.ObjectKey{Namespace: dst.Namespace, Name: ref.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Sprintf("CA bundle key %q of Secret %q: Secret not found", ref.Key, ref.Name), nil
		}
		return nil, "", fmt.Errorf("get CA bundle Secret %q: %w", ref.Name, err)
	}
	bundle, ok := secret.Data[ref.Key]
	if !ok {
		return nil, fmt.Sprintf("CA bundle key %q of Secret %q: key not found", ref.Key, ref.Name), nil
	}
	pool, err := common.ParseCABundle(bundle)
	if err != nil {
		return nil, fmt.Sprintf("CA bundle key %q of Secret %q: %v", ref.Key, ref.Name, err), nil
	}
	cfg.RootCAs = pool
	return cfg, "", nil
}

func (r *DestinationReconciler) destinationsOfClass(ctx context.Context, class client.Object) []reconcile.Request {
	if class.GetName() != r.classes.ClassName {
		return nil
	}
	list := &monitoringv1alpha1.MonitoringDestinationList{}
	if err := r.client.List(ctx, list); err != nil {
		log.FromContext(ctx).Error(err, "listing MonitoringDestinations failed")
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.ClassRef.Name == r.classes.ClassName {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}

// truncate keeps a message within maxMessageBytes.
func truncate(message string) string {
	if len(message) <= maxMessageBytes {
		return message
	}
	return strings.ToValidUTF8(message[:maxMessageBytes-3], "") + "..."
}

func unreachable(message string) readiness {
	return readiness{reason: monitoringv1alpha1.ReasonUnreachable, message: message}
}

func unauthorized(message string) readiness {
	return readiness{reason: monitoringv1alpha1.ReasonUnauthorized, message: message}
}

func invalidParameters(message string) readiness {
	return readiness{reason: monitoringv1alpha1.ReasonReadyInvalidParameters, message: message}
}
