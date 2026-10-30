package testrunner

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
)

// S0bExecutionInputs are the approved scenario and all script bodies that its
// worker adapter will consume. PR3 must bind/snapshot this set before admission.
type S0bExecutionInputs struct {
	Scenario []byte
	Scripts  map[string][]byte
}

var insecureSSHOption = regexp.MustCompile(`(?i)StrictHostKeyChecking[\s=]+["']?no\b`)
var nestedSSH = regexp.MustCompile(`(?i)(^|[^[:alnum:]_.-])ssh(\.exe)?($|[^[:alnum:]_.-])`)

// NewS0bRunScope is the S0b-specific entrance; S0a's local/test entrance remains
// NewRunScope. This does not supply the independent authority verifier.
func NewS0bRunScope(ctx context.Context, a RunAdmission, backend OwnedJobBackend, verify func(context.Context, RunAdmission) error, inputs S0bExecutionInputs) (*RunScope, error) {
	if len(inputs.Scenario) == 0 {
		return nil, errors.New("S0b scenario bytes required")
	}
	check := func(name string, body []byte) error {
		if insecureSSHOption.Match(body) || nestedSSH.Match(body) {
			return fmt.Errorf("S0b refuses insecure host-key option or nested SSH in %s", name)
		}
		return nil
	}
	if err := check("scenario", inputs.Scenario); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(inputs.Scripts))
	for name := range inputs.Scripts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := check(name, inputs.Scripts[name]); err != nil {
			return nil, err
		}
	}
	return NewRunScope(ctx, a, backend, verify)
}
