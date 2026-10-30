package smoke

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Approval pins a canonical Go JSON export. Requiring its exact round-trip
// spelling rejects aliases/duplicates as well as unknown fields, including in
// legacy S0b types with capitalized field names. No generic patch language.
func strictFreshTemplate(raw []byte, dst *freshTemplate) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(dst); e != nil {
		return e
	}
	canonical, e := json.Marshal(dst)
	if e != nil || !bytes.Equal(raw, canonical) {
		return errors.New("template must be exact canonical typed JSON")
	}
	return nil
}
