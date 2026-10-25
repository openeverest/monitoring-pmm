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

// Package controller claims the MonitoringClass monitoring-pmm is configured
// to serve and keeps the Ready condition and serverVersion of its
// MonitoringDestinations current by probing the PMM server behind each one.
// Rendering pmm-client into the engine pods is the providers' job.
package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

// ControllerName is matched against MonitoringClass.spec.controllerName and
// is the field owner of everything this controller writes.
const ControllerName = "openeverest.io/monitoring-pmm"

// Options configures the controllers.
type Options struct {
	// ClassName is the one MonitoringClass this controller claims and serves.
	ClassName string
	// AllowLoopback lets the probe connect to loopback addresses.
	AllowLoopback bool
}

// ClassReconciler sets Accepted on the configured MonitoringClass when it
// names this controller.
type ClassReconciler struct {
	classes   monitoring.ClassController
	className string
}

// NewClassReconciler returns a ClassReconciler claiming className through c.
func NewClassReconciler(c client.Client, className string) *ClassReconciler {
	return &ClassReconciler{
		classes:   monitoring.ClassController{Client: c, ControllerName: ControllerName},
		className: className,
	}
}

// SetupWithManager claims on class creation and on every spec change, so
// Accepted always carries the current generation.
func (r *ClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-pmm-class").
		For(&monitoringv1alpha1.MonitoringClass{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

// Reconcile claims the configured class if it names this controller; a
// failed claim is retried with backoff. Any other class naming this
// controller stays unclaimed.
func (r *ClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	class := &monitoringv1alpha1.MonitoringClass{}
	if err := r.classes.Client.Get(ctx, req.NamespacedName, class); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if class.Name != r.className {
		if class.Spec.ControllerName == ControllerName {
			log.FromContext(ctx).Info("Not claiming a MonitoringClass that names this controller: it serves only its configured class",
				"class", class.Name, "configuredClass", r.className)
		}
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.classes.ClaimClass(ctx, class)
}

// Setup registers both controllers with the manager.
func Setup(mgr ctrl.Manager, opts Options) error {
	if err := NewClassReconciler(mgr.GetClient(), opts.ClassName).SetupWithManager(mgr); err != nil {
		return err
	}
	return NewDestinationReconciler(mgr.GetClient(), mgr.GetAPIReader(), opts).SetupWithManager(mgr)
}
