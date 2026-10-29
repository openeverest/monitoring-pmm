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
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"k8s.io/apimachinery/pkg/util/version"

	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
)

const (
	probeTimeout = 10 * time.Second
	// maxResponseBytes bounds how much of a server's reply is read.
	maxResponseBytes = 64 << 10
	// supportedMajor is the only PMM server major version the providers'
	// pmm-client 3 rendering works with.
	supportedMajor = 3
)

// versionResponse is the reply of the PMM version API.
type versionResponse struct {
	Version string `json:"version"`
}

// newHTTPClient returns the client the probe connects with. It follows no
// redirects, so the token is only ever sent to the destination's own URL.
func newHTTPClient(tlsConfig *tls.Config, allowLoopback bool) *http.Client {
	return &http.Client{
		Timeout: probeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DialContext:         guardedDial(&net.Dialer{Timeout: probeTimeout}, allowLoopback),
			TLSHandshakeTimeout: probeTimeout,
			IdleConnTimeout:     90 * time.Second,
			TLSClientConfig:     tlsConfig,
		},
	}
}

// guardedDial resolves the host itself and connects only if none of its
// addresses is link-local (cloud metadata endpoints), unspecified, or
// loopback unless allowLoopback is set.
func guardedDial(dialer *net.Dialer, allowLoopback bool) func(ctx context.Context, network, address string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, addr := range addrs {
			if err := allowedAddr(addr.Unmap(), allowLoopback); err != nil {
				return nil, fmt.Errorf("refusing to connect to %s (%s): %w", host, addr.Unmap(), err)
			}
		}
		var errs []error
		for _, addr := range addrs {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.Unmap().String(), port))
			if err == nil {
				return conn, nil
			}
			errs = append(errs, err)
		}
		return nil, errors.Join(errs...)
	}
}

func allowedAddr(addr netip.Addr, allowLoopback bool) error {
	switch {
	case addr.IsLinkLocalUnicast():
		return errors.New("link-local addresses are not allowed")
	case addr.IsUnspecified():
		return errors.New("unspecified addresses are not allowed")
	case addr.IsLoopback() && !allowLoopback:
		return errors.New("loopback addresses are not allowed unless the chart value probe.allowLoopback is set")
	}
	return nil
}

// baseURL parses the destination's spec.url.
func baseURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, errors.New("spec.url is not set")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("spec.url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("spec.url %q is not an http(s) URL", raw)
	}
	return u, nil
}

// probe calls the PMM server version API and, on PMM 3, checks the token's
// role. PMM 3 serves /v1/version as an alias of /v1/server/version, so the
// same request also identifies a PMM 2 server.
func probe(ctx context.Context, httpClient *http.Client, base *url.URL, token string) readiness {
	endpoint := base.JoinPath("v1", "version").String()
	status, body, err := get(ctx, httpClient, endpoint, token)
	if err != nil {
		return unreachable(err.Error())
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return unauthorized(fmt.Sprintf("PMM server rejected the token: GET %s returned HTTP %d", endpoint, status))
	case status != http.StatusOK:
		return unreachable(fmt.Sprintf("GET %s returned HTTP %d", endpoint, status))
	}

	reply := versionResponse{}
	if err := json.Unmarshal(body, &reply); err != nil {
		return unreachable(fmt.Sprintf("GET %s did not return a PMM server version", endpoint))
	}
	v, err := version.ParseSemantic(reply.Version)
	if err != nil {
		return unreachable(fmt.Sprintf("GET %s did not return a PMM server version", endpoint))
	}
	if v.Major() != supportedMajor {
		return readiness{
			reason:  monitoringv1alpha1.ReasonUnsupportedServerVersion,
			message: fmt.Sprintf("PMM server %s is not supported; the pmm class supports PMM 3 only", v),
			version: v.String(),
		}
	}
	return checkAdminRole(ctx, httpClient, base, token, v.String())
}

// checkAdminRole reads the server settings, which PMM serves only to the
// Admin role pmm-agent registers with; a Viewer or Editor token passes the
// version check alone.
func checkAdminRole(ctx context.Context, httpClient *http.Client, base *url.URL, token, version string) readiness {
	endpoint := base.JoinPath("v1", "server", "settings").String()
	status, _, err := get(ctx, httpClient, endpoint, token)
	switch {
	case err != nil:
		return readiness{reason: monitoringv1alpha1.ReasonUnreachable, message: err.Error(), version: version}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return readiness{
			reason:  monitoringv1alpha1.ReasonUnauthorized,
			message: fmt.Sprintf("token lacks the Admin role pmm-agent needs: GET %s returned HTTP %d", endpoint, status),
			version: version,
		}
	case status != http.StatusOK:
		return readiness{
			reason:  monitoringv1alpha1.ReasonUnreachable,
			message: fmt.Sprintf("GET %s returned HTTP %d", endpoint, status),
			version: version,
		}
	}
	return readiness{
		reason:  monitoringv1alpha1.ReasonReady,
		message: fmt.Sprintf("PMM server %s is reachable and the token has the Admin role pmm-agent needs", version),
		version: version,
	}
}

// get sends an authenticated GET and returns the status code and at most
// maxResponseBytes of the body. A redirect is an error naming only the
// target host.
func get(ctx context.Context, httpClient *http.Client, endpoint, token string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		target := "an unknown location"
		if loc, err := resp.Location(); err == nil && loc.Host != "" {
			target = loc.Host
		}
		return 0, nil, fmt.Errorf("GET %s returned HTTP %d: server redirected to %s; redirects are not followed", endpoint, resp.StatusCode, target)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, fmt.Errorf("GET %s: reading the reply: %w", endpoint, err)
	}
	return resp.StatusCode, body, nil
}
