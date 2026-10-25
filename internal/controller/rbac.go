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

// Run `make generate` to regenerate config/rbac/role.yaml and the chart's
// ClusterRole rules from these markers. Leader election runs on a namespaced
// Role the chart ships separately.

// +kubebuilder:rbac:groups=monitoring.openeverest.io,resources=monitoringclasses;monitoringdestinations,verbs=get;list;watch
// +kubebuilder:rbac:groups=monitoring.openeverest.io,resources=monitoringclasses/status;monitoringdestinations/status,verbs=patch

// The destination Secret carries the PMM token the probe authenticates with.
// monitoring-pmm is the one monitoring extension that reads destination
// Secrets; it never reads Instance credentials.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
