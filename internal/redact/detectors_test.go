package redact

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findValues is the shape most detector tests want: the matched strings, in
// order.
func findValues(d Detector, text string) []string {
	var out []string
	for _, m := range d.Find(text) {
		out = append(out, m.Value)
	}
	return out
}

func TestEmailDetector(t *testing.T) {
	d := NewEmailDetector()
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"alice@example.com", []string{"alice@example.com"}},
		{"write to Alice.Hansen+tag@sub.example.co.uk today", []string{"Alice.Hansen+tag@sub.example.co.uk"}},
		{"a@b.io and c@d.org", []string{"a@b.io", "c@d.org"}},
		{"nothing here", nil},
		{"user@localhost", nil},    // no dotted TLD: more often a config value than an address
		{"@handle on social", nil}, // a bare mention is not an address
		{"40 units @ 12.50 each", nil},
	} {
		assert.Equalf(t, tc.want, findValues(d, tc.text), "%q", tc.text)
	}
}

func TestLuhn(t *testing.T) {
	for _, tc := range []struct {
		digits string
		want   bool
	}{
		{"4111111111111111", true},
		{"4012888888881881", true},
		{"378282246310005", true},
		{"6011111111111117", true},
		{"5500005555555559", true},
		{"4111111111111112", false},
		{"0", true}, // degenerate but consistent
		{"", true},
	} {
		assert.Equalf(t, tc.want, luhn(tc.digits), "luhn(%q)", tc.digits)
	}
}

// The claim in NewCreditCardDetector's doc comment is that the regex alone has
// an unacceptable false-positive rate and that validation fixes it. This
// measures both numbers rather than asserting the claim.
func TestCreditCardValidationFalsePositiveRate(t *testing.T) {
	const trials = 20000
	r := rand.New(rand.NewPCG(20260509, 1))
	d := NewCreditCardDetector()

	var patternHits, validatedHits int
	for i := 0; i < trials; i++ {
		// A digit run of card-like length: an order number, a timestamp, an
		// account reference. None of these is a card.
		n := 13 + r.IntN(7)
		digits := make([]byte, n)
		for j := range digits {
			digits[j] = byte('0' + r.IntN(10))
		}
		s := string(digits)
		if creditCardPattern.MatchString(s) {
			patternHits++
		}
		if len(d.Find(s)) > 0 {
			validatedHits++
		}
	}

	patternRate := float64(patternHits) / trials
	validatedRate := float64(validatedHits) / trials
	t.Logf("random %d-19 digit strings, n=%d: pattern alone accepts %.1f%%, pattern+Luhn+issuer accepts %.2f%%",
		13, trials, patternRate*100, validatedRate*100)

	assert.Greater(t, patternRate, 0.99, "the pattern alone accepts essentially every digit run")
	// Luhn passes one candidate in ten by chance; the issuer-digit check keeps
	// four leading digits out of ten. The product is 4%, and the residue is
	// irreducible without an issuer range table.
	assert.Less(t, validatedRate, 0.06)
	assert.Greater(t, validatedRate, 0.02, "a rate near zero would mean the detector rejects real cards too")
	assert.Less(t, validatedRate, patternRate/10, "validation must remove at least an order of magnitude")
}

func TestCreditCardDetector(t *testing.T) {
	d := NewCreditCardDetector()
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"card 4111111111111111 declined", []string{"4111111111111111"}},
		{"4012 8888 8888 1881", []string{"4012 8888 8888 1881"}},
		{"6011-1111-1111-1117", []string{"6011-1111-1111-1117"}},
		{"amex 378282246310005", []string{"378282246310005"}},
		{"4111111111111112 fails the checksum", nil},
		{"1234567812345670 passes Luhn but starts with 1", nil},
		{"phone 415-555-0132", nil},
	} {
		assert.Equalf(t, tc.want, findValues(d, tc.text), "%q", tc.text)
	}
}

func TestIBANMod97(t *testing.T) {
	for _, tc := range []struct {
		iban string
		want bool
	}{
		{"GB82WEST12345698765432", true},
		{"DE89370400440532013000", true},
		{"ES9121000418450200051332", true},
		{"FR1420041010050500013M02606", true},
		{"GB82WEST12345698765433", false},
		{"AB12CDEF3456789012", false},
	} {
		assert.Equalf(t, tc.want, ibanMod97(tc.iban), "%s", tc.iban)
	}
}

