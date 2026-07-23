// Package redact finds personal and secret data in prompts and replaces it
// according to a policy, reversibly where that is useful.
//
// The design has three load-bearing decisions.
//
// First, every detector that can validate its own findings does. A regular
// expression that matches the shape of a credit card number matches a great
// many things that are not credit card numbers; the Luhn check removes about
// nine tenths of those, and the mod-97 check does the same for IBANs. Detection
// without validation produces a false-positive rate high enough that operators
// switch redaction off, which is a worse outcome than not shipping it.
//
// Second, redaction is reversible by default for the entity types where reading
// the answer requires the original value back. A gateway that turns
// "alice@example.com" into "[REDACTED]" makes the model's reply useless; one
// that turns it into a stable placeholder and restores it on the way out keeps
// the reply useful and the prompt clean. That round trip is the feature, and
// most of the care in this package goes into making it exact.
//
// Third, detection works on a stream. An entity that straddles a chunk boundary
// is the case that matters, because it is the one that leaks in production and
// never in a demo. See StreamRedactor.
package redact

import "sort"

// EntityType names a class of sensitive value.
type EntityType string

// The entity classes this package detects. Each has a detector in
// detectors.go and a measured precision and recall in eval_test.go.
const (
	EntityEmail      EntityType = "email"
	EntityPhone      EntityType = "phone"
	EntityCreditCard EntityType = "credit_card"
	EntityIBAN       EntityType = "iban"
	EntityIPAddress  EntityType = "ip_address"
	// EntityUSSSN is a US Social Security Number. It, EntityUKNINO and
	// EntityESDNI are national identifiers, and each has a validity rule beyond
	// its shape, which is what makes them worth detecting separately rather
	// than as one "national id" pattern.
	EntityUSSSN  EntityType = "us_ssn"
	EntityUKNINO EntityType = "uk_nino"
	EntityESDNI  EntityType = "es_dni"
	// EntityAPIKey covers credentials: recognised vendor formats and
	// high-entropy strings that look like secrets.
	EntityAPIKey EntityType = "api_key"
	// EntityPersonName is the least reliable detector here; see nameDetector.
	EntityPersonName EntityType = "person_name"
)

// Match is one detected entity, as a byte range into the text it was found in.
type Match struct {
	Type  EntityType
	Start int
	End   int
	Value string
	// Confidence is a coarse indication of how much the detector's validation
	// step narrowed the result. A checksum-validated card is 0.99; a person
	// name recognised only by a gazetteer hit is 0.5. Policy.MinConfidence
	// turns it into a knob rather than leaving it as documentation.
	Confidence float64
}

// Len is the length of the matched text in bytes.
func (m Match) Len() int { return m.End - m.Start }

// Detector finds one class of entity.
//
// Implementations must be safe for concurrent use and must not retain the text
// they are given.
type Detector interface {
	// Type is the entity class this detector produces.
	Type() EntityType
	// Find returns non-overlapping matches, in order of appearance.
	Find(text string) []Match
	// MaxLen is the longest match this detector can produce, in bytes.
	//
	// It is part of the interface rather than a constant because the streaming
	// redactor's look-behind window is the maximum of these: hold back fewer
	// bytes than the longest possible entity and an entity spanning a chunk
	// boundary is emitted half-redacted. A detector that under-reports MaxLen
	// breaks streaming redaction, which is why it is stated per detector
	// instead of guessed globally.
	MaxLen() int
	// Priority breaks ties when two detectors claim overlapping text. Higher
	// wins.
	Priority() int
}

// resolveOverlaps reduces matches to a non-overlapping set.
//
// The rule is longest-first, then highest priority, then leftmost. Longest
// first because a nine-digit run inside a longer card number is the card
// number, not a separate entity; priority second because equal-length claims
// come from detectors with genuinely different reliability, and a checksum-
// validated IBAN should beat a phone number that happens to share its shape.
func resolveOverlaps(matches []Match, detectors map[EntityType]int) []Match {
	if len(matches) < 2 {
		return matches
	}
	ordered := append([]Match(nil), matches...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.Len() != b.Len() {
			return a.Len() > b.Len()
		}
		if detectors[a.Type] != detectors[b.Type] {
			return detectors[a.Type] > detectors[b.Type]
		}
		return a.Start < b.Start
	})

	var kept []Match
	for _, m := range ordered {
		overlaps := false
		for _, k := range kept {
			if m.Start < k.End && k.Start < m.End {
				overlaps = true
				break
			}
		}
		if !overlaps {
			kept = append(kept, m)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Start < kept[j].Start })
	return kept
}
