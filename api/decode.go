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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Errors returned by the decoders. Providers report them on the binding.
var (
	// ErrUnsupportedParameter means the parameters carry a key this version of
	// the contract does not know, typically a parameter newer than the
	// provider.
	ErrUnsupportedParameter = errors.New("unsupported parameter")
	// ErrInvalidParameters means the parameters are not a JSON object of the
	// contract's shape, lack a required parameter or hold a value the
	// contract does not accept.
	ErrInvalidParameters = errors.New("invalid parameters")
)

// urlPattern is the pattern of DestinationParameters.URL, as its marker
// declares it.
var urlPattern = regexp.MustCompile(`^https?://[^/?#@]+/?$`)

// DecodeDestination decodes the raw spec.parameters of a pmm
// MonitoringDestination strictly: a key the contract does not know is
// ErrUnsupportedParameter, never ignored; anything else the schema rejects,
// and a url that does not parse, is ErrInvalidParameters.
func DecodeDestination(raw []byte) (DestinationParameters, error) {
	var p DestinationParameters
	if err := decode(raw, &p); err != nil {
		return DestinationParameters{}, err
	}
	if p.URL == "" {
		return DestinationParameters{}, fmt.Errorf("%w: url is required", ErrInvalidParameters)
	}
	if !urlPattern.MatchString(p.URL) {
		return DestinationParameters{}, fmt.Errorf("%w: url %q must be an http:// or https:// URL without user information, path, query or fragment",
			ErrInvalidParameters, p.URL,
		)
	}
	if _, err := url.Parse(p.URL); err != nil {
		return DestinationParameters{}, fmt.Errorf("%w: url: %w", ErrInvalidParameters, err)
	}
	return p, nil
}

// DecodeInstance decodes the raw parameters of an Instance's entry for a pmm
// destination strictly: a key the contract does not know is
// ErrUnsupportedParameter, never ignored; anything else the schema rejects,
// and a request above the limit of the same resource, is
// ErrInvalidParameters. Empty parameters are valid.
func DecodeInstance(raw []byte) (InstanceParameters, error) {
	var p InstanceParameters
	if err := decode(raw, &p); err != nil {
		return InstanceParameters{}, err
	}
	if r := p.Resources; r != nil {
		for _, name := range slices.Sorted(maps.Keys(r.Requests)) {
			request := r.Requests[name]
			if limit, ok := r.Limits[name]; ok && request.Cmp(limit) > 0 {
				return InstanceParameters{}, fmt.Errorf("%w: resources.requests.%s %s exceeds resources.limits.%s %s",
					ErrInvalidParameters, name, request.String(), name, limit.String(),
				)
			}
		}
	}
	return p, nil
}

// decode decodes raw into v, rejecting unknown keys at any depth and trailing
// data. Empty input leaves v unchanged.
func decode(raw []byte, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if msg, ok := strings.CutPrefix(err.Error(), "json: "); ok && strings.HasPrefix(msg, "unknown field ") {
			return fmt.Errorf("%w: %s", ErrUnsupportedParameter, msg)
		}
		return fmt.Errorf("%w: %w", ErrInvalidParameters, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: data after the parameters object", ErrInvalidParameters)
	}
	return nil
}