func TestIBANDetector(t *testing.T) {
	d := NewIBANDetector()
	assert.Equal(t, []string{"GB82WEST12345698765432"},
		findValues(d, "pay to GB82WEST12345698765432 please"))
	assert.Equal(t, []string{"GB82 WEST 1234 5698 7654 32"},
		findValues(d, "pay to GB82 WEST 1234 5698 7654 32 please"))
	assert.Nil(t, findValues(d, "reference AB12CDEF3456789012 was closed"))
}

func TestPhoneDetector(t *testing.T) {
	d := NewPhoneDetector()
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"+44 20 7946 0958", []string{"+44 20 7946 0958"}},
		{"+1 (555) 123-4567", []string{"+1 (555) 123-4567"}},
		{"(415) 555-0132", []string{"(415) 555-0132"}},
		{"415-555-0132", []string{"415-555-0132"}},
		{"415.555.0132", []string{"415.555.0132"}},
		{"020 7946 0958", []string{"020 7946 0958"}},
		{"+49 30 12345678", []string{"+49 30 12345678"}},
		// Unseparated digit runs are rejected however plausible their length:
		// order numbers vastly outnumber unformatted phone numbers in prompts.
		{"call 4155550132", nil},
		{"order 1234567 shipped", nil},
		{"the range is 100-200", nil},
	} {
		assert.Equalf(t, tc.want, findValues(d, tc.text), "%q", tc.text)
	}
}

func TestIPAddressDetector(t *testing.T) {
	d := NewIPAddressDetector()
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"from 192.168.14.203 inside", []string{"192.168.14.203"}},
		{"2001:db8::8a2e:370:7334 responded", []string{"2001:db8::8a2e:370:7334"}},
		{"::1 is loopback", []string{"::1"}},
		{"999.888.777.666 is not an address", nil},
		{"the meeting is at 10:30:45 sharp", nil},
		{"192.168.1 is a prefix", nil},
	} {
		assert.Equalf(t, tc.want, findValues(d, tc.text), "%q", tc.text)
	}
}

func TestNationalIdentifierDetectors(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    Detector
		text string
		want []string
	}{
		{"ssn hyphenated", NewUSSSNDetector(), "gave 123-45-6789 today", []string{"123-45-6789"}},
		{"ssn spaced", NewUSSSNDetector(), "gave 078 05 1120 today", []string{"078 05 1120"}},
		{"ssn area 000", NewUSSSNDetector(), "000-12-3456", nil},
		{"ssn area 666", NewUSSSNDetector(), "666-12-3456", nil},
		{"ssn area 9xx", NewUSSSNDetector(), "900-12-3456", nil},
		{"ssn group 00", NewUSSSNDetector(), "123-00-6789", nil},
		{"ssn serial 0000", NewUSSSNDetector(), "123-45-0000", nil},
		{"ssn unseparated", NewUSSSNDetector(), "123456789", nil},

		{"nino plain", NewUKNINODetector(), "AB123456C on file", []string{"AB123456C"}},
		{"nino spaced", NewUKNINODetector(), "JG 12 34 56 A", []string{"JG 12 34 56 A"}},
		{"nino reserved prefix", NewUKNINODetector(), "BG123456A", nil},
		{"nino illegal letter", NewUKNINODetector(), "DA123456A", nil},
		{"nino bad suffix", NewUKNINODetector(), "AB123456Z", nil},

		{"dni valid", NewESDNIDetector(), "gave 12345678Z", []string{"12345678Z"}},
		{"dni valid 2", NewESDNIDetector(), "gave 87654321X", []string{"87654321X"}},
		{"dni wrong letter", NewESDNIDetector(), "gave 12345678A", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, findValues(tc.d, tc.text))
		})
	}
}

// The entropy threshold is only defensible if the measurement is right, so the
// measurement is checked against values that can be derived by hand.
func TestShannonEntropy(t *testing.T) {
	assert.InDelta(t, 0.0, ShannonEntropy(""), 0)
	assert.InDelta(t, 0.0, ShannonEntropy("aaaaaaaa"), 0, "one symbol carries no information")
	assert.InDelta(t, 1.0, ShannonEntropy("abab"), 1e-9, "two equally likely symbols is one bit")
	assert.InDelta(t, 2.0, ShannonEntropy("abcd"), 1e-9)
	assert.InDelta(t, math.Log2(16), ShannonEntropy("0123456789abcdef"), 1e-9)
}

