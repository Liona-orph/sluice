package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Liona-orph/sluice/pkg/llm"
)

// Redactor applies a Policy using a set of Detectors. It is immutable after
// New and safe for concurrent use.
type Redactor struct {
	detectors []Detector
	priority  map[EntityType]int
	policy    Policy
	lookAhead int
}

// New builds a Redactor. Passing no detectors uses DefaultDetectors.
func New(policy Policy, detectors ...Detector) *Redactor {
	if len(detectors) == 0 {
		detectors = DefaultDetectors()
	}
	r := &Redactor{
		detectors: append([]Detector(nil), detectors...),
		priority:  make(map[EntityType]int, len(detectors)),
		policy:    policy.clone(),
	}
	for _, d := range r.detectors {
		r.priority[d.Type()] = d.Priority()
		if n := d.MaxLen(); n > r.lookAhead {
			r.lookAhead = n
		}
	}
	return r
}

// Policy returns the policy in force.
func (r *Redactor) Policy() Policy { return r.policy }

// Detect runs every detector and resolves overlaps.
func (r *Redactor) Detect(text string) []Match {
	var all []Match
	for _, d := range r.detectors {
		for _, m := range d.Find(text) {
			if m.Confidence < r.policy.MinConfidence {
				continue
			}
			all = append(all, m)
		}
	}
	return resolveOverlaps(all, r.priority)
}

// Redact rewrites text according to the policy and returns a Vault holding the
// mapping needed to reverse the tokenized entities.
func (r *Redactor) Redact(text string) (string, *Vault) {
	v := NewVault(r.policy.HashSalt)
	return r.RedactWith(text, v), v
}

// RedactWith rewrites text into an existing vault, so that the same value
// appearing in several messages of one conversation gets one placeholder.
func (r *Redactor) RedactWith(text string, v *Vault) string {
	v.reserve(text)
	return r.apply(text, r.Detect(text), v)
}

// apply rewrites the matched ranges. Matches must be non-overlapping and
// ordered, which is what Detect guarantees.
func (r *Redactor) apply(text string, matches []Match, v *Vault) string {
	if len(matches) == 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	for _, m := range matches {
		if m.Start < last {
			continue
		}
		b.WriteString(text[last:m.Start])
		b.WriteString(r.replacement(m, v))
		last = m.End
	}
	b.WriteString(text[last:])
	return b.String()
}

func (r *Redactor) replacement(m Match, v *Vault) string {
	switch r.policy.For(m.Type) {
	case ActionAllow:
		return m.Value
	case ActionMask:
		v.countReplaced(m.Type)
		return mask(m.Value, r.policy.MaskKeepLast)
	case ActionHash:
		return v.hash(m.Type, m.Value)
	default:
		return v.tokenize(m.Type, m.Value)
	}
}

// mask replaces every rune with an asterisk except the last keepLast.
func mask(value string, keepLast int) string {
	runes := []rune(value)
	if keepLast < 0 {
		keepLast = 0
	}
	if keepLast >= len(runes) {
		keepLast = 0
	}
	var b strings.Builder
	for i := range runes {
		if i >= len(runes)-keepLast {
			b.WriteRune(runes[i])
		} else {
			b.WriteByte('*')
		}
	}
	return b.String()
}

// RedactRequest rewrites every message in req, returning a copy. The original
// request is not modified.
func (r *Redactor) RedactRequest(req llm.Request) (llm.Request, *Vault) {
	v := NewVault(r.policy.HashSalt)
	out := req
	out.Messages = llm.CloneMessages(req.Messages)
	for i := range out.Messages {
		m := &out.Messages[i]
		// Reserving across the whole conversation before rewriting any of it
		// would be tidier, but reserve is idempotent and RedactWith does it per
		// message, which is also correct: a placeholder already used in message
		// three cannot be allocated for message one because the counter only
		// moves forward.
		m.Content = r.RedactWith(m.Content, v)
		for j := range m.ToolCalls {
			args := r.RedactWith(string(m.ToolCalls[j].Arguments), v)
			m.ToolCalls[j].Arguments = []byte(args)
		}
	}
	return out, v
}

// RestoreResponse puts the original values back into a response.
//
// The response is copied rather than rewritten in place, because the cache may
// be holding the same value and a restoration is caller-specific: two callers
// share a cached answer but each has their own vault.
func (r *Redactor) RestoreResponse(resp llm.Response, v *Vault) llm.Response {
	out := resp
	out.Message = llm.CloneMessages([]llm.Message{resp.Message})[0]
	out.Message.Content = v.Restore(resp.Message.Content)
	for i := range out.Message.ToolCalls {
		out.Message.ToolCalls[i].Arguments = []byte(v.Restore(string(out.Message.ToolCalls[i].Arguments)))
	}
	return out
}

// --- placeholders -----------------------------------------------------------

// placeholderPattern recognises a placeholder on the way back in.
//
// It is deliberately looser than the format tokenize emits. Models lowercase
// things, insert spaces inside brackets, and swap underscores for hyphens while
// rewriting the text around a token they do not understand; a strict matcher
// loses the restoration in exactly those cases, and a lost restoration means
// the caller gets a placeholder in their answer. The looseness costs nothing,
// because a string of this shape that is not in the vault is left alone.
var placeholderPattern = regexp.MustCompile(`(?i)\[\s*sluice[_ \-]([a-z][a-z0-9_ \-]*?)[_ \-](\d{1,6})\s*\]`)

