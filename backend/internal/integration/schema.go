package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"unicode/utf8"
)

// This deliberately supports only the schema subset used by AgentOwl tools and outputs.
// Unsupported keywords fail before an HTTP call instead of silently weakening validation.
func parseSchema(raw []byte) (map[string]any, error) {
	v, err := decodeUniqueJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid JSON schema", ErrInvalidInput)
	}
	s, ok := v.(map[string]any)
	if !ok || checkSchema(s, 0) != nil {
		return nil, fmt.Errorf("%w: unsupported JSON schema", ErrInvalidInput)
	}
	return s, nil
}

func checkSchema(s map[string]any, depth int) error {
	if depth > 32 {
		return ErrInvalidInput
	}
	for key, value := range s {
		switch key {
		case "type":
			if name, ok := value.(string); ok {
				if !validSchemaType(name) {
					return ErrInvalidInput
				}
			} else if names, ok := value.([]any); ok && len(names) > 0 {
				for _, n := range names {
					name, ok := n.(string)
					if !ok || !validSchemaType(name) {
						return ErrInvalidInput
					}
				}
			} else {
				return ErrInvalidInput
			}
		case "properties":
			props, ok := value.(map[string]any)
			if !ok {
				return ErrInvalidInput
			}
			for _, v := range props {
				child, ok := v.(map[string]any)
				if !ok || checkSchema(child, depth+1) != nil {
					return ErrInvalidInput
				}
			}
		case "items":
			child, ok := value.(map[string]any)
			if !ok || checkSchema(child, depth+1) != nil {
				return ErrInvalidInput
			}
		case "required":
			items, ok := value.([]any)
			if !ok {
				return ErrInvalidInput
			}
			for _, v := range items {
				if _, ok := v.(string); !ok {
					return ErrInvalidInput
				}
			}
		case "additionalProperties":
			if _, ok := value.(bool); !ok {
				return ErrInvalidInput
			}
		case "enum":
			if values, ok := value.([]any); !ok || len(values) == 0 {
				return ErrInvalidInput
			}
		case "minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems":
			if _, ok := number(value); !ok {
				return ErrInvalidInput
			}
		case "description", "title", "$schema":
			if _, ok := value.(string); !ok {
				return ErrInvalidInput
			}
		default:
			return ErrInvalidInput
		}
	}
	if _, ok := s["type"]; !ok {
		return ErrInvalidInput
	}
	return nil
}

func validSchemaType(s string) bool {
	return s == "object" || s == "array" || s == "string" || s == "boolean" || s == "integer" || s == "number" || s == "null"
}

func matchesType(t string, v any) bool {
	switch t {
	case "null":
		return v == nil
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := number(v)
		return ok
	case "integer":
		n, ok := number(v)
		return ok && math.Trunc(n) == n
	}
	return false
}

func validateValue(s map[string]any, v any) error {
	types := []any{s["type"]}
	if list, ok := s["type"].([]any); ok {
		types = list
	}
	matched := false
	for _, t := range types {
		if name, ok := t.(string); ok && matchesType(name, v) {
			matched = true
		}
	}
	if !matched {
		return ErrInvalidModelOutput
	}
	if values, ok := s["enum"].([]any); ok {
		found := false
		for _, x := range values {
			if reflect.DeepEqual(x, v) {
				found = true
			}
		}
		if !found {
			return ErrInvalidModelOutput
		}
	}
	switch value := v.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		if required, ok := s["required"].([]any); ok {
			for _, key := range required {
				if _, exists := value[key.(string)]; !exists {
					return ErrInvalidModelOutput
				}
			}
		}
		for key, entry := range value {
			if sub, exists := props[key]; exists {
				if validateValue(sub.(map[string]any), entry) != nil {
					return ErrInvalidModelOutput
				}
			} else if allow, ok := s["additionalProperties"].(bool); ok && !allow {
				return ErrInvalidModelOutput
			}
		}
	case []any:
		if !within(float64(len(value)), s, "minItems", "maxItems") {
			return ErrInvalidModelOutput
		}
		if sub, ok := s["items"].(map[string]any); ok {
			for _, entry := range value {
				if validateValue(sub, entry) != nil {
					return ErrInvalidModelOutput
				}
			}
		}
	case string:
		if !within(float64(utf8.RuneCountInString(value)), s, "minLength", "maxLength") {
			return ErrInvalidModelOutput
		}
	case json.Number:
		n, ok := number(value)
		if !ok || !within(n, s, "minimum", "maximum") {
			return ErrInvalidModelOutput
		}
	}
	return nil
}

func number(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
}
func within(n float64, s map[string]any, min, max string) bool {
	if low, ok := number(s[min]); ok && n < low {
		return false
	}
	if high, ok := number(s[max]); ok && n > high {
		return false
	}
	return true
}

func decodeUniqueJSON(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := readJSONValue(d, 0)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, ErrInvalidModelOutput
	}
	return v, nil
}

func readJSONValue(d *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, ErrInvalidModelOutput
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := t.(json.Delim)
	if !isDelim {
		return t, nil
	}
	switch delim {
	case '{':
		object := map[string]any{}
		for d.More() {
			keyToken, err := d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, ErrInvalidModelOutput
			}
			if _, exists := object[key]; exists {
				return nil, ErrInvalidModelOutput
			}
			value, err := readJSONValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return nil, ErrInvalidModelOutput
		}
		return object, nil
	case '[':
		array := []any{}
		for d.More() {
			v, err := readJSONValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, v)
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return nil, ErrInvalidModelOutput
		}
		return array, nil
	}
	return nil, ErrInvalidModelOutput
}
