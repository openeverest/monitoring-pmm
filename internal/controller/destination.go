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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// DestinationReconciler probes the PMM server behind every destination of a
// class this controller owns and reports the result as the destination's
// Ready condition and serverVersion.
type DestinationReconciler struct {
	client    client.Client
	secrets   client.Reader
	classes   monitoring.ClassController
	verifying *http.Client
	insecure  *http.Client
}

// NewDestinationReconciler reads destination Secrets through secrets, so the
// manager need not cache every Secret in the cluster.
func NewDestinationReconciler(c client.Client, secrets client.Reader) *DestinationReconciler {
	return &DestinationReconciler{
		client:    c,
		secrets:   secrets,
		classes:   monitoring.ClassController{Client: c, ControllerName: ControllerName},
		verifying: newHTTPClient(true),
		insecure:  newHTTPClient(false),
	}
}

// SetupWithManager re-probes a destination when its spec, its credentials
// Secret or its class changes, and periodically after that.
func (r *DestinationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-pmm-destination").
		For(&monitoringv1alpha1.MonitoringDestination{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.destinationsUsingSecret), builder.OnlyMetadata).
		Watches(&monitoringv1alpha1.MonitoringClass{}, handler.EnqueueRequestsFromMapFunc(r.destinationsOfClass),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

// Reconcile writes Ready and serverVersion; it never touches core's Accepted
// condition or the bindings, whose Configured is provider-runtime's.
func (r *DestinationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	dst := &monitoringv1alpha1.MonitoringDestination{}
	if err := r.client.Get(ctx, req.NamespacedName, dst); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ours, err := r.classes.OwnsClass(ctx, dst.Spec.ClassRef.Name)
	if err != nil || !ours {
		return ctrl.Result{}, err
	}

	ready, err := r.check(ctx, dst)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.classes.SetDestinationReady(ctx, dst, ready.reason, ready.message, ready.version); err != nil {
		return ctrl.Result{}, err
	}
	if ready.reason == monitoringv1alpha1.ReasonUnreachable {
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	}
	return ctrl.Result{RequeueAfter: recheckInterval}, nil
}

func (r *DestinationReconciler) check(ctx context.Context, dst *monitoringv1alpha1.MonitoringDestination) (readiness, error) {
	params, err := decodeParameters(dst)
	if err != nil {
		return unreachable(err.Error()), nil
	}
	endpoint, err := versionEndpoint(params.URL)
	if err != nil {
		return unreachable(err.Error()), nil
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
	token := string(secret.Data[credentialsKey])
	if token == "" {
		return unauthorized(fmt.Sprintf("credentials Secret %q has no %q key", secret.Name, credentialsKey)), nil
	}

	httpClient := r.verifying
	if params.VerifyTLS != nil && !*params.VerifyTLS {
		httpClient = r.insecure
	}
	return probe(ctx, httpClient, endpoint, token), nil
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

func (r *DestinationReconciler) destinationsUsingSecret(ctx context.Context, secret client.Object) []reconcile.Request {
	return r.enqueue(ctx, func(dst *monitoringv1alpha1.MonitoringDestination) bool {
		return dst.Spec.CredentialsSecretRef != nil && dst.Spec.CredentialsSecretRef.Name == secret.GetName()
	}, client.InNamespace(secret.GetNamespace()))
}

func (r *DestinationReconciler) destinationsOfClass(ctx context.Context, class client.Object) []reconcile.Request {
	return r.enqueue(ctx, func(dst *monitoringv1alpha1.MonitoringDestination) bool {
		return dst.Spec.ClassRef.Name == class.GetName()
	})
}

func (r *DestinationReconciler) enqueue(ctx context.Context, match func(*monitoringv1alpha1.MonitoringDestination) bool, opts ...client.ListOption) []reconcile.Request {
	list := &monitoringv1alpha1.MonitoringDestinationList{}
	if err := r.client.List(ctx, list, opts...); err != nil {
		log.FromContext(ctx).Error(err, "listing MonitoringDestinations failed")
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if match(&list.Items[i]) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}

func unreachable(message string) readiness {
	return readiness{reason: monitoringv1alpha1.ReasonUnreachable, message: message}
}

func unauthorized(message string) readiness {
	return readiness{reason: monitoringv1alpha1.ReasonUnauthorized, message: message}
}
