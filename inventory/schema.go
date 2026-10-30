package inventory

import (
	"bytes"
	_ "embed"
)

//go:embed schema.json
var schemaJSON []byte

// Schema returns a copy of structural JSON Schema. Decode/Join additionally
// enforce canonical paths, public-key syntax and cross-document identities.
func Schema() []byte { return bytes.Clone(schemaJSON) }
