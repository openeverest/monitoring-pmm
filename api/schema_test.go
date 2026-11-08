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

package api

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// jsonFields returns the JSON names of the fields of the struct t, and those
// without omitempty.
func jsonFields(t reflect.Type) ([]string, []string) {
	names := make([]string, 0, t.NumField())
	var required []string
	for f := range t.Fields() {
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		names = append(names, name)
		if !strings.Contains(opts, "omitempty") {
			required = append(required, name)
		}
	}
	return names, required
}

// assertSchemaOf checks that s is an object with the properties and required
// properties of the struct t, so the schema the class publishes and the
// decoder read the same keys.
func assertSchemaOf(t *testing.T, s *apiextensionsv1.JSONSchemaProps, typ reflect.Type) {
	t.Helper()
	names, required := jsonFields(typ)
	assert.Equal(t, "object", s.Type)
	assert.ElementsMatch(t, names, slices.Collect(maps.Keys(s.Properties)))
	assert.ElementsMatch(t, required, s.Required)
}

func TestDestinationParametersSchema(t *testing.T) {
	t.Parallel()

	s := DestinationParametersSchema()
	assertSchemaOf(t, s, reflect.TypeFor[DestinationParameters]())
	assert.Equal(t, urlPattern.String(), s.Properties["url"].Pattern, "the decoder checks the pattern the schema declares")
}

func TestInstanceParametersSchema(t *testing.T) {
	t.Parallel()

	s := InstanceParametersSchema()
	assertSchemaOf(t, s, reflect.TypeFor[InstanceParameters]())
	resources := s.Properties["resources"]
	assertSchemaOf(t, &resources, reflect.TypeFor[Resources]())
	require.NotNil(t, resources.AdditionalProperties)
	assert.False(t, resources.AdditionalProperties.Allows, "resources rejects keys it does not declare, as the decoder does")
	for _, list := range []string{"limits", "requests"} {
		quantity := resources.Properties[list].AdditionalProperties
		require.NotNil(t, quantity, list)
		require.NotNil(t, quantity.Schema, list)
		assert.True(t, quantity.Schema.XIntOrString, list)
	}
}

func TestSchemasAreCopies(t *testing.T) {
	t.Parallel()

	DestinationParametersSchema().Properties["url"] = apiextensionsv1.JSONSchemaProps{}
	InstanceParametersSchema().Properties = nil
	assert.NotEmpty(t, DestinationParametersSchema().Properties["url"].Pattern)
	assert.Contains(t, InstanceParametersSchema().Properties, "resources")
}
