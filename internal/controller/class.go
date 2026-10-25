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

// Package controller claims the MonitoringClasses naming monitoring-pmm and
// keeps the Ready condition and serverVersion of their MonitoringDestinations
// current by probing the PMM server behind each one. Rendering pmm-client
// into the engine pods is the providers' job.
package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

// ControllerName is matched against MonitoringClass.spec.controllerName and
// is the field owner of everything this controller writes.
const ControllerName = "openeverest.io/monitoring-pmm"

// ClassReconciler sets Accepted on the MonitoringClasses naming this
// controller.
type ClassReconciler struct {
	classes monitoring.ClassController
}

// NewClassReconciler returns a ClassReconciler writing through c.
func NewClassReconciler(c client.Client) *ClassReconciler {
	return &ClassReconciler{classes: monitoring.ClassController{Client: c, ControllerName: ControllerName}}
}

// SetupWithManager claims on class creation and on every spec change, so
// Accepted always carries the current generation.
func (r *ClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("monitoring-pmm-class").
		For(&monitoringv1alpha1.MonitoringClass{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

// Reconcile claims every class naming this controller.
func (r *ClassReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	_, err := r.classes.Claim(ctx)
	return ctrl.Result{}, err
}

// Setup registers both controllers with the manager.
func Setup(mgr ctrl.Manager) error {
	if err := NewClassReconciler(mgr.GetClient()).SetupWithManager(mgr); err != nil {
		return err
	}
	return NewDestinationReconciler(mgr.GetClient(), mgr.GetAPIReader()).SetupWithManager(mgr)
}
