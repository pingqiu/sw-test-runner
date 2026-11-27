package declared

// Placement: how an unknown answer of a verb is placed by the observation
// of its subject after it, against the one before it. One definition, as
// data: a row of the product's placement table (the "placement" rows of its
// checks description), read by the driver here and by the product's checker
// reader. The vocabulary is closed: one way to place ("by") and four
// outcomes, each a fixed name.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// The placement outcomes, a fixed enumeration.
const (
	PlacedTookEffect = "took_effect" // the operation took effect
	PlacedDidNot     = "did_not"     // it did not take effect
	PlacedThird      = "third"       // neither: a third state
	PlacedNotPlaced  = "not_placed"  // the observations cannot place it
)

// PlacementOutcomes is the enumeration, in order.
var PlacementOutcomes = []string{PlacedTookEffect, PlacedDidNot, PlacedThird, PlacedNotPlaced}

// PlaceByNames is the one way to place now: the observation's list Part
// holds the names a request of the verb creates, and the request's Arg is
// the name it creates. The cases, each mapped to an outcome by the row:
// present_before (the name is already in the list before the request),
// others_lost (a name of the list before is gone after), present (the name
// is in the list after), absent (it is not).
const PlaceByNames = "snapshot_names"

// placeCases are PlaceByNames' cases, in the order they are decided.
var placeCases = []string{"present_before", "others_lost", "present", "absent"}

// Placement is one row of the placement table.
type Placement struct {
	Verb     string            `json:"verb"`
	By       string            `json:"by"`
	Part     string            `json:"part"`
	Arg      string            `json:"arg"`
	Outcomes map[string]string `json:"outcomes"`
	Cite     string            `json:"cite"`
	What     string            `json:"what,omitempty"`
}

// ParsePlacements reads the "placement" rows of a checks description (its
// other keys are not read here), by verb. A row outside the closed
// vocabulary does not load: another key, another "by", a case missing or
// added, an outcome outside the enumeration, two rows of one verb.
func ParsePlacements(b []byte) (map[string]*Placement, error) {
	var d struct {
		Placement []json.RawMessage `json:"placement"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("placement: %w", err)
	}
	out := map[string]*Placement{}
	for i, raw := range d.Placement {
		var p Placement
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&p); err != nil {
			return nil, fmt.Errorf("placement row %d: %w", i, err)
		}
		if err := p.check(); err != nil {
			return nil, fmt.Errorf("placement row %d (%s): %w", i, p.Verb, err)
		}
		if out[p.Verb] != nil {
			return nil, fmt.Errorf("placement row %d: a second row of verb %q", i, p.Verb)
		}
		out[p.Verb] = &p
	}
	return out, nil
}

func (p *Placement) check() error {
	if p.Verb == "" || p.Part == "" || p.Arg == "" || !pageCite.MatchString(p.Cite) {
		return fmt.Errorf("a row needs its verb, part, arg and the page line it rests on (cite docs/<page>.md:<line>)")
	}
	if p.By != PlaceByNames {
		return fmt.Errorf("by %q; the one way to place is %q", p.By, PlaceByNames)
	}
	if len(p.Outcomes) != len(placeCases) {
		return fmt.Errorf("outcomes %v; want exactly the cases %s", p.Outcomes, strings.Join(placeCases, ", "))
	}
	for _, c := range placeCases {
		o, ok := p.Outcomes[c]
		if !ok {
			return fmt.Errorf("outcomes: no case %q", c)
		}
		if !slices.Contains(PlacementOutcomes, o) {
			return fmt.Errorf("outcome %q of case %q is not one of %s", o, c, strings.Join(PlacementOutcomes, ", "))
		}
	}
	return nil
}

// Place places the request that created name from the two observations'
// results (before: the observation before it; after: the one after it):
// the outcome and why. Either observation without the list Part (or not a
// list of names) cannot place it: PlacedNotPlaced, and ok is false (the
// list was not recorded, so nothing is decided from it).
func (p *Placement) Place(name string, before, after map[string]any) (outcome, why string, ok bool) {
	bn, okb := nameList(before[p.Part])
	an, oka := nameList(after[p.Part])
	if !okb || !oka {
		return PlacedNotPlaced, fmt.Sprintf("%s not recorded as a list by both observations", p.Part), false
	}
	var lost []string
	for _, n := range bn {
		if !slices.Contains(an, n) {
			lost = append(lost, n)
		}
	}
	c := "absent"
	switch {
	case slices.Contains(bn, name):
		c = "present_before"
	case len(lost) > 0:
		c = "others_lost"
	case slices.Contains(an, name):
		c = "present"
	}
	why = fmt.Sprintf("by %s: %q %s", p.By, name, c)
	if c == "others_lost" {
		why = fmt.Sprintf("by %s: others lost: %s", p.By, strings.Join(lost, ", "))
	}
	return p.Outcomes[c], why, true
}

// nameList reads a list of names from a decoded observation part.
func nameList(v any) ([]string, bool) {
	switch l := v.(type) {
	case []string:
		return l, true
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			s, ok := x.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}
