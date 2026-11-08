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

package api_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	pmmapi "github.com/openeverest/monitoring-pmm/api"
)

func TestDecodeDestination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    pmmapi.DestinationParameters
		wantErr error
		errMsg  string
	}{
		{name: "https", raw: `{"url":"https://pmm.example.com"}`, want: pmmapi.DestinationParameters{URL: "https://pmm.example.com"}},
		{name: "http with port and path", raw: `{"url":"http://pmm:8080/base/"}`, want: pmmapi.DestinationParameters{URL: "http://pmm:8080/base/"}},
		{name: "unknown parameter", raw: `{"url":"https://pmm","serverHost":"pmm"}`, wantErr: pmmapi.ErrUnsupportedParameter, errMsg: `unknown field "serverHost"`},
		{name: "no parameters", raw: ``, wantErr: pmmapi.ErrInvalidParameters, errMsg: "url is required"},
		{name: "null", raw: `null`, wantErr: pmmapi.ErrInvalidParameters, errMsg: "url is required"},
		{name: "empty object", raw: `{}`, wantErr: pmmapi.ErrInvalidParameters, errMsg: "url is required"},
		{name: "no scheme", raw: `{"url":"pmm.example.com"}`, wantErr: pmmapi.ErrInvalidParameters, errMsg: "must be an http:// or https:// URL"},
		{name: "other scheme", raw: `{"url":"ftp://pmm.example.com"}`, wantErr: pmmapi.ErrInvalidParameters, errMsg: "must be an http:// or https:// URL"},
		{name: "user information", raw: `{"url":"https://admin@pmm.example.com"}`, wantErr: pmmapi.ErrInvalidParameters, errMsg: "without user information"},
		{name: "no host", raw: `{"url":"https:///path"}`, wantErr: pmmapi.ErrInvalidParameters, errMsg: "must be an http:// or https:// URL"},
		{name: "unparsable", raw: `{"url":"https://pmm:port"}`, wantErr: pmmapi.ErrInvalidParameters, errMsg: "invalid port"},
		{name: "wrong type", raw: `{"url":1}`, wantErr: pmmapi.ErrInvalidParameters, errMsg: "cannot unmarshal"},
		{name: "not an object", raw: `["https://pmm"]`, wantErr: pmmapi.ErrInvalidParameters},
		{name: "trailing data", raw: `{"url":"https://pmm"} {}`, wantErr: pmmapi.ErrInvalidParameters, errMsg: "data after the parameters object"},
		{name: "malformed", raw: `{"url":`, wantErr: pmmapi.ErrInvalidParameters},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := pmmapi.DecodeDestination([]byte(tt.raw))
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				require.ErrorContains(t, err, tt.errMsg)
				assert.Zero(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDecodeInstance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    pmmapi.InstanceParameters
		wantErr error
		errMsg  string
	}{
		{name: "no parameters", raw: ``},
		{name: "empty object", raw: `{}`},
		{name: "empty resources", raw: `{"resources":{}}`, want: pmmapi.InstanceParameters{Resources: &pmmapi.Resources{}}},
		{
			name: "requests and limits",
			raw:  `{"resources":{"requests":{"cpu":"100m","memory":"128Mi"},"limits":{"cpu":1,"memory":"256Mi"}}}`,
			want: pmmapi.InstanceParameters{Resources: &pmmapi.Resources{
				Requests: map[string]resource.Quantity{"cpu": resource.MustParse("100m"), "memory": resource.MustParse("128Mi")},
				Limits:   map[string]resource.Quantity{"cpu": resource.MustParse("1"), "memory": resource.MustParse("256Mi")},
			}},
		},
		{
			name: "request equal to the limit",
			raw:  `{"resources":{"requests":{"memory":"256Mi"},"limits":{"memory":"256Mi"}}}`,
			want: pmmapi.InstanceParameters{Resources: &pmmapi.Resources{
				Requests: map[string]resource.Quantity{"memory": resource.MustParse("256Mi")},
				Limits:   map[string]resource.Quantity{"memory": resource.MustParse("256Mi")},
			}},
		},
		{
			name:    "request above the limit",
			raw:     `{"resources":{"requests":{"memory":"512Mi"},"limits":{"memory":"256Mi"}}}`,
			wantErr: pmmapi.ErrInvalidParameters,
			errMsg:  "resources.requests.memory 512Mi exceeds resources.limits.memory 256Mi",
		},
		{name: "unknown parameter", raw: `{"image":"pmm-client"}`, wantErr: pmmapi.ErrUnsupportedParameter, errMsg: `unknown field "image"`},
		{name: "unknown resources field", raw: `{"resources":{"claims":[]}}`, wantErr: pmmapi.ErrUnsupportedParameter, errMsg: `unknown field "claims"`},
		{name: "invalid quantity", raw: `{"resources":{"limits":{"cpu":"lots"}}}`, wantErr: pmmapi.ErrInvalidParameters},
		{name: "wrong type", raw: `{"resources":[]}`, wantErr: pmmapi.ErrInvalidParameters, errMsg: "cannot unmarshal"},
		{name: "trailing data", raw: `{}}`, wantErr: pmmapi.ErrInvalidParameters, errMsg: "data after the parameters object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := pmmapi.DecodeInstance([]byte(tt.raw))
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				require.ErrorContains(t, err, tt.errMsg)
				assert.Zero(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
