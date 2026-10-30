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
	// encoding/json otherwise accepts case-insensitive matches for struct tags.
	if err := checkFields(data, reflect.TypeOf(Document{}), "inventory"); err != nil {
		return nil, err
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
			if !ok || seen[strings.ToLower(s)] {
				return fmt.Errorf("duplicate or invalid object field %q", s)
			}
			seen[strings.ToLower(s)] = true
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

// checkFields is one recursive shape pass: exact tags, required declarations
// and schema-specific nullability, before encoding/json can match case aliases.
func checkFields(raw []byte, typ reflect.Type, where string) error {
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
		fields := map[string]reflect.StructField{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			fields[strings.Split(f.Tag.Get("json"), ",")[0]] = f
		}
		for key := range obj {
			if _, ok := fields[key]; !ok {
				return fmt.Errorf("%s has noncanonical or unknown field %q", where, key)
			}
		}
		for name, f := range fields {
			value, present := obj[name]
			optional := strings.Contains(f.Tag.Get("json"), ",omitempty")
			if !present {
				if optional {
					continue
				}
				return fmt.Errorf("%s.%s is required and non-null", where, name)
			}
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				// These are the only nullable declarations in the published v1 schema.
				// State-specific validation still refuses observed nil GIDs/capabilities.
				nullable := typ == reflect.TypeOf(Document{}) && name == "observation" || typ == reflect.TypeOf(RDMA{}) && (name == "gid_index" || name == "capabilities")
				if optional && nullable {
					continue
				}
				return fmt.Errorf("%s.%s cannot be null", where, name)
			}
			if err := checkFields(value, f.Type, where+"."+name); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := checkFields(value, typ.Elem(), where+"[]"); err != nil {
				return err
			}
		}
	}
	return nil
}
