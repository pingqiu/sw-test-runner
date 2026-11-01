package seaweed

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Only these observed Azure IMDS warnings are recoverable for this smoke. This is
// the logging message, without the timestamp/logger prefix in cloud-init.log.
var cloudIMDSGone = regexp.MustCompile(`\APolling IMDS failed attempt [1-9][0-9]* with exception: UrlError\('410 Client Error: Gone for url: http://169\.254\.169\.254/metadata/instance\?api-version=2021-08-01&extended=true'\)\z`)

// Azure retries this poll; run 7d (client2) logged attempts 1 and 2.
var cloudIMDSReprovisionNotFound = regexp.MustCompile(`\APolling IMDS failed attempt [1-9][0-9]* with exception: UrlError\('404 Client Error: Not Found for url: http://169\.254\.169\.254/metadata/reprovisiondata\?api-version=2019-06-01'\)\z`)

func qualifyCloudInit(raw string, rc int) error {
	if rc != 0 && rc != 2 {
		return fmt.Errorf("cloud-init exit %d", rc)
	}
	d := json.NewDecoder(strings.NewReader(raw))
	value, err := cloudInitValue(d, 0)
	if err != nil {
		return err
	}
	if _, err = d.Token(); err != io.EOF {
		return fmt.Errorf("cloud-init trailing JSON")
	}
	root, ok := value.(map[string]any)
	if !ok || root["status"] != "done" {
		return fmt.Errorf("cloud-init must be done")
	}
	if _, ok := root["errors"].([]any); !ok {
		return fmt.Errorf("cloud-init errors array required")
	}
	if _, ok := root["recoverable_errors"].(map[string]any); !ok {
		return fmt.Errorf("cloud-init recoverable_errors object required")
	}
	var check func(any) error
	check = func(v any) error {
		switch x := v.(type) {
		case map[string]any:
			for key, child := range x {
				switch key {
				case "errors":
					a, ok := child.([]any)
					if !ok || len(a) != 0 {
						return fmt.Errorf("cloud-init fatal errors or invalid errors array")
					}
				case "recoverable_errors":
					levels, ok := child.(map[string]any)
					if !ok {
						return fmt.Errorf("invalid cloud-init recoverable_errors")
					}
					for level, messages := range levels {
						a, ok := messages.([]any)
						if !ok {
							return fmt.Errorf("invalid cloud-init error messages")
						}
						if level != "WARNING" && level != "DEPRECATED" && level != "ERROR" && level != "CRITICAL" {
							return fmt.Errorf("unknown cloud-init error level %s", level)
						}
						for _, message := range a {
							s, ok := message.(string)
							if !ok || level != "WARNING" || (!cloudIMDSGone.MatchString(s) && !cloudIMDSReprovisionNotFound.MatchString(s)) {
								return fmt.Errorf("unapproved cloud-init %s: %v", level, message)
							}
						}
					}
				default:
					if strings.Contains(strings.ToLower(key), "error") {
						return fmt.Errorf("unknown cloud-init error field %s", key)
					}
					if err := check(child); err != nil {
						return err
					}
				}
			}
		case []any:
			for _, child := range x {
				if err := check(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return check(root)
}

// Cloud-init metadata is extensible; decision keys must still be unambiguous.
func cloudInitValue(d *json.Decoder, depth int) (any, error) {
	if depth > 32 {
		return nil, fmt.Errorf("cloud-init nesting exceeds 32")
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		result, seen := map[string]any{}, map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := k.(string)
			if !ok || seen[strings.ToLower(key)] {
				return nil, fmt.Errorf("duplicate cloud-init key %v", k)
			}
			seen[strings.ToLower(key)] = true
			if (strings.EqualFold(key, "status") && key != "status") || (strings.Contains(strings.ToLower(key), "error") && key != strings.ToLower(key)) {
				return nil, fmt.Errorf("case-aliased cloud-init key %s", key)
			}
			result[key], err = cloudInitValue(d, depth+1)
			if err != nil {
				return nil, err
			}
		}
		_, err = d.Token()
		return result, err
	case json.Delim('['):
		result := []any{}
		for d.More() {
			child, err := cloudInitValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, child)
		}
		_, err = d.Token()
		return result, err
	default:
		return token, nil
	}
}
