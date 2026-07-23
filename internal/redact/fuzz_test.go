package redact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzRedact asserts the two properties that must hold for arbitrary input.
//
// First, nothing panics. A redactor sits on the request path of every call
// through the gateway; an index arithmetic mistake on a rune boundary or a
// detector returning an out-of-range span would take the whole service down for
// one malformed prompt.
//
// Second, tokenization round-trips exactly. Restore(Redact(x)) == x is what the
// caller is promised, and the interesting counterexamples are inputs that
// already contain placeholder-shaped text -- which the fuzzer finds quickly, and
// which the vault's reservation of pre-existing indices is there to handle.
//
// The chunked path is checked against the whole-text path in the same corpus,
// because a streaming implementation that agrees with the batch one on
// hand-written fixtures and disagrees on adversarial input is the failure mode
// that matters.
func FuzzRedact(f *testing.F) {
	seeds := []string{
		"",
		"alice@example.com",
		"card 4111111111111111 and ssn 123-45-6789",
		"[SLUICE_EMAIL_0001] alice@example.com",
		"[ sluice-email-1 ] bob@example.com [SLUICE_EMAIL_0002]",
		"My name is Sarah Chen, +44 20 7946 0958",
		"GB82WEST12345698765432 12345678Z AB123456C",
		"sk-Ab3xK9mQ2pL7vR4tY8wZ1nHj",
		"\x00\xff\xfe invalid utf8 alice@example.com",
		"日本語 alice@example.com テスト",
		strings.Repeat("a@b.co ", 40),
		"[SLUICE__0]",
		"[SLUICE_EMAIL_999999]",
		"[SLUICE_EMAIL_0001][SLUICE_EMAIL_0001]",
		// Found by fuzzing: the same literal is a name in one position and part
		// of a longer token in another, and only the first is an entity.
		"0000000 Sara 0Sara",
	}
	for _, s := range seeds {
		f.Add(s, 1)
	}

	r := New(Policy{Default: ActionTokenize})

	f.Fuzz(func(t *testing.T, text string, split int) {
		redacted, vault := r.Redact(text)

		if got := vault.Restore(redacted); got != text {
			t.Fatalf("round trip changed the text\n input: %q\n redacted: %q\n restored: %q",
				text, redacted, got)
		}
		if utf8.ValidString(text) && !utf8.ValidString(redacted) {
			t.Fatalf("redaction produced invalid UTF-8 from valid input: %q", redacted)
		}

		// Every detected value must actually be gone, or the redactor is
		// reporting work it did not do.
		//
		// Stated as a count rather than as "the string does not appear",
		// because the same literal can legitimately survive elsewhere: the
		// fuzzer found "0000000 Sara 0Sara", where "Sara" is a name in one
		// position and part of a longer token in the other, and the detector is
		// right not to touch the second. What must hold is that each detected
		// occurrence was replaced.
		//
		// Placeholders are stripped before counting because a generated
		// placeholder is itself text that a detector may match, and an
		// occurrence the redactor created is not one it failed to remove.
		stripped := placeholderPattern.ReplaceAllString(redacted, "")
		detected := map[string]int{}
		for _, m := range r.Detect(text) {
			if m.Value != "" {
				detected[m.Value]++
			}
		}
		for value, n := range detected {
			if got, limit := strings.Count(stripped, value), strings.Count(text, value)-n; got > limit {
				t.Fatalf("%d occurrences of %q survive redaction, at most %d should\n input: %q\n redacted: %q",
					got, value, limit, text, redacted)
			}
		}

		// The streamed path must agree with the batch path at an arbitrary
		// split point.
		if split < 0 {
			split = -split
		}
		if text != "" {
			split %= len(text) + 1
		} else {
			split = 0
		}
		streamed, streamVault := feed(r, []string{text[:split], text[split:]})
		if streamed != redacted {
			t.Fatalf("stream disagreed with batch at split %d\n input: %q\n batch: %q\n stream: %q",
				split, text, redacted, streamed)
		}
		if got := streamVault.Restore(streamed); got != text {
			t.Fatalf("streamed round trip changed the text at split %d: %q -> %q", split, text, got)
		}
	})
}

// FuzzDetect exercises the detectors alone with no policy in the way, so that a
// span bug shows up as a span bug rather than as a strange replacement.
func FuzzDetect(f *testing.F) {
	for _, s := range []string{
		"",
		"a@b.co",
		"1234567890123456789012345",
		"::::::::",
		"999.999.999.999",
		"AB12 CD34 EF56 GH78 IJ90",
		"Dr. ",
		strings.Repeat("+", 300),
	} {
		f.Add(s)
	}

	r := New(Policy{})
	f.Fuzz(func(t *testing.T, text string) {
		prev := -1
		for _, m := range r.Detect(text) {
			if m.Start < 0 || m.End > len(text) || m.Start >= m.End {
				t.Fatalf("match %s has span [%d,%d) in text of length %d", m.Type, m.Start, m.End, len(text))
			}
			if m.Value != text[m.Start:m.End] {
				t.Fatalf("match %s value %q does not match its span %q", m.Type, m.Value, text[m.Start:m.End])
			}
			if m.Start < prev {
				t.Fatalf("matches are not ordered: %d after %d", m.Start, prev)
			}
			prev = m.End
		}
	})
}
