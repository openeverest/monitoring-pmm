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
	"encoding/json"
	"fmt"
	"net/http"
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

	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

// credentialsKey is the destination Secret key holding the PMM 3
// service-account token.
const credentialsKey = "apiKey"

const (
	recheckInterval = 5 * time.Minute
	retryInterval   = time.Minute
	// maxMessageBytes bounds the Ready condition message.
	maxMessageBytes = 1 << 10
)

// destinationParameters is MonitoringDestination.spec.parameters of the pmm
// class.
type destinationParameters struct {
	URL       string `json:"url"`
	VerifyTLS *bool  `json:"verifyTLS,omitempty"` //nolint:tagliatelle // parameter name is part of the class schema
}

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
	client    client.Client
	secrets   client.Reader
	classes   monitoring.ClassController
	className string
	verifying *http.Client
	insecure  *http.Client
}

// NewDestinationReconciler reads destination Secrets through secrets, so the
// manager need not cache or watch any Secret.
func NewDestinationReconciler(c client.Client, secrets client.Reader, opts Options) *DestinationReconciler {
	return &DestinationReconciler{
		client:    c,
		secrets:   secrets,
		classes:   monitoring.ClassController{Client: c, ControllerName: ControllerName},
		className: opts.ClassName,
		verifying: newHTTPClient(true, opts.AllowLoopback),
		insecure:  newHTTPClient(false, opts.AllowLoopback),
	}
}

// SetupWithManager re-probes a destination when its spec or its class
// changes, and periodically after that, backing off while it keeps failing;
// the periodic probe also picks up a rotated token.
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
	if dst.Spec.ClassRef.Name != r.className {
		return ctrl.Result{}, nil
	}
	ours, err := r.classes.OwnsClass(ctx, r.className)
	if err != nil || !ours {
		return ctrl.Result{}, err
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
	params, err := decodeParameters(dst)
	if err != nil {
		return invalidParameters(err.Error()), nil
	}
	base, err := baseURL(params.URL)
	if err != nil {
		return invalidParameters(err.Error()), nil
	}

	// Without a token there is nothing to authenticate with: Ready=False,
	// Unauthorized, next to core's Accepted=False, CredentialsNotFound.
	if dst.Spec.CredentialsSecretRef == nil {
		return unauthorized("spec.credentialsSecretRef is not set"), nil
	}
	secret := &corev1.Secret{}
	if err := r.secrets.Get(ctx, client.ObjectKey{Namespace: dst.Namespace, Name: dst.Spec.CredentialsSecretRef.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return unauthorized(fmt.Sprintf("credentials Secret %q not found", dst.Spec.CredentialsSecretRef.Name)), nil
		}
		return readiness{}, fmt.Errorf("get credentials Secret %q: %w", dst.Spec.CredentialsSecretRef.Name, err)
	}
	token := strings.TrimSpace(string(secret.Data[credentialsKey]))
	if token == "" {
		return unauthorized(fmt.Sprintf("credentials Secret %q has no %q key", secret.Name, credentialsKey)), nil
	}

	httpClient := r.verifying
	if params.VerifyTLS != nil && !*params.VerifyTLS {
		httpClient = r.insecure
	}
	return probe(ctx, httpClient, base, token), nil
}

func decodeParameters(dst *monitoringv1alpha1.MonitoringDestination) (destinationParameters, error) {
	params := destinationParameters{}
	if dst.Spec.Parameters == nil {
		return params, nil
	}
	if err := json.Unmarshal(dst.Spec.Parameters.Raw, &params); err != nil {
		return params, fmt.Errorf("invalid parameters: %w", err)
	}
	return params, nil
}

func (r *DestinationReconciler) destinationsOfClass(ctx context.Context, class client.Object) []reconcile.Request {
	if class.GetName() != r.className {
		return nil
	}
	list := &monitoringv1alpha1.MonitoringDestinationList{}
	if err := r.client.List(ctx, list); err != nil {
		log.FromContext(ctx).Error(err, "listing MonitoringDestinations failed")
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.ClassRef.Name == r.className {
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
