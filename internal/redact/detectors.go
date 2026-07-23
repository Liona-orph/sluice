package redact

import (
	"net/netip"
	"regexp"
	"strings"
)

// regexDetector is the shape almost every detector takes: a pattern that finds
// candidates and a predicate that decides whether a candidate is real.
//
// Separating the two is the whole point. The pattern is allowed to be greedy
// and wrong; validate is where the false positives die, and it is the part that
// can be measured.
type regexDetector struct {
	typ        EntityType
	pattern    *regexp.Regexp
	maxLen     int
	priority   int
	confidence float64
	// validate returns the accepted value, or false to reject the candidate.
	validate func(candidate string) (string, bool)
}

func (d *regexDetector) Type() EntityType { return d.typ }
func (d *regexDetector) MaxLen() int      { return d.maxLen }
func (d *regexDetector) Priority() int    { return d.priority }

func (d *regexDetector) Find(text string) []Match {
	idx := d.pattern.FindAllStringIndex(text, -1)
	if idx == nil {
		return nil
	}
	out := make([]Match, 0, len(idx))
	for _, loc := range idx {
		start, end := loc[0], loc[1]
		candidate := text[start:end]
		if d.validate != nil {
			if _, ok := d.validate(candidate); !ok {
				continue
			}
		}
		if end-start > d.maxLen {
			continue
		}
		out = append(out, Match{
			Type: d.typ, Start: start, End: end,
			Value: candidate, Confidence: d.confidence,
		})
	}
	return out
}

// Detector priorities, in one place because the ranking only makes sense read
// together.
//
// The ordering principle is strength of evidence. A value that satisfies a
// published checksum is a value of that type; nothing a heuristic produces
// should be allowed to outrank it. In particular the secret detector, which is
// the greediest thing here, sits below every checksum-validated type -- an IBAN
// is 22 mixed-case alphanumerics and scores perfectly respectable entropy, and
// without this ordering it would be classified as a credential and hashed
// instead of masked.
const (
	priorityChecksummed = 110 // card, IBAN
	priorityNationalID  = 105 // SSN, NINO, DNI: checksummed or rule-validated
	prioritySecret      = 100
	priorityEmail       = 80
	priorityPhone       = 60
	priorityIP          = 50
	priorityName        = 30
)

// DefaultDetectors returns one instance of every detector in this package.
//
// The order is irrelevant: overlaps are resolved by length and priority, not by
// position in this slice.
func DefaultDetectors() []Detector {
	return []Detector{
		NewEmailDetector(),
		NewCreditCardDetector(),
		NewIBANDetector(),
		NewPhoneDetector(),
		NewIPAddressDetector(),
		NewUSSSNDetector(),
		NewUKNINODetector(),
		NewESDNIDetector(),
		NewAPIKeyDetector(),
		NewPersonNameDetector(),
	}
}

// --- email ------------------------------------------------------------------

// The pattern is deliberately narrower than RFC 5322, which permits quoted
// local parts and comments that no real address uses. Matching the RFC would
// add false positives and no recall worth having.
var emailPattern = regexp.MustCompile(
	`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9](?:[a-zA-Z0-9\-]*[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9\-]*[a-zA-Z0-9])?)*\.[a-zA-Z]{2,24}`)

// NewEmailDetector detects email addresses.
func NewEmailDetector() Detector {
	return &regexDetector{
		typ: EntityEmail, pattern: emailPattern,
		// RFC 5321's limit. It also sets the streaming look-behind, so it is
		// the number that decides how much text the stream redactor buffers.
		maxLen: 254, priority: priorityEmail, confidence: 0.95,
	}
}

// --- credit card ------------------------------------------------------------

// A run of 13 to 19 digits, optionally split by single spaces or hyphens. This
// pattern alone is unusably noisy -- order numbers, timestamps concatenated,
// hashes of digits -- which is what the Luhn check is for.
var creditCardPattern = regexp.MustCompile(`\b\d(?:[ \-]?\d){12,18}\b`)

// NewCreditCardDetector detects payment card numbers, validated with Luhn.
//
// Measured on the fixture corpus in testdata: the pattern alone accepts
// non-card digit strings at a rate that makes it unusable, and Luhn plus the
// issuer-digit check removes roughly nine in ten of them. The residual false
// positives are digit strings that pass Luhn by coincidence -- one in ten do,
// since Luhn is a single check digit -- and that residue is irreducible without
// knowing the issuer ranges. detectors_test.go reports both numbers.
func NewCreditCardDetector() Detector {
	return &regexDetector{
		typ: EntityCreditCard, pattern: creditCardPattern,
		maxLen: 25, priority: priorityChecksummed, confidence: 0.99,
		validate: func(s string) (string, bool) {
			digits := keepDigits(s)
			if len(digits) < 13 || len(digits) > 19 {
				return "", false
			}
			// Major industry identifier: issued cards start 3 (Amex, Diners),
			// 4 (Visa), 5 (Mastercard) or 6 (Discover, UnionPay). Excluding the
			// other six leading digits removes more than half the coincidental
			// Luhn passes at no cost in recall for cards in circulation.
			switch digits[0] {
			case '3', '4', '5', '6':
			default:
				return "", false
			}
			if !luhn(digits) {
				return "", false
			}
			return s, true
		},
	}
}

