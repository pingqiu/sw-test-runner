package randomrun

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Operations is a product's operations table, read from its description
// directory (operations.json). Every file there starts with the product and
// the product commit its facts were read at.
type Operations struct {
	Product    string      `json:"product"`
	ReadAt     string      `json:"read_at"`
	Comment    string      `json:"comment,omitempty"`
	Operations []Operation `json:"operations"`
	Limits     []Limit     `json:"limits"`
}

// Operation is one typed action the generator may propose or run.
type Operation struct {
	Action string `json:"action"`
	// Op is the Module.Action id of the model-to-code trace the action's
	// record carries; "" when no model has an action for it.
	Op string `json:"op,omitempty"`
	// Weight is the default share of run TIME; absent, the action is not
	// proposed as an ordinary operation unless a run's weights name it.
	Weight *int `json:"weight,omitempty"`
	// Cost is the operation's rough duration in seconds: the time half of the
	// weighting (a proposal's chance is weight/cost) and the operations-per-hour
	// estimate.
	Cost       float64 `json:"cost"`
	Note       string  `json:"note,omitempty"`
	VerifiedAt string  `json:"verified_at,omitempty"` // the product commit a cite in the note was re-read at
}

// Limit is one refusal the product documents. A proposed one is probed by
// a negative operation, and Name is what a gen_step record carries as its
// negative; one with proposed false is listed with why it is not proposed.
type Limit struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Action     string `json:"action"`
	Cite       string `json:"cite"`
	Value      int    `json:"value,omitempty"` // a bound the limit states, if any
	SideEffect string `json:"side_effect,omitempty"`
	SideCite   string `json:"side_effect_cite,omitempty"`
	Note       string `json:"note,omitempty"`
	Proposed   *bool  `json:"proposed,omitempty"` // absent: true
	Why        string `json:"why,omitempty"`      // why a limit is not proposed
	VerifiedAt string `json:"verified_at,omitempty"`
}

// IsProposed says the generator probes the limit with negative operations.
func (l Limit) IsProposed() bool { return l.Proposed == nil || *l.Proposed }

// ParseOperations reads and checks an operations table.
func ParseOperations(b []byte) (Operations, error) {
	var t Operations
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&t); err != nil {
		return t, fmt.Errorf("operations: %w", err)
	}
	if t.Product == "" || t.ReadAt == "" {
		return t, fmt.Errorf("operations: product and read_at are required")
	}
	acts := map[string]bool{}
	for _, o := range t.Operations {
		if o.Action == "" || acts[o.Action] {
			return t, fmt.Errorf("operations: action %q empty or repeated", o.Action)
		}
		acts[o.Action] = true
		if o.Cost <= 0 || (o.Weight != nil && *o.Weight < 0) {
			return t, fmt.Errorf("operations: %s: cost %v, weight %v", o.Action, o.Cost, o.Weight)
		}
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for _, l := range t.Limits {
		if l.ID == "" || l.Name == "" || ids[l.ID] || names[l.Name] {
			return t, fmt.Errorf("operations: limit %q (%q) empty or repeated", l.ID, l.Name)
		}
		ids[l.ID], names[l.Name] = true, true
		if l.Cite == "" || (l.SideEffect != "") != (l.SideCite != "") {
			return t, fmt.Errorf("operations: limit %s: a fact without its cite", l.ID)
		}
		// A proposed limit's action is an operation of the table; one not
		// proposed may name an action the generator never issues as a
		// negative, and says why.
		if (l.IsProposed() && !acts[l.Action]) || (!l.IsProposed() && (l.Why == "" || l.Action == "")) || (l.IsProposed() && l.Why != "") {
			return t, fmt.Errorf("operations: limit %s: action %q unknown, or proposed %v with why %q", l.ID, l.Action, l.IsProposed(), l.Why)
		}
	}
	return t, nil
}

// DefaultWeights are the weights of the operations that have one.
func (t Operations) DefaultWeights() map[string]int {
	w := map[string]int{}
	for _, o := range t.Operations {
		if o.Weight != nil {
			w[o.Action] = *o.Weight
		}
	}
	return w
}

// Costs are the operations' costs in seconds.
func (t Operations) Costs() map[string]float64 {
	c := map[string]float64{}
	for _, o := range t.Operations {
		c[o.Action] = o.Cost
	}
	return c
}

// Limit returns the limit with this id.
func (t Operations) Limit(id string) (Limit, bool) {
	for _, l := range t.Limits {
		if l.ID == id {
			return l, true
		}
	}
	return Limit{}, false
}
