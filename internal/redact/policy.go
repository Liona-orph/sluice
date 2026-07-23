package redact

// Action is what a policy does with a detected entity.
type Action uint8

const (
	// ActionTokenize replaces the value with a stable placeholder and restores
	// the original in the response. It is the zero value because it is the only
	// action that keeps the model's answer useful, and an operator who has not
	// thought about a new entity type is better served by a reversible default
	// than by silently shipping the value upstream.
	ActionTokenize Action = iota
	// ActionMask replaces the value with asterisks, optionally keeping a
	// trailing suffix. Irreversible, shape-preserving, and the right choice for
	// values the model may need to see the form of but never the content of.
	ActionMask
	// ActionHash replaces the value with a salted digest. Irreversible, but
	// stable: two occurrences of the same value get the same token, so a model
	// can still reason about "the same customer" without learning who.
	ActionHash
	// ActionAllow passes the value through untouched.
	ActionAllow
)

func (a Action) String() string {
	switch a {
	case ActionTokenize:
		return "tokenize"
	case ActionMask:
		return "mask"
	case ActionHash:
		return "hash"
	case ActionAllow:
		return "allow"
	}
	return "unknown"
}

// Policy decides what happens to each entity type.
//
// It is a value, copied by every Redactor that uses it, so that changing policy
// at runtime means building a new Redactor rather than mutating one that
// requests are already flowing through.
type Policy struct {
	// Default applies to types not named in ByType.
	Default Action
	ByType  map[EntityType]Action
	// HashSalt makes ActionHash digests unguessable. Without it, an email
	// address is recoverable from its hash by anyone willing to hash a
	// dictionary, which is most of the value of hashing gone.
	HashSalt []byte
	// MaskKeepLast is the number of trailing characters ActionMask leaves
	// visible, e.g. 4 to keep the last four digits of a card.
	MaskKeepLast int
	// MinConfidence drops matches a detector is not sure enough about. It is
	// the knob for the person-name detector, whose gazetteer-only matches score
	// 0.5; raising this above that removes them and their false positives with
	// them.
	MinConfidence float64
}

// DefaultPolicy is the configuration a deployment should be able to adopt
// without thinking, and the reasoning behind each choice.
//
//   - Contact details -- email, phone, name, IP -- are tokenized. The reply
//     usually needs them back ("draft a message to [PERSON_1]"), and they are
//     not secrets in the sense that seeing one again is a breach.
//   - Card numbers, IBANs and national identifiers are masked with the last
//     four characters kept. Restoring a card number into a model's output is a
//     way to leak it into a log or a transcript, and the last four are what a
//     human actually needs to identify the instrument.
//   - Credentials are hashed, never restored. A key that came back out of the
//     gateway would defeat the point of stopping it going in.
//
// MinConfidence is 0, which keeps the low-confidence name matches. Raising it
// to 0.6 trades recall on names for precision; eval_test.go measures both.
func DefaultPolicy() Policy {
	return Policy{
		Default:      ActionTokenize,
		MaskKeepLast: 4,
		ByType: map[EntityType]Action{
			EntityEmail:      ActionTokenize,
			EntityPhone:      ActionTokenize,
			EntityPersonName: ActionTokenize,
			EntityIPAddress:  ActionTokenize,

			EntityCreditCard: ActionMask,
			EntityIBAN:       ActionMask,
			EntityUSSSN:      ActionMask,
			EntityUKNINO:     ActionMask,
			EntityESDNI:      ActionMask,

			EntityAPIKey: ActionHash,
		},
	}
}

// For returns the action configured for t.
func (p Policy) For(t EntityType) Action {
	if a, ok := p.ByType[t]; ok {
		return a
	}
	return p.Default
}

// With returns a copy of p with t bound to a.
func (p Policy) With(t EntityType, a Action) Policy {
	out := p.clone()
	out.ByType[t] = a
	return out
}

// WithAll returns a copy of p that applies a to every entity type.
func (p Policy) WithAll(a Action) Policy {
	out := p.clone()
	out.Default = a
	for t := range out.ByType {
		out.ByType[t] = a
	}
	return out
}

func (p Policy) clone() Policy {
	out := p
	out.ByType = make(map[EntityType]Action, len(p.ByType))
	for k, v := range p.ByType {
		out.ByType[k] = v
	}
	if p.HashSalt != nil {
		out.HashSalt = append([]byte(nil), p.HashSalt...)
	}
	return out
}