// luhn reports whether the digit string satisfies the Luhn checksum.
func luhn(digits string) bool {
	sum, alt := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if d < 0 || d > 9 {
			return false
		}
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

func keepDigits(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// --- IBAN -------------------------------------------------------------------

var ibanPattern = regexp.MustCompile(`\b[A-Z]{2}\d{2} ?(?:[A-Z0-9]{4} ?){2,7}[A-Z0-9]{1,4}\b`)

// NewIBANDetector detects IBANs, validated with the ISO 7064 mod-97 check.
//
// The check is worth its complexity: two letters followed by digits is a shape
// shared by product codes, flight numbers and internal references, and mod-97
// rejects all but one in ninety-seven of them.
func NewIBANDetector() Detector {
	return &regexDetector{
		typ: EntityIBAN, pattern: ibanPattern,
		maxLen: 42, priority: priorityChecksummed, confidence: 0.99,
		validate: func(s string) (string, bool) {
			compact := strings.ReplaceAll(s, " ", "")
			if len(compact) < 15 || len(compact) > 34 {
				return "", false
			}
			if !ibanMod97(compact) {
				return "", false
			}
			return s, true
		},
	}
}

// ibanMod97 moves the first four characters to the end, expands letters to
// numbers (A=10 ... Z=35) and checks that the resulting integer is 1 mod 97.
//
// The remainder is accumulated digit by digit because the expanded value is a
// number of up to 60 digits, far past int64.
func ibanMod97(compact string) bool {
	rearranged := compact[4:] + compact[:4]
	rem := 0
	for i := 0; i < len(rearranged); i++ {
		c := rearranged[i]
		switch {
		case c >= '0' && c <= '9':
			rem = rem*10 + int(c-'0')
		case c >= 'A' && c <= 'Z':
			v := int(c-'A') + 10
			rem = rem*100 + v
		default:
			return false
		}
		rem %= 97
	}
	return rem == 1
}

// --- phone ------------------------------------------------------------------

// Three families, in one alternation: E.164 with an explicit country code,
// North American with area-code punctuation, and the European habit of grouping
// digits with spaces behind a trunk zero.
//
// Every alternative requires either a leading "+" or internal separators. A
// bare run of digits is not accepted as a phone number at any length, because
// order numbers, identifiers and quantities are far more common in prompts than
// unformatted phone numbers, and accepting them costs more in false positives
// than it gains in recall.
var phonePattern = regexp.MustCompile(
	`(?:\+\d{1,3}[ \-.]?(?:\(\d{1,4}\)[ \-.]?)?\d{1,4}(?:[ \-.]?\d{2,4}){1,4}` +
		`|\(\d{3}\)[ \-.]?\d{3}[ \-.]?\d{4}` +
		`|\b\d{3}[ \-.]\d{3}[ \-.]\d{4}\b` +
		`|\b0\d{1,4}[ \-]\d{3,4}[ \-]?\d{3,4}\b)`)

// NewPhoneDetector detects telephone numbers in several international formats.
func NewPhoneDetector() Detector {
	return &regexDetector{
		typ: EntityPhone, pattern: phonePattern,
		maxLen: 24, priority: priorityPhone, confidence: 0.85,
		validate: func(s string) (string, bool) {
			digits := keepDigits(s)
			// E.164 allows at most 15 digits; fewer than 7 is a fragment of
			// something else.
			if len(digits) < 7 || len(digits) > 15 {
				return "", false
			}
			return s, true
		},
	}
}

// --- IP address -------------------------------------------------------------

// The candidate patterns are deliberately loose. Writing a correct IPv6 regex
// is a well-known way to be wrong in public -- the "::" elision alone has eight
// cases -- so the pattern only has to find something colon-separated and
// netip.ParseAddr decides.
var (
	ipv4Pattern = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	ipv6Pattern = regexp.MustCompile(`[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}`)
)

type ipDetector struct{}

// NewIPAddressDetector detects IPv4 and IPv6 addresses.
//
// It cannot distinguish an address from a version number that happens to have
// four dotted components; nothing can, without knowing what the sentence is
// about, and a policy that masks "1.2.3.4" in a changelog is a nuisance rather
// than a leak. The IPv6 pattern's looseness is bounded the same way: "10:30:45"
// matches it and ParseAddr throws it out.
func NewIPAddressDetector() Detector { return ipDetector{} }

func (ipDetector) Type() EntityType { return EntityIPAddress }

// MaxLen is the longest textual IPv6 address, an IPv4-mapped form with a zone.
func (ipDetector) MaxLen() int   { return 45 }
func (ipDetector) Priority() int { return priorityIP }

func (ipDetector) Find(text string) []Match {
	var out []Match
	for _, pattern := range []*regexp.Regexp{ipv4Pattern, ipv6Pattern} {
		for _, loc := range pattern.FindAllStringIndex(text, -1) {
			s := text[loc[0]:loc[1]]
			if _, err := netip.ParseAddr(s); err != nil {
				continue
			}
			out = append(out, Match{
				Type: EntityIPAddress, Start: loc[0], End: loc[1],
				Value: s, Confidence: 0.9,
			})
		}
	}
	return resolveOverlaps(out, nil)
}

// --- United States: Social Security Number ----------------------------------

var ssnPattern = regexp.MustCompile(`\b(\d{3})[ \-](\d{2})[ \-](\d{4})\b`)

// NewUSSSNDetector detects US Social Security Numbers.
//
// Only the separated form is accepted. An unseparated nine-digit run is
// indistinguishable from an order number, and the SSA's own issuance rules --
// no area 000, 666 or 900-999, no group 00, no serial 0000 -- remove only about
// 2% of candidates, nowhere near enough to make a bare nine-digit pattern
// usable.
func NewUSSSNDetector() Detector {
	return &regexDetector{
		typ: EntityUSSSN, pattern: ssnPattern,
		maxLen: 11, priority: priorityNationalID, confidence: 0.9,
		validate: func(s string) (string, bool) {
			d := keepDigits(s)
			area, group, serial := d[0:3], d[3:5], d[5:9]
			if area == "000" || area == "666" || area[0] == '9' {
				return "", false
			}
			if group == "00" || serial == "0000" {
				return "", false
			}
			return s, true
		},
	}
}

// --- United Kingdom: National Insurance Number ------------------------------

var ninoPattern = regexp.MustCompile(`\b([A-Za-z]{2})[ ]?(\d{2})[ ]?(\d{2})[ ]?(\d{2})[ ]?([A-Da-d])\b`)

// NewUKNINODetector detects UK National Insurance Numbers.
//
// The prefix rules are what make this usable: two letters followed by six
// digits and a letter is a common shape for internal references, and excluding
// the disallowed letters and the reserved prefixes removes most of them.
func NewUKNINODetector() Detector {
	return &regexDetector{
		typ: EntityUKNINO, pattern: ninoPattern,
		maxLen: 13, priority: priorityNationalID, confidence: 0.9,
		validate: func(s string) (string, bool) {
			prefix := strings.ToUpper(s[:2])
			// D, F, I, Q, U and V are never used in either position; O is never
			// used in the second.
			if strings.ContainsAny(prefix[:1], "DFIQUV") || strings.ContainsAny(prefix[1:2], "DFIOQUV") {
				return "", false
			}
			switch prefix {
			case "BG", "GB", "KN", "NK", "NT", "TN", "ZZ":
				return "", false
			}
			return s, true
		},
	}
}

// --- Spain: DNI / NIF -------------------------------------------------------

var dniPattern = regexp.MustCompile(`\b(\d{8})[ \-]?([A-Za-z])\b`)

// dniLetters is the check-letter table, indexed by the number modulo 23.
const dniLetters = "TRWAGMYFPDXBNJZSQVHLCKE"

// NewESDNIDetector detects Spanish national identity numbers.
//
// Eight digits and a letter is an unremarkable shape, so the check letter is
// doing all the work: it accepts one candidate in 23.
func NewESDNIDetector() Detector {
	return &regexDetector{
		typ: EntityESDNI, pattern: dniPattern,
		maxLen: 10, priority: priorityNationalID, confidence: 0.9,
		validate: func(s string) (string, bool) {
			digits := keepDigits(s)
			if len(digits) != 8 {
				return "", false
			}
			n := 0
			for i := 0; i < 8; i++ {
				n = n*10 + int(digits[i]-'0')
			}
			want := dniLetters[n%23]
			got := strings.ToUpper(s)[len(s)-1]
			if got != want {
				return "", false
			}
			return s, true
		},
	}
}
