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
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/managedfields"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/kube-openapi/pkg/validation/spec"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	common "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"

	"github.com/openeverest/monitoring-pmm/internal/controller"
)

const (
	testNamespace   = "team-a"
	testDestination = "pmm-prod"
	testSecret      = "pmm-prod-token"  //nolint:gosec // a Secret name, not a credential
	testToken       = "glsa_test-token" //nolint:gosec // fake token the fake PMM server accepts
)

func destinationKey() client.ObjectKey {
	return client.ObjectKey{Namespace: testNamespace, Name: testDestination}
}

func newFakeClient(t *testing.T, objs ...client.Object) client.WithWatch { //nolint:ireturn // the fake is used through the client interface
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, monitoringv1alpha1.AddToScheme(s))
	return fake.NewClientBuilder().
		WithScheme(s).
		WithTypeConverters(crdTypeConverter(t), managedfields.NewDeducedTypeConverter()).
		WithObjects(objs...).
		WithStatusSubresource(&monitoringv1alpha1.MonitoringClass{}, &monitoringv1alpha1.MonitoringDestination{}).
		Build()
}

// crdTypeConverter makes the fake client's server-side apply follow core's
// CRD schemas (conditions merged by type) instead of treating every list as
// atomic. The CRDs come from the core module go.mod pins.
func crdTypeConverter(t *testing.T) managedfields.TypeConverter { //nolint:ireturn // the fake client takes the interface
	t.Helper()
	dir, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}", "github.com/openeverest/openeverest/v2").Output()
	require.NoError(t, err)
	crds := os.DirFS(filepath.Join(strings.TrimSpace(string(dir)), "config", "crd", "bases"))
	models := map[string]*spec.Schema{}
	for _, plural := range []string{"monitoringclasses", "monitoringdestinations"} {
		raw, err := fs.ReadFile(crds, "monitoring.openeverest.io_"+plural+".yaml")
		require.NoError(t, err)
		crd := &apiextensionsv1.CustomResourceDefinition{}
		require.NoError(t, yaml.Unmarshal(raw, crd))
		for _, v := range crd.Spec.Versions {
			js, err := json.Marshal(v.Schema.OpenAPIV3Schema)
			require.NoError(t, err)
			s := &spec.Schema{}
			require.NoError(t, json.Unmarshal(js, s))
			s.AddExtension("x-kubernetes-group-version-kind", []any{
				map[string]any{"group": crd.Spec.Group, "version": v.Name, "kind": crd.Spec.Names.Kind},
			})
			models[crd.Spec.Group+"."+v.Name+"."+crd.Spec.Names.Kind] = s
		}
	}
	tc, err := managedfields.NewTypeConverter(models, false)
	require.NoError(t, err)
	return tc
}

func pmmClass() *monitoringv1alpha1.MonitoringClass {
	return &monitoringv1alpha1.MonitoringClass{
		ObjectMeta: metav1.ObjectMeta{Name: "pmm", Generation: 1},
		Spec: monitoringv1alpha1.MonitoringClassSpec{
			ControllerName:  controller.ControllerName,
			ProviderManaged: &monitoringv1alpha1.ProviderManagedFulfilment{Integration: "pmm"},
		},
	}
}

func prometheusClass() *monitoringv1alpha1.MonitoringClass {
	return &monitoringv1alpha1.MonitoringClass{
		ObjectMeta: metav1.ObjectMeta{Name: "prometheus", Generation: 1},
		Spec: monitoringv1alpha1.MonitoringClassSpec{
			ControllerName:   "openeverest.io/monitoring-prometheus",
			ExtensionManaged: &monitoringv1alpha1.ExtensionManagedFulfilment{},
		},
	}
}

func destination(t *testing.T, class string, params map[string]any) *monitoringv1alpha1.MonitoringDestination {
	t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	return &monitoringv1alpha1.MonitoringDestination{
		ObjectMeta: metav1.ObjectMeta{Name: testDestination, Namespace: testNamespace, Generation: 1},
		Spec: monitoringv1alpha1.MonitoringDestinationSpec{
			ClassRef:             common.ObjectRef{Name: class},
			CredentialsSecretRef: &common.SecretRef{Name: testSecret},
			Parameters:           &runtime.RawExtension{Raw: raw},
		},
	}
}

