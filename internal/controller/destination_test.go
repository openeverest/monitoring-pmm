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
	testSecret      = "pmm-prod-token"    //nolint:gosec // a Secret name, not a credential
	testToken       = "glsa_test-token"   //nolint:gosec // fake token the fake PMM server accepts
	testViewerToken = "glsa_viewer-token" //nolint:gosec // fake token without the Admin role
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
	return pmmClassNamed("pmm")
}

func pmmClassNamed(name string) *monitoringv1alpha1.MonitoringClass {
	return &monitoringv1alpha1.MonitoringClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
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

// pmmServer answers the version and settings APIs like a PMM server of the
// given version where testToken has the Admin role and testViewerToken the
// Viewer role.
func pmmServer(version string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		admin := auth == "Bearer "+testToken
		switch {
		case r.URL.Path != "/v1/version" && r.URL.Path != "/v1/server/settings":
			http.NotFound(w, r)
		case !admin && auth != "Bearer "+testViewerToken:
			http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
		case r.URL.Path == "/v1/server/settings" && !admin:
			http.Error(w, `{"message":"Access denied"}`, http.StatusForbidden)
		case r.URL.Path == "/v1/server/settings":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"settings":{"telemetry_enabled":true}}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"version":%q,"server":{"version":%q}}`, version, version)
		}
	})
}

// reply answers every request with status and body.
func reply(status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
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

// reconcile serves the pmm class; httptest servers listen on loopback.
func reconcile(t *testing.T, c client.Client) (ctrl.Result, *monitoringv1alpha1.MonitoringDestination) {
	t.Helper()
	return reconcileWith(t, c, controller.Options{ClassName: "pmm", AllowLoopback: true})
}

func reconcileWith(t *testing.T, c client.Client, opts controller.Options) (ctrl.Result, *monitoringv1alpha1.MonitoringDestination) {
	t.Helper()
	res, err := controller.NewDestinationReconciler(c, c, opts).Reconcile(t.Context(), ctrl.Request{NamespacedName: destinationKey()})
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
	server := func(h http.Handler) func(t *testing.T) any {
		return func(t *testing.T) any { t.Helper(); return serve(t, h) }
	}
	oversized := `{"version":"3.8.0","padding":"` + strings.Repeat("x", 70<<10) + `"}`
	settingsFailing := http.NewServeMux()
	settingsFailing.Handle("/v1/version", pmmServer("3.8.0"))
	settingsFailing.Handle("/v1/server/settings", reply(http.StatusInternalServerError, "settings with internal details"))

	tests := []struct {
		name        string
		url         func(t *testing.T) any
		secret      *corev1.Secret
		wantReason  monitoringv1alpha1.ReadyReason
		wantMessage string
		notMessage  string
		wantVersion string
		wantRequeue time.Duration
	}{
		{
			name:        "PMM 3 server accepting the token",
			url:         server(pmmServer("3.8.0")),
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonReady,
			wantMessage: "PMM server 3.8.0 is reachable and the token has the Admin role pmm-agent needs",
			wantVersion: "3.8.0",
			wantRequeue: 5 * time.Minute,
		},
		{
			name:        "token without the Admin role",
			url:         server(pmmServer("3.8.0")),
			secret:      tokenSecret("apiKey", testViewerToken),
			wantReason:  monitoringv1alpha1.ReasonUnauthorized,
			wantMessage: "token lacks the Admin role pmm-agent needs: GET http://127.0.0.1",
			wantVersion: "3.8.0",
			wantRequeue: time.Minute,
		},
		{
			name:        "settings API failing",
			url:         server(settingsFailing),
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnreachable,
			wantMessage: "/v1/server/settings returned HTTP 500",
			notMessage:  "internal details",
			wantVersion: "3.8.0",
			wantRequeue: time.Minute,
		},
		{
			name:        "token with a trailing newline",
			url:         server(pmmServer("3.8.0")),
			secret:      tokenSecret("apiKey", testToken+"\n"),
			wantReason:  monitoringv1alpha1.ReasonReady,
			wantMessage: "PMM server 3.8.0 is reachable",
			wantVersion: "3.8.0",
			wantRequeue: 5 * time.Minute,
		},
		{
			name:        "token rejected",
			url:         server(pmmServer("3.8.0")),
			secret:      tokenSecret("apiKey", "glsa_revoked"),
			wantReason:  monitoringv1alpha1.ReasonUnauthorized,
			wantMessage: "HTTP 401",
			wantRequeue: time.Minute,
		},
		{
			name:        "PMM 2 server",
			url:         server(pmmServer("2.44.1")),
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnsupportedServerVersion,
			wantMessage: "PMM server 2.44.1 is not supported",
			wantVersion: "2.44.1",
			wantRequeue: time.Minute,
		},
		{
			name:        "major version only",
			url:         server(pmmServer("3")),
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnreachable,
			wantMessage: "did not return a PMM server version",
			wantRequeue: time.Minute,
		},
		{
			name:        "version that is not semver",
			url:         server(pmmServer("3.x")),
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnreachable,
			wantMessage: "did not return a PMM server version",
			wantRequeue: time.Minute,
		},
		{
			name:        "empty version",
			url:         server(pmmServer("")),
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnreachable,
			wantMessage: "did not return a PMM server version",
			wantRequeue: time.Minute,
		},
		{
			name:        "HTML login page",
			url:         server(reply(http.StatusOK, "<html><body>Sign in</body></html>")),
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnreachable,
			wantMessage: "did not return a PMM server version",
			notMessage:  "Sign in",
			wantRequeue: time.Minute,
		},
		{
			name:        "malformed JSON",
			url:         server(reply(http.StatusOK, `{"version":"3.8.0"`)),
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnreachable,
			wantMessage: "did not return a PMM server version",
			wantRequeue: time.Minute,
		},
		{
			name:        "reply over 64 KiB",
			url:         server(reply(http.StatusOK, oversized)),
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnreachable,
			wantMessage: "did not return a PMM server version",
			wantRequeue: time.Minute,
		},
		{
			name:        "redirect",
			url:         server(http.RedirectHandler("https://pmm.attacker.example/steal?token=1", http.StatusFound)),
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnreachable,
			wantMessage: "HTTP 302: server redirected to pmm.attacker.example; redirects are not followed",
			notMessage:  "steal",
			wantRequeue: time.Minute,
		},
		{
			name:        "server error",
			url:         server(reply(http.StatusInternalServerError, "stack trace with internal details")),
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnreachable,
			wantMessage: "HTTP 500",
			notMessage:  "internal details",
			wantRequeue: time.Minute,
		},
		{
			name:        "not a PMM server",
			url:         server(http.NotFoundHandler()),
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnreachable,
			wantMessage: "HTTP 404",
			wantRequeue: time.Minute,
		},
		{
			name:        "connection refused",
			url:         func(t *testing.T) any { t.Helper(); return closedServer(t) },
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnreachable,
			wantMessage: "connection refused",
			wantRequeue: time.Minute,
		},
		{
			name:        "URL without a scheme",
			url:         func(*testing.T) any { return "pmm.example.com" },
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonReadyInvalidParameters,
			wantMessage: `"pmm.example.com" is not an http(s) URL`,
			wantRequeue: time.Minute,
		},
		{
			name:        "URL that is not a string",
			url:         func(*testing.T) any { return 8443 },
			secret:      tokenSecret("apiKey", testToken),
			wantReason:  monitoringv1alpha1.ReasonReadyInvalidParameters,
			wantMessage: "invalid parameters",
			wantRequeue: time.Minute,
		},
		{
			name:        "credentials Secret missing",
			url:         func(t *testing.T) any { t.Helper(); return unexpectedServer(t) },
			wantReason:  monitoringv1alpha1.ReasonUnauthorized,
			wantMessage: `credentials Secret "pmm-prod-token" not found`,
			wantRequeue: time.Minute,
		},
		{
			name:        "credentials Secret without apiKey",
			url:         func(t *testing.T) any { t.Helper(); return unexpectedServer(t) },
			secret:      tokenSecret("token", testToken),
			wantReason:  monitoringv1alpha1.ReasonUnauthorized,
			wantMessage: `credentials Secret "pmm-prod-token" has no "apiKey" key`,
			wantRequeue: time.Minute,
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
			assert.Equal(t, tt.wantReason.Status(), ready.Status)
			assert.Contains(t, ready.Message, tt.wantMessage)
			if tt.notMessage != "" {
				assert.NotContains(t, ready.Message, tt.notMessage)
			}
			assert.Equal(t, int64(1), ready.ObservedGeneration)
			assert.Equal(t, tt.wantVersion, dst.Status.ServerVersion)
			assert.Equal(t, tt.wantRequeue, res.RequeueAfter)
		})
	}
}

func TestDestinationRetryBacksOffWhileFailing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		previous metav1.ConditionStatus
		since    time.Duration
		want     time.Duration
	}{
		{name: "first probe", want: time.Minute},
		{name: "ready until now", previous: metav1.ConditionTrue, since: time.Hour, want: time.Minute},
		{name: "failing for 30 seconds", previous: metav1.ConditionFalse, since: 30 * time.Second, want: time.Minute},
		{name: "failing for 3 minutes", previous: metav1.ConditionFalse, since: 3 * time.Minute, want: 3 * time.Minute},
		{name: "failing for an hour", previous: metav1.ConditionFalse, since: time.Hour, want: 5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dst := destination(t, "pmm", map[string]any{"url": serve(t, pmmServer("3.8.0"))})
			if tt.previous != "" {
				dst.Status.Conditions = []metav1.Condition{{
					Type:               monitoringv1alpha1.MonitoringDestinationConditionReady,
					Status:             tt.previous,
					Reason:             "Previous",
					LastTransitionTime: metav1.NewTime(time.Now().Add(-tt.since)),
				}}
			}

			res, got := reconcile(t, newFakeClient(t, pmmClass(), dst, tokenSecret("apiKey", "glsa_revoked")))

			assert.Equal(t, string(monitoringv1alpha1.ReasonUnauthorized), readyCondition(t, got).Reason)
			assert.InDelta(t, float64(tt.want), float64(res.RequeueAfter), float64(5*time.Second))
		})
	}
}

func TestDestinationProbeRefusesAddresses(t *testing.T) {
	t.Parallel()

	loopbackServer := func(t *testing.T) string { t.Helper(); return serve(t, rejectRequests(t)) }
	localhostServer := func(t *testing.T) string {
		t.Helper()
		return strings.Replace(serve(t, rejectRequests(t)), "127.0.0.1", "localhost", 1)
	}

	tests := []struct {
		name        string
		url         func(t *testing.T) string
		wantMessage string
	}{
		{name: "cloud metadata endpoint", url: func(*testing.T) string { return "http://169.254.169.254" }, wantMessage: "link-local addresses are not allowed"},
		{name: "IPv6 link-local address", url: func(*testing.T) string { return "http://[fe80::1]:8080" }, wantMessage: "link-local addresses are not allowed"},
		{name: "IPv4-mapped link-local address", url: func(*testing.T) string { return "http://[::ffff:169.254.169.254]" }, wantMessage: "link-local addresses are not allowed"},
		{name: "unspecified address", url: func(*testing.T) string { return "http://0.0.0.0:8080" }, wantMessage: "unspecified addresses are not allowed"},
		{name: "loopback address", url: loopbackServer, wantMessage: "loopback addresses are not allowed"},
		{name: "localhost", url: localhostServer, wantMessage: "loopback addresses are not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newFakeClient(t, pmmClass(), destination(t, "pmm", map[string]any{"url": tt.url(t)}), tokenSecret("apiKey", testToken))

			res, dst := reconcileWith(t, c, controller.Options{ClassName: "pmm"})

			ready := readyCondition(t, dst)
			assert.Equal(t, string(monitoringv1alpha1.ReasonUnreachable), ready.Reason)
			assert.Contains(t, ready.Message, "refusing to connect to")
			assert.Contains(t, ready.Message, tt.wantMessage)
			assert.Equal(t, time.Minute, res.RequeueAfter)
		})
	}
}

func TestDestinationProbeReachesLoopbackWhenAllowed(t *testing.T) {
	t.Parallel()
	url := strings.Replace(serve(t, pmmServer("3.8.0")), "127.0.0.1", "localhost", 1)
	c := newFakeClient(t, pmmClass(), destination(t, "pmm", map[string]any{"url": url}), tokenSecret("apiKey", testToken))

	_, dst := reconcileWith(t, c, controller.Options{ClassName: "pmm", AllowLoopback: true})

	assert.Equal(t, string(monitoringv1alpha1.ReasonReady), readyCondition(t, dst).Reason)
	assert.Equal(t, "3.8.0", dst.Status.ServerVersion)
}

func TestDestinationReadyMessageIsTruncated(t *testing.T) {
	t.Parallel()
	url := serve(t, http.NotFoundHandler()) + "/" + strings.Repeat("a", 2<<10)
	c := newFakeClient(t, pmmClass(), destination(t, "pmm", map[string]any{"url": url}), tokenSecret("apiKey", testToken))

	_, dst := reconcile(t, c)

	ready := readyCondition(t, dst)
	assert.Equal(t, string(monitoringv1alpha1.ReasonUnreachable), ready.Reason)
	assert.Len(t, ready.Message, 1<<10)
	assert.True(t, strings.HasSuffix(ready.Message, "..."), ready.Message)
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
		{name: "second class naming this controller", class: "pmm-eu"},
		{name: "class not installed", class: "datadog"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, dst := reconcile(t, newFakeClient(t, pmmClass(), pmmClassNamed("pmm-eu"), prometheusClass(),
				destination(t, tt.class, map[string]any{"url": serve(t, rejectRequests(t))}), tokenSecret("apiKey", testToken)))

			assert.Zero(t, res.RequeueAfter)
			assert.Empty(t, dst.Status.Conditions)
			assert.Empty(t, dst.Status.ServerVersion)
		})
	}
}