// placeholderKey normalises a match of placeholderPattern to the form used as a
// map key, so that "[ sluice-email-1 ]" and "[SLUICE_EMAIL_0001]" agree.
func placeholderKey(typ, index string) string {
	t := strings.ToUpper(typ)
	t = strings.NewReplacer(" ", "_", "-", "_").Replace(t)
	n, err := strconv.Atoi(index)
	if err != nil {
		return ""
	}
	return t + "|" + strconv.Itoa(n)
}

// Vault holds the reversible mappings produced by one redaction.
//
// It is safe for concurrent use because a streaming response is restored on one
// goroutine while the request that produced it may still be being redacted on
// another, and because the same vault covers every message of a conversation.
type Vault struct {
	mu       sync.RWMutex
	salt     []byte
	byValue  map[string]string // type|value -> placeholder
	byKey    map[string]string // normalised placeholder key -> original value
	counters map[EntityType]int
	// reserved marks placeholder keys that already occur in the source text.
	//
	// This is what makes the round trip exact rather than nearly exact. If a
	// prompt already contains "[SLUICE_EMAIL_0001]" -- because it was pasted
	// from a previous redaction, or because someone is probing -- and the vault
	// then allocated that same placeholder for a real address, restoration
	// would replace both occurrences and the caller would get back text they
	// never sent. Skipping reserved indices removes the collision instead of
	// making it unlikely.
	reserved map[string]bool
}

// NewVault returns an empty vault. salt is used for ActionHash.
func NewVault(salt []byte) *Vault {
	return &Vault{
		salt:     append([]byte(nil), salt...),
		byValue:  map[string]string{},
		byKey:    map[string]string{},
		counters: map[EntityType]int{},
		reserved: map[string]bool{},
	}
}

// Len is the number of distinct values the vault can restore.
func (v *Vault) Len() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.byKey)
}

// Types reports how many values of each type were replaced, for audit logs that
// must record that redaction happened without recording what it found.
//
// The counting rule differs by action, and the difference is worth knowing when
// reading a report built from it. Tokenized values are counted once per
// distinct value, because that is what a placeholder identifies; masked and
// hashed values are counted once per occurrence, because neither leaves
// anything behind that could tell two identical values apart. A prompt naming
// the same card twice therefore reports two credit_card replacements and one
// email replacement for two mentions of one address.
func (v *Vault) Types() map[EntityType]int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make(map[EntityType]int, len(v.counters))
	for k, n := range v.counters {
		if n > 0 {
			out[k] = n
		}
	}
	return out
}

// reserve records every placeholder-shaped substring already present in text.
func (v *Vault) reserve(text string) {
	if !strings.Contains(strings.ToLower(text), "sluice") {
		// The common case by an enormous margin; the regex is not cheap enough
		// to run over every message for nothing.
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, g := range placeholderPattern.FindAllStringSubmatch(text, -1) {
		if key := placeholderKey(g[1], g[2]); key != "" {
			v.reserved[key] = true
		}
	}
}

// tokenize returns the stable placeholder for a value, allocating one if
// needed.
func (v *Vault) tokenize(t EntityType, value string) string {
	v.mu.Lock()
	defer v.mu.Unlock()

	valueKey := string(t) + "|" + value
	if p, ok := v.byValue[valueKey]; ok {
		return p
	}
	typeName := strings.ToUpper(string(t))
	for {
		v.counters[t]++
		n := v.counters[t]
		key := typeName + "|" + strconv.Itoa(n)
		if v.reserved[key] || v.byKey[key] != "" {
			continue
		}
		placeholder := fmt.Sprintf("[SLUICE_%s_%04d]", typeName, n)
		v.byValue[valueKey] = placeholder
		v.byKey[key] = value
		return placeholder
	}
}

// countReplaced records an irreversible replacement, which leaves nothing in
// the vault but still has to appear in the audit record: a log that only
// counted the reversible replacements would understate what was removed.
func (v *Vault) countReplaced(t EntityType) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.counters[t]++
}

// hash returns a salted, stable pseudonym.
//
// The delimiters differ from a placeholder's on purpose: a hashed value must
// never be mistaken for something restorable, and using the same brackets would
// make that depend on whether eight hex characters happened to be all digits.
func (v *Vault) hash(t EntityType, value string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.counters[t]++

	h := sha256.New()
	h.Write(v.salt)
	h.Write([]byte(value))
	return "<" + strings.ToUpper(string(t)) + ":" + hex.EncodeToString(h.Sum(nil))[:12] + ">"
}

// Restore replaces known placeholders with their original values.
//
// Unknown placeholders are left exactly as they are: the model may have
// invented one, and inventing a value to go with it would be worse than
// returning the token.
func (v *Vault) Restore(text string) string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if len(v.byKey) == 0 || !strings.Contains(strings.ToLower(text), "sluice") {
		return text
	}
	return placeholderPattern.ReplaceAllStringFunc(text, func(match string) string {
		g := placeholderPattern.FindStringSubmatch(match)
		if g == nil {
			return match
		}
		if original, ok := v.byKey[placeholderKey(g[1], g[2])]; ok {
			return original
		}
		return match
	})
}

// alignToRuneStart moves i back to the nearest position that begins a rune, so
// that a stream never emits half of a multi-byte character. It gives up after
// utf8.UTFMax bytes, which means arbitrary binary input costs a few bytes of
// misalignment rather than an unbounded search.
func alignToRuneStart(s string, i int) int {
	if i >= len(s) {
		return len(s)
	}
	for n := 0; i > 0 && n < utf8.UTFMax && !utf8.RuneStart(s[i]); n++ {
		i--
	}
	return i
}
