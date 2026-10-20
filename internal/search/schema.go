// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package search

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// schemaProp is the part of a JSON Schema property the check needs.
type schemaProp struct {
	Type    json.RawMessage   `json:"type"`
	Minimum *float64          `json:"minimum"`
	Maximum *float64          `json:"maximum"`
	Enum    []json.RawMessage `json:"enum"`
}

// CheckSchema refuses a search space that does not fit the train_config schema of the training template (#168):
// every parameter must be a setting of the template, of a type that fits, and its bounds or values must lie
// inside what the template allows. A schema without properties is an error (the template cannot be searched).
func CheckSchema(s Space, schema json.RawMessage) error {
	var sc struct {
		Properties map[string]schemaProp `json:"properties"`
	}
	if err := json.Unmarshal(schema, &sc); err != nil || len(sc.Properties) == 0 {
		return errors.New("the training template has no readable configuration schema")
	}
	for _, name := range s.Names() {
		spec := s[name]
		prop, ok := sc.Properties[name]
		if !ok {
			return fmt.Errorf("%s is not a setting of this training template", name)
		}
		if typ, ok := schemaType(prop.Type); ok && !typeFits(spec, typ) {
			return fmt.Errorf("%s: the template expects a %s, the search space gives a %s", name, typ, spec.Type)
		}
		switch spec.Type {
		case TypeFloat, TypeInt:
			if prop.Minimum != nil && *spec.Min < *prop.Minimum || prop.Maximum != nil && *spec.Max > *prop.Maximum {
				return fmt.Errorf("%s: %s exceeds what the template allows (%s)", name, spec.String(), schemaBounds(prop))
			}
		case TypeChoice:
			if len(prop.Enum) == 0 {
				continue
			}
			allowed := map[string]bool{}
			for _, e := range prop.Enum {
				allowed[choiceKey(e)] = true
			}
			for _, v := range spec.Values {
				if !allowed[choiceKey(v)] {
					return fmt.Errorf("%s: %s is not one of the values the template allows", name, string(v))
				}
			}
		}
	}
	return nil
}

func schemaType(raw json.RawMessage) (string, bool) {
	var t string
	if json.Unmarshal(raw, &t) != nil || t == "" {
		return "", false
	}
	return t, true
}

func typeFits(spec ParamSpec, typ string) bool {
	switch spec.Type {
	case TypeFloat:
		return typ == "number"
	case TypeInt:
		return typ == "integer" || typ == "number"
	case TypeBool:
		return typ == "boolean"
	case TypeChoice:
		for _, v := range spec.Values {
			if !valueFits(v, typ) {
				return false
			}
		}
		return true
	}
	return false
}

func valueFits(v json.RawMessage, typ string) bool {
	k := choiceKey(v)
	switch {
	case len(k) < 2:
		return false
	case k[0] == 'n':
		if typ == "number" {
			return true
		}
		if typ != "integer" {
			return false
		}
		f, err := strconv.ParseFloat(k[2:], 64)
		return err == nil && f == float64(int64(f))
	case k[0] == 's':
		return typ == "string"
	case k[0] == 'b':
		return typ == "boolean"
	}
	return false
}

func schemaBounds(p schemaProp) string {
	lo, hi := "-inf", "+inf"
	if p.Minimum != nil {
		lo = strconv.FormatFloat(*p.Minimum, 'g', -1, 64)
	}
	if p.Maximum != nil {
		hi = strconv.FormatFloat(*p.Maximum, 'g', -1, 64)
	}
	return lo + " to " + hi
}