func tokenSecret(key, value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testSecret, Namespace: testNamespace},
		Data:       map[string][]byte{key: []byte(value)},
	}
}

// pmmServer answers the version API like a PMM server of the given version
// that accepts only testToken.
func pmmServer(version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/version" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"version":%q,"server":{"version":%q}}`, version, version)
	})
}

func rejectRequests(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to the PMM server: %s %s", r.Method, r.URL)
	})
}

func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s.URL
}

func reconcile(t *testing.T, c client.Client) (ctrl.Result, *monitoringv1alpha1.MonitoringDestination) {
	t.Helper()
	res, err := controller.NewDestinationReconciler(c, c).Reconcile(t.Context(), ctrl.Request{NamespacedName: destinationKey()})
	require.NoError(t, err)
	dst := &monitoringv1alpha1.MonitoringDestination{}
	require.NoError(t, c.Get(t.Context(), destinationKey(), dst))
	return res, dst
}

func readyCondition(t *testing.T, dst *monitoringv1alpha1.MonitoringDestination) metav1.Condition {
	t.Helper()
	ready := meta.FindStatusCondition(dst.Status.Conditions, monitoringv1alpha1.MonitoringDestinationConditionReady)
	require.NotNil(t, ready)
	return *ready
}

func TestDestinationReadiness(t *testing.T) {
	t.Parallel()

	closedServer := func(t *testing.T) string {
		t.Helper()
		s := httptest.NewServer(rejectRequests(t))
		s.Close()
		return s.URL
	}
	unexpectedServer := func(t *testing.T) string { t.Helper(); return serve(t, rejectRequests(t)) }

	tests := []struct {
		name        string
		url         func(t *testing.T) string
		secret      *corev1.Secret
		wantReason  monitoringv1alpha1.ReadyReason
		wantMessage string
		wantVersion string
		wantRequeue time.Duration
	}{
		{
			name:        "PMM 3 server accepting the token",
			url:         func(t *testing.T) string { t.Helper(); return serve(t, pmmServer("3.8.0")) },
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonReady,
			wantMessage: "PMM server 3.8.0 is reachable",
			wantVersion: "3.8.0",
			wantRequeue: 5 * time.Minute,
		},
		{
			name:        "token rejected",
			url:         func(t *testing.T) string { t.Helper(); return serve(t, pmmServer("3.8.0")) },
			secret:      tokenSecret("apiKey", "glsa_revoked"),
			wantReason:  monitoringv1alpha1.ReasonUnauthorized,
			wantMessage: "HTTP 401",
			wantRequeue: 5 * time.Minute,
		},
		{
			name:        "PMM 2 server",
			url:         func(t *testing.T) string { t.Helper(); return serve(t, pmmServer("2.44.1")) },
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnsupportedServerVersion,
			wantMessage: "PMM server 2.44.1 is not supported",
			wantVersion: "2.44.1",
			wantRequeue: 5 * time.Minute,
		},
		{
			name:        "not a PMM server",
			url:         func(t *testing.T) string { t.Helper(); return serve(t, http.NotFoundHandler()) },
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnreachable,
			wantMessage: "HTTP 404",
			wantRequeue: time.Minute,
		},
		{
			name:        "connection refused",
			url:         closedServer,
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnreachable,
			wantMessage: "connection refused",
			wantRequeue: time.Minute,
		},
		{
			name:        "URL without a scheme",
			url:         func(*testing.T) string { return "pmm.example.com" },
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnreachable,
			wantMessage: `"pmm.example.com" is not an http(s) URL`,
			wantRequeue: time.Minute,
		},
		{
			name:        "credentials Secret missing",
			url:         unexpectedServer,
			wantReason:  monitoringv1alpha1.ReasonUnauthorized,
			wantMessage: `credentials Secret "pmm-prod-token" not found`,
			wantRequeue: 5 * time.Minute,
		},
		{
			name:        "credentials Secret without apiKey",
			url:         unexpectedServer,
			secret:      tokenSecret("token", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnauthorized,
			wantMessage: `credentials Secret "pmm-prod-token" has no "apiKey" key`,
			wantRequeue: 5 * time.Minute,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			objs := []client.Object{pmmClass(), destination(t, "pmm", map[string]any{"url": tt.url(t)})}
			if tt.secret != nil {
				objs = append(objs, tt.secret)
			}

			res, dst := reconcile(t, newFakeClient(t, objs...))

			ready := readyCondition(t, dst)
			assert.Equal(t, string(tt.wantReason), ready.Reason)
			assert.Contains(t, ready.Message, tt.wantMessage)
			assert.Equal(t, int64(1), ready.ObservedGeneration)
			assert.Equal(t, tt.wantVersion, dst.Status.ServerVersion)
			assert.Equal(t, tt.wantRequeue, res.RequeueAfter)
		})
	}
}

