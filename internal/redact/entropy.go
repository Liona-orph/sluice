package redact

import (
	"math"
	"regexp"
)

// knownSecretPattern matches credential formats whose issuers publish them.
//
// A recognised prefix is worth far more than any statistical test: it is exact,
// it needs no threshold, and it catches short keys that entropy alone would
// miss. The entropy detector below exists for everything not on this list.
var knownSecretPattern = regexp.MustCompile(
	`sk-ant-api\d{2}-[A-Za-z0-9_\-]{20,120}` +
		`|sk-[A-Za-z0-9]{20,120}` +
		`|gh[pousr]_[A-Za-z0-9]{30,80}` +
		`|github_pat_[A-Za-z0-9_]{20,100}` +
		`|AKIA[0-9A-Z]{16}` +
		`|ASIA[0-9A-Z]{16}` +
		`|AIza[0-9A-Za-z_\-]{35}` +
		`|xox[baprs]-[A-Za-z0-9\-]{10,80}` +
		`|glpat-[A-Za-z0-9_\-]{20,64}` +
		`|eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`)

// secretCandidate finds runs long enough to be worth measuring.
//
// "=" is allowed only as trailing base64 padding, never inside the run: secrets
// are overwhelmingly written as NAME=value in configuration and shell, and a
// pattern that swallowed the "=" would report the variable name as part of the
// key and put it in the redacted output.
//
// The bounds are checked against neighbouring bytes rather than with \b,
// because the character class includes hyphens, for which \b does not mean what
// it looks like it means.
var secretCandidate = regexp.MustCompile(`[A-Za-z0-9+/_\-]{20,200}={0,2}`)

// Entropy thresholds. The derivation, since a bare number here would be
// unjustifiable:
//
// Shannon entropy over the characters of a string of length n is bounded above
// by log2(n), so short strings cannot score highly however random they are. At
// the 20-character floor this detector uses, that ceiling is 4.32 bits.
// TestEntropyOfRepresentativeStrings measures the figures below; they are not
// recalled from anywhere:
//
//	internationalisation           2.95 bits/char
//	getUserByIdentifierName        3.76
//	9f86d081884c7d659a2feaa0c...   3.64  (32 hex characters)
//	Xk7Qp2Rm9Tz4Vb8Nc3Wd6Yf1       4.58
//	aGVsbG8gd29ybGQgdGhpcyB...     4.23  (base64)
//
// Entropy alone therefore does not separate the classes: an English identifier
// at 3.76 outscores a hexadecimal digest at 3.64. A threshold set high enough
// to exclude the identifier would exclude the digest with it, which is the
// wrong trade -- a leaked key is worse than a redacted variable name.
//
// So minSecretEntropy is set at 3.5, low enough to keep hex, and two further
// conditions carry the discrimination: at least two character classes must be
// present, and either a digit must appear or the entropy must clear 4.2.
// "getUserByIdentifierName" fails both extra conditions and is rejected;
// "internationalisation" fails on entropy and on having one class.
const (
	minSecretEntropy = 3.5
	highEntropy      = 4.2
	minSecretLen     = 20
)

type secretDetector struct{}

// NewAPIKeyDetector detects credentials: recognised vendor formats first, then
// high-entropy strings.
func NewAPIKeyDetector() Detector { return secretDetector{} }

func (secretDetector) Type() EntityType { return EntityAPIKey }
func (secretDetector) MaxLen() int      { return 200 }
func (secretDetector) Priority() int    { return prioritySecret }

func (d secretDetector) Find(text string) []Match {
	var out []Match
	for _, loc := range knownSecretPattern.FindAllStringIndex(text, -1) {
		out = append(out, Match{
			Type: EntityAPIKey, Start: loc[0], End: loc[1],
			Value: text[loc[0]:loc[1]], Confidence: 0.99,
		})
	}
	for _, loc := range secretCandidate.FindAllStringIndex(text, -1) {
		if !isolated(text, loc[0], loc[1]) {
			continue
		}
		s := text[loc[0]:loc[1]]
		if !looksSecret(s) {
			continue
		}
		out = append(out, Match{
			Type: EntityAPIKey, Start: loc[0], End: loc[1],
			Value: s, Confidence: 0.7,
		})
	}
	// The two passes overlap on every known format, which also scores high
	// entropy. Keeping the higher-confidence one is what resolveOverlaps does,
	// but it works on equal-length matches, and these are equal length.
	return resolveOverlaps(out, map[EntityType]int{EntityAPIKey: 0})
}

// isolated reports whether the candidate is bounded by something other than a
// character that could have been part of it.
func isolated(text string, start, end int) bool {
	if start > 0 && isSecretByte(text[start-1]) {
		return false
	}
	if end < len(text) && isSecretByte(text[end]) {
		return false
	}
	return true
}

func isSecretByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '+', c == '/', c == '_', c == '-':
		return true
	}
	return false
}

// looksSecret applies the three conditions described above minSecretEntropy.
func looksSecret(s string) bool {
	if len(s) < minSecretLen {
		return false
	}
	h := ShannonEntropy(s)
	if h < minSecretEntropy {
		return false
	}
	var hasLower, hasUpper, hasDigit, hasSymbol bool
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z':
			hasLower = true
		case c >= 'A' && c <= 'Z':
			hasUpper = true
		case c >= '0' && c <= '9':
			hasDigit = true
		default:
			hasSymbol = true
		}
	}
	classes := 0
	for _, present := range []bool{hasLower, hasUpper, hasDigit, hasSymbol} {
		if present {
			classes++
		}
	}
	if classes < 2 {
		return false
	}
	return hasDigit || h >= highEntropy
}

// ShannonEntropy returns the entropy of s in bits per byte.
//
// Exported because the threshold above is only defensible if the measurement
// behind it can be checked, and the test does check it against strings whose
// entropy is known analytically.
func ShannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	h := 0.0
	for _, c := range &counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
