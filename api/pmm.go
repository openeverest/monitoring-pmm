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

// Package api is the contract of the pmm monitoring integration: the Go types
// of the parameters of a pmm MonitoringDestination and of an Instance's entry
// for one, the integration's authentication mode and its credentials key,
// strict decoders, and the OpenAPI v3 schemas of the parameters, generated
// from the types. The pmm MonitoringClass in this repository's chart
// publishes those schemas, and every provider that renders pmm-client
// decodes the parameters with this package, so the two cannot drift.
//
// The package is a module of its own and imports only the standard library,
// the apiextensions v1 types, resource quantities and sigs.k8s.io/yaml (a
// test enforces it), so providers depend on it without depending on this
// repository's controller. Import it as pmmapi:
//
//	import pmmapi "github.com/openeverest/monitoring-pmm/api"
package api

import "k8s.io/apimachinery/pkg/api/resource"

// IntegrationName is the name of the integration: the
// spec.providerManaged.integration of the pmm MonitoringClass and the key a
// provider declares it under in Provider.spec.monitoring.integrations.
const IntegrationName = "pmm"

// AuthModeServiceAccountToken is the integration's only authentication mode:
// the token of a PMM 3 service account with the Admin role, which pmm-client
// registers the database nodes and services with.
const AuthModeServiceAccountToken = "ServiceAccountToken"

// KeyToken is the key of the service account token in the credentials Secret
// of a destination that authenticates with AuthModeServiceAccountToken.
const KeyToken = "token"

// DestinationParameters are the parameters of a pmm MonitoringDestination,
// its spec.parameters.
type DestinationParameters struct {
	// PMM Server URL, for example https://pmm.example.com, without user information.
	// +kubebuilder:validation:Pattern=`^https?://[^/?#@]+([/?#].*)?$`
	URL string `json:"url"`
}

// InstanceParameters are the parameters of an Instance's entry for a pmm
// destination, its spec.monitoring.destinations[].parameters.
type InstanceParameters struct {
	// Resource requirements of the pmm-client container.
	// +optional
	Resources *Resources `json:"resources,omitempty"`
}

// Resources are the requests and limits of the pmm-client container, keyed by
// resource name such as cpu or memory. The provider merges them over the
// requests it sizes from the Instance.
//
// +openeverest:validation:Closed
type Resources struct {
	// +optional
	Limits map[string]resource.Quantity `json:"limits,omitempty"`
	// +optional
	Requests map[string]resource.Quantity `json:"requests,omitempty"`
}