func TestDestinationVerifyTLS(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		params     map[string]any
		wantReason monitoringv1alpha1.ReadyReason
	}{
		{name: "verification disabled", params: map[string]any{"verifyTLS": false}, wantReason: monitoringv1alpha1.ReasonReady},
		{name: "verification enabled", params: map[string]any{"verifyTLS": true}, wantReason: monitoringv1alpha1.ReasonUnreachable},
		{name: "verification by default", params: map[string]any{}, wantReason: monitoringv1alpha1.ReasonUnreachable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := httptest.NewTLSServer(pmmServer("3.8.0"))
			t.Cleanup(s.Close)
			tt.params["url"] = s.URL

			_, dst := reconcile(t, newFakeClient(t, pmmClass(), destination(t, "pmm", tt.params), tokenSecret("apiKey", testToken)))

			ready := readyCondition(t, dst)
			assert.Equal(t, string(tt.wantReason), ready.Reason)
			if tt.wantReason == monitoringv1alpha1.ReasonUnreachable {
				assert.Contains(t, ready.Message, "certificate")
			}
		})
	}
}

func TestDestinationReadyKeepsAcceptedAndServerVersion(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	c := newFakeClient(t, pmmClass(), destination(t, "pmm", map[string]any{"url": serve(t, pmmServer("3.8.0"))}),
		tokenSecret("apiKey", testToken))

	dst := &monitoringv1alpha1.MonitoringDestination{}
	require.NoError(t, c.Get(ctx, destinationKey(), dst))
	accepted := monitoring.NewCondition(dst, monitoringv1alpha1.MonitoringDestinationConditionAccepted, metav1.ConditionTrue, monitoringv1alpha1.ReasonAccepted, "")
	require.NoError(t, monitoring.ApplyCondition(ctx, c, dst, dst.Status.Conditions, monitoring.MaterializerFieldOwner, accepted))

	_, dst = reconcile(t, c)
	require.Equal(t, "3.8.0", dst.Status.ServerVersion)

	secret := &corev1.Secret{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testSecret}, secret))
	secret.Data["apiKey"] = []byte("glsa_rotated")
	require.NoError(t, c.Update(ctx, secret))

	reconcile(t, c)
	_, dst = reconcile(t, c)

	ready := readyCondition(t, dst)
	assert.Equal(t, string(monitoringv1alpha1.ReasonUnauthorized), ready.Reason)
	assert.Equal(t, "3.8.0", dst.Status.ServerVersion)
	gotAccepted := meta.FindStatusCondition(dst.Status.Conditions, monitoringv1alpha1.MonitoringDestinationConditionAccepted)
	require.NotNil(t, gotAccepted)
	assert.Equal(t, metav1.ConditionTrue, gotAccepted.Status)
	assert.Equal(t, monitoringv1alpha1.ReasonAccepted, gotAccepted.Reason)
}

func TestDestinationOfOtherClassIsLeftAlone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		class string
	}{
		{name: "class claimed by another controller", class: "prometheus"},
		{name: "class not installed", class: "datadog"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, dst := reconcile(t, newFakeClient(t, pmmClass(), prometheusClass(),
				destination(t, tt.class, map[string]any{"url": serve(t, rejectRequests(t))}), tokenSecret("apiKey", testToken)))

			assert.Zero(t, res.RequeueAfter)
			assert.Empty(t, dst.Status.Conditions)
			assert.Empty(t, dst.Status.ServerVersion)
		})
	}
}
