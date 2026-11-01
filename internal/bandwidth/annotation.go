package bandwidth

import (
	"encoding/json"
	"fmt"
)

// Version is the format of the limits annotation. A reader ignores a version it does not know (no limits), never guesses.
const Version = 1

// Limits is the content of the limits annotation (names.AnnotationBandwidthLimits). Which fields count depends on the object:
//
//	node:                       GeneveKbps (the class, already scaled), LabCapKbps (the ceiling of one lab's Geneve traffic)
//	demux, L7 proxy pod:        ClassKbps (the part of the entry class of this component), CapKbps (the ceiling of one participant)
//	gateway pod:                CapKbps (the ceiling of one lab's internet traffic, the fair share is already in it)
type Limits struct {
	V          int   `json:"v"`
	ClassKbps  int64 `json:"classKbps,omitempty"`
	CapKbps    int64 `json:"capKbps,omitempty"`
	GeneveKbps int64 `json:"geneveKbps,omitempty"`
	LabCapKbps int64 `json:"labCapKbps,omitempty"`
}

// Encode writes the annotation value.
func (l Limits) Encode() string {
	l.V = Version
	b, _ := json.Marshal(l)
	return string(b)
}

// Decode reads an annotation value. An empty value is no limits and no error; a value of another version is an error.
func Decode(s string) (Limits, error) {
	if s == "" {
		return Limits{}, nil
	}
	var l Limits
	if err := json.Unmarshal([]byte(s), &l); err != nil {
		return Limits{}, fmt.Errorf("bandwidth limits annotation: %w", err)
	}
	if l.V != Version {
		return Limits{}, fmt.Errorf("bandwidth limits annotation: version %d is not %d", l.V, Version)
	}
	if l.ClassKbps < 0 || l.CapKbps < 0 || l.GeneveKbps < 0 || l.LabCapKbps < 0 {
		return Limits{}, fmt.Errorf("bandwidth limits annotation: negative rate")
	}
	return l, nil
}

// Zero says there is nothing to apply.
func (l Limits) Zero() bool { return l == Limits{V: l.V} || l == Limits{} }
