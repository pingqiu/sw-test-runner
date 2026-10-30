package inventory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

// MaxBytes limits one input document before decoding.
const MaxBytes = 1 << 20

// Decode accepts exactly one bounded UTF-8 document, without duplicate or unknown
// fields. It performs no filesystem, network, credential, or authorization work.
func Decode(data []byte) (*Document, error) {
	if len(data) > MaxBytes || !utf8.Valid(data) {
		return nil, fmt.Errorf("inventory must be UTF-8 and <=%d bytes", MaxBytes)
	}
	scan := json.NewDecoder(bytes.NewReader(data))
	scan.UseNumber()
	if err := uniqueValue(scan, 0); err != nil {
		return nil, err
	}
	if _, err := scan.Token(); err != io.EOF {
		return nil, fmt.Errorf("inventory has trailing JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var d Document
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("inventory shape: %w", err)
	}
	if d.Transport != "http" && d.Transport != "rc" && d.Transport != "dc" {
		return nil, fmt.Errorf("transport must explicitly be http, rc or dc")
	}
	if err := requiredFields(data, reflect.TypeOf(d), "inventory"); err != nil {
		return nil, err
	}
	if err := validate(&d); err != nil {
		return nil, err
	}
	return &d, nil
}

func uniqueValue(d *json.Decoder, depth int) error {
	if depth > 32 {
		return fmt.Errorf("inventory nesting exceeds32")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			s, ok := key.(string)
			if !ok || seen[s] {
				return fmt.Errorf("duplicate or invalid object field %q", s)
			}
			seen[s] = true
			if err := uniqueValue(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	case json.Delim('['):
		for d.More() {
			if err := uniqueValue(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	}
	return nil
}

// Non-omitempty Go fields are required, including explicit false/empty values.
// This prevents JSON null or omission from silently becoming a Go zero value.
func requiredFields(raw []byte, typ reflect.Type, where string) error {
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("%s cannot be null", where)
	}
	switch typ.Kind() {
	case reflect.Struct:
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return err
		}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")
			value, present := obj[tag[0]]
			optional := len(tag) > 1 && tag[1] == "omitempty"
			if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				if optional {
					continue
				}
				return fmt.Errorf("%s.%s is required and non-null", where, tag[0])
			}
			if err := requiredFields(value, f.Type, where+"."+tag[0]); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := requiredFields(value, typ.Elem(), where+"[]"); err != nil {
				return err
			}
		}
	}
	return nil
}