// The numbers quoted in the threshold's derivation, measured rather than
// asserted from memory.
func TestEntropyOfRepresentativeStrings(t *testing.T) {
	samples := []struct {
		kind, value string
	}{
		{"english identifier", "getUserByIdentifierName"},
		{"english word run", "internationalisation"},
		{"lowercase hex digest", "9f86d081884c7d659a2feaa0c55ad015"},
		{"mixed alphanumeric key", "Xk7Qp2Rm9Tz4Vb8Nc3Wd6Yf1"},
		{"base64-ish", "aGVsbG8gd29ybGQgdGhpcyBpcyBhIHRlc3Q"},
	}
	for _, s := range samples {
		t.Logf("%-24s %5.2f bits/char (len %d, ceiling %.2f) secret=%v",
			s.kind, ShannonEntropy(s.value), len(s.value),
			math.Log2(float64(len(s.value))), looksSecret(s.value))
	}
	assert.False(t, looksSecret("getUserByIdentifierName"))
	assert.False(t, looksSecret("internationalisation"))
	assert.True(t, looksSecret("Xk7Qp2Rm9Tz4Vb8Nc3Wd6Yf1"))
	assert.True(t, looksSecret("9f86d081884c7d659a2feaa0c55ad015"))
}

func TestAPIKeyDetector(t *testing.T) {
	d := NewAPIKeyDetector()
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"key sk-Ab3xK9mQ2pL7vR4tY8wZ1nHj set", []string{"sk-Ab3xK9mQ2pL7vR4tY8wZ1nHj"}},
		{"AKIAIOSFODNN7EXAMPLE", []string{"AKIAIOSFODNN7EXAMPLE"}},
		{"SECRET=Xk7Qp2Rm9Tz4Vb8Nc3Wd6Yf1 in env", []string{"Xk7Qp2Rm9Tz4Vb8Nc3Wd6Yf1"}},
		{"rename getUserByIdentifierName soon", nil},
		{"short abc123 value", nil},
	} {
		assert.Equalf(t, tc.want, findValues(d, tc.text), "%q", tc.text)
	}
}

func TestPersonNameDetector(t *testing.T) {
	d := NewPersonNameDetector()
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"My name is Sarah Chen and", []string{"Sarah Chen"}},
		{"Dear Mr. Thompson,", []string{"Thompson"}},
		{"Regards, Elena", []string{"Elena"}},
		{"opened by Michael Okafor in March", []string{"Michael Okafor"}},
		{"Mark the invoice as paid", nil},
		{"The renewal falls in August", nil},
		{"The Berlin office reported it", nil},
		// Sentence boundaries: the following capital is a new sentence, not a
		// surname.
		{"Ask Michael. Then file it.", []string{"Michael"}},
	} {
		assert.Equalf(t, tc.want, findValues(d, tc.text), "%q", tc.text)
	}
}

func TestResolveOverlaps(t *testing.T) {
	priority := map[EntityType]int{EntityIBAN: 110, EntityAPIKey: 100}
	matches := []Match{
		{Type: EntityAPIKey, Start: 0, End: 22},
		{Type: EntityIBAN, Start: 0, End: 22},
	}
	kept := resolveOverlaps(matches, priority)
	require.Len(t, kept, 1)
	assert.Equal(t, EntityIBAN, kept[0].Type, "a checksum beats an entropy heuristic at equal length")

	// The longer match wins regardless of priority: a digit run inside a card
	// number is part of the card, not an entity of its own.
	longer := resolveOverlaps([]Match{
		{Type: EntityAPIKey, Start: 0, End: 30},
		{Type: EntityIBAN, Start: 5, End: 22},
	}, priority)
	require.Len(t, longer, 1)
	assert.Equal(t, EntityAPIKey, longer[0].Type)
}

func TestMaxLenBoundsEveryDetector(t *testing.T) {
	// The streaming look-behind is the maximum of these, so a detector that
	// reports a MaxLen shorter than it can actually produce breaks streaming
	// redaction silently. Find must never exceed its own claim.
	corpus := loadCorpus(t)
	for _, d := range DefaultDetectors() {
		for _, doc := range corpus {
			for _, m := range d.Find(doc.Text) {
				assert.LessOrEqualf(t, m.Len(), d.MaxLen(),
					"%s returned a %d byte match but claims MaxLen %d", d.Type(), m.Len(), d.MaxLen())
			}
		}
	}
}

func BenchmarkDetect(b *testing.B) {
	r := New(DefaultPolicy())
	text := "My name is David Larsen, my email is d.larsen@example.org, my number " +
		"is +47 22 12 34 56 and the card 4111111111111111 was charged twice from 192.168.1.9."
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = r.Detect(text)
	}
}
