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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
)

const (
	probeTimeout = 10 * time.Second
	// maxResponseBytes bounds how much of a misbehaving server's reply is read.
	maxResponseBytes = 1 << 20
	// supportedMajor is the only PMM server major version the providers'
	// pmm-client 3 rendering works with.
	supportedMajor = "3"
)

func newHTTPClient(verifyTLS bool) *http.Client {
	return &http.Client{
		Timeout: probeTimeout,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			TLSHandshakeTimeout: probeTimeout,
			IdleConnTimeout:     90 * time.Second,
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: !verifyTLS, //nolint:gosec // verifyTLS: false is the destination owner's explicit choice
			},
		},
	}
}

// versionEndpoint returns the PMM server version API under baseURL. PMM 3
// serves /v1/version as an alias of /v1/server/version, so the same request
// also identifies a PMM 2 server.
func versionEndpoint(baseURL string) (string, error) {
	if baseURL == "" {
		return "", errors.New("parameters.url is not set")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parameters.url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("parameters.url %q is not an http(s) URL", baseURL)
	}
	return u.JoinPath("v1", "version").String(), nil
}

func probe(ctx context.Context, httpClient *http.Client, endpoint, token string) readiness {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return unreachable(err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return unreachable(err.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return unauthorized(fmt.Sprintf("PMM server rejected the token: GET %s returned HTTP %d", endpoint, resp.StatusCode))
	case resp.StatusCode != http.StatusOK:
		return unreachable(fmt.Sprintf("GET %s returned HTTP %d", endpoint, resp.StatusCode))
	}

	body := struct {
		Version string `json:"version"`
	}{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&body); err != nil || body.Version == "" {
		return unreachable(fmt.Sprintf("GET %s did not return a PMM server version", endpoint))
	}
	if major, _, _ := strings.Cut(body.Version, "."); major != supportedMajor {
		return readiness{
			status:  metav1.ConditionFalse,
			reason:  monitoringv1alpha1.ReasonUnsupportedServerVersion,
			message: fmt.Sprintf("PMM server %s is not supported; the pmm class supports PMM 3 only", body.Version),
			version: body.Version,
		}
	}
	return readiness{
		status:  metav1.ConditionTrue,
		reason:  monitoringv1alpha1.ReasonReady,
		message: fmt.Sprintf("PMM server %s is reachable and accepts the token", body.Version),
		version: body.Version,
	}
}
