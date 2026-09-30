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

// Package controller keeps pmm MonitoringDestinations' Ready condition current
// by probing the PMM server, and claims the pmm MonitoringClass.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/pkg/pmm"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

// ControllerName is matched against MonitoringClass.spec.controllerName.
const ControllerName = "openeverest.io/monitoring-pmm"

// ClassName is the MonitoringClass this PoC controller claims.
const ClassName = "pmm"

// credentialsKey is the Secret key carrying the PMM 3 service-account token
// (kept from the built-in PMM integration so existing Secrets keep working).
const credentialsKey = "apiKey"

// destinationParameters is MonitoringDestination.spec.parameters for the pmm
// class.
type destinationParameters struct {
	URL       string `json:"url"`
	VerifyTLS *bool  `json:"verifyTLS,omitempty"`
}

// Reconciler probes the PMM server behind each pmm MonitoringDestination.
type Reconciler struct {
	client.Client
	cc monitoring.ClassController
}

// Setup wires the controller.
func Setup(mgr ctrl.Manager) error {
	r := &Reconciler{Client: mgr.GetClient(), cc: monitoring.ClassController{Client: mgr.GetClient(), ControllerName: ControllerName}}
	ours := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		dst, ok := obj.(*monitoringv1alpha1.MonitoringDestination)
		return ok && dst.Spec.ClassRef.Name == ClassName
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-pmm").
		For(&monitoringv1alpha1.MonitoringDestination{}, builder.WithPredicates(ours)).
		Watches(&monitoringv1alpha1.MonitoringClass{}, handler.EnqueueRequestsFromMapFunc(r.claim),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

func (r *Reconciler) claim(ctx context.Context, _ client.Object) []reconcile.Request {
	if _, err := r.cc.Claim(ctx); err != nil {
		log.FromContext(ctx).Error(err, "claiming classes failed")
	}
	return nil
}

// Reconcile probes the server and writes Ready + serverVersion. It re-probes
// periodically so an outage or a token rotation shows up on the destination.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	dst := &monitoringv1alpha1.MonitoringDestination{}
	if err := r.Get(ctx, req.NamespacedName, dst); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !dst.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	version, err := r.probe(ctx, dst)
	if err != nil {
		return ctrl.Result{RequeueAfter: time.Minute},
			r.cc.SetDestinationReady(ctx, dst, metav1.ConditionFalse, "Unreachable", err.Error(), "")
	}
	if version != "" && version[0] != '3' {
		return ctrl.Result{RequeueAfter: 10 * time.Minute},
			r.cc.SetDestinationReady(ctx, dst, metav1.ConditionFalse, "UnsupportedServerVersion",
				fmt.Sprintf("PMM %s is not supported; the pmm class supports PMM 3 only", version), version)
	}
	return ctrl.Result{RequeueAfter: 10 * time.Minute},
		r.cc.SetDestinationReady(ctx, dst, metav1.ConditionTrue, "Ready", "PMM server "+version+" reachable", version)
}

func (r *Reconciler) probe(ctx context.Context, dst *monitoringv1alpha1.MonitoringDestination) (string, error) {
	params := destinationParameters{}
	if dst.Spec.Parameters != nil {
		if err := json.Unmarshal(dst.Spec.Parameters.Raw, &params); err != nil {
			return "", fmt.Errorf("decode parameters: %w", err)
		}
	}
	if params.URL == "" {
		return "", fmt.Errorf("parameters.url is required")
	}
	if dst.Spec.CredentialsSecretRef == nil {
		return "", fmt.Errorf("credentialsSecretRef is required")
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: dst.Namespace, Name: dst.Spec.CredentialsSecretRef.Name}, secret); err != nil {
		return "", fmt.Errorf("credentials Secret: %w", err)
	}
	token := secret.Data[credentialsKey]
	if len(token) == 0 {
		return "", fmt.Errorf("credentials Secret %q has no %q key", secret.Name, credentialsKey)
	}
	skipVerify := params.VerifyTLS != nil && !*params.VerifyTLS
	v, err := pmm.GetPMMServerVersion(ctx, params.URL, string(token), skipVerify)
	if err != nil {
		return "", err
	}
	return string(v), nil
}
