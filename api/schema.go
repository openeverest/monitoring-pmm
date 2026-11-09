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

package pmm

import (
	_ "embed"
	"fmt"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// The schemas are generated from the types with the class generator of the
// OpenEverest core module; make generate runs it and copies them into the
// chart, whose MonitoringClass publishes them. provider-sdk reads the two
// files by name at the module root, so keep their names and location.
//
//go:generate -command classgen go run github.com/openeverest/openeverest/v2/pkg/classgen/cmd@v2.0.0-dev.2.0.20261108230303-18ed25cba3bb --package .
//go:generate classgen --type DestinationParameters --output zz_generated.destination-parameters.yaml
//go:generate classgen --type InstanceParameters --output zz_generated.instance-parameters.yaml

//go:embed zz_generated.destination-parameters.yaml
var destinationParametersSchema []byte

//go:embed zz_generated.instance-parameters.yaml
var instanceParametersSchema []byte

// DestinationParametersSchema returns the OpenAPI v3 schema of
// DestinationParameters, the destinationParametersSchema of the pmm class.
func DestinationParametersSchema() *apiextensionsv1.JSONSchemaProps {
	return mustSchema("DestinationParameters", destinationParametersSchema)
}

// InstanceParametersSchema returns the OpenAPI v3 schema of
// InstanceParameters, the instanceParametersSchema of the pmm class.
func InstanceParametersSchema() *apiextensionsv1.JSONSchemaProps {
	return mustSchema("InstanceParameters", instanceParametersSchema)
}

func mustSchema(typeName string, data []byte) *apiextensionsv1.JSONSchemaProps {
	s := &apiextensionsv1.JSONSchemaProps{}
	if err := yaml.UnmarshalStrict(data, s); err != nil {
		panic(fmt.Sprintf("the embedded schema of %s is invalid, run go generate: %v", typeName, err))
	}
	return s
}
