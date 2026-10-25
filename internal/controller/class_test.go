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

package controller_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"

	"github.com/openeverest/monitoring-pmm/internal/controller"
)

func TestClassReconcileClaimsOnlyTheConfiguredClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		request     string
		wantClaimed []string
	}{
		{name: "configured class naming this controller", request: "pmm", wantClaimed: []string{"pmm"}},
		{name: "second class naming this controller", request: "pmm-eu"},
		{name: "class naming another controller", request: "prometheus"},
		{name: "class deleted", request: "datadog"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newFakeClient(t, pmmClass(), pmmClassNamed("pmm-eu"), prometheusClass())

			_, err := controller.NewClassReconciler(c, "pmm").Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: tt.request}})
			require.NoError(t, err)

			classes := &monitoringv1alpha1.MonitoringClassList{}
			require.NoError(t, c.List(t.Context(), classes))
			var claimed []string
			for _, class := range classes.Items {
				accepted := meta.FindStatusCondition(class.Status.Conditions, monitoringv1alpha1.MonitoringClassConditionAccepted)
				if accepted == nil {
					continue
				}
				assert.Equal(t, metav1.ConditionTrue, accepted.Status)
				assert.Equal(t, int64(1), accepted.ObservedGeneration)
				claimed = append(claimed, class.Name)
			}
			assert.Equal(t, tt.wantClaimed, claimed)
		})
	}
}

func TestClassReconcileReturnsFailedClaimForRetry(t *testing.T) {
	t.Parallel()
	errUnavailable := errors.New("apiserver unavailable")
	c := interceptor.NewClient(newFakeClient(t, pmmClass()), interceptor.Funcs{
		SubResourceApply: func(context.Context, client.Client, string, runtime.ApplyConfiguration, ...client.SubResourceApplyOption) error {
			return errUnavailable
		},
	})

	_, err := controller.NewClassReconciler(c, "pmm").Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "pmm"}})

	require.ErrorIs(t, err, errUnavailable)
}
