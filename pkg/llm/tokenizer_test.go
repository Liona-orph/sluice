package llm

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tokenCase struct {
	Text   string `json:"text"`
	Tokens int    `json:"tokens"`
}

func loadTokenCorpus(t *testing.T) []tokenCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/token_corpus.json")
	require.NoError(t, err)
	var doc struct {
		Cases []tokenCase `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.NotEmpty(t, doc.Cases)
	return doc.Cases
}

// Accuracy floors. They are assertions about a measured property, not
// aspirations: if a change to the vocabulary or the scanner pushes the error
// past them, the cost accounting built on top has silently become less
// trustworthy and the build should say so.
const (
	maxMeanAbsolutePercentageError = 3.0
	maxAbsoluteErrorTokens         = 1
)

// A corpus assembled by the same person who wrote the tokenizer flatters it.
// This is the independent check, and its bound comes from outside this
// repository: OpenAI documents English text as roughly 100 tokens per 75 words
// for cl100k_base, i.e. 1.333 tokens per word. A counter within 20% of that on
// ordinary prose is still modelling the encoding; one outside it has stopped,
// whatever the hand-derived corpus says.
func TestApproxTokensPerWord(t *testing.T) {
	var tok Approx
	const ruleOfThumb = 100.0 / 75.0
	const tolerance = 0.20
	const prose = `Sluice sits between an organisation's applications and the ` +
		`language models they depend on. It exists because four problems arrive ` +
		`together once usage becomes real: nobody can say what the spending is, ` +
		`sensitive information finds its way into prompts sent to third parties, ` +
		`the same question is paid for repeatedly, and a single upstream outage ` +
		`removes a product feature entirely. Handling those in one place is ` +
		`cheaper and considerably safer than handling them badly in twenty.`

	tokens := tok.CountTokens(prose)
	words := len(strings.Fields(prose))
	perWord := float64(tokens) / float64(words)
	t.Logf("English prose: %d chars, %d words, %d tokens; %.3f tokens/word, %.2f chars/token",
		len(prose), words, tokens, perWord, float64(len(prose))/float64(tokens))
	assert.Greater(t, perWord, ruleOfThumb*(1-tolerance))
	assert.Less(t, perWord, ruleOfThumb*(1+tolerance))
}

func TestApproxAccuracy(t *testing.T) {
	cases := loadTokenCorpus(t)
	var tok Approx

	var (
		sumAbsPct  float64
		pctCount   int
		worstCase  tokenCase
		worstDelta int
	)
	for _, c := range cases {
		got := tok.CountTokens(c.Text)
		delta := got - c.Tokens
		if abs(delta) > abs(worstDelta) {
			worstDelta, worstCase = delta, c
		}
		assert.LessOrEqualf(t, abs(delta), maxAbsoluteErrorTokens,
			"%q: got %d tokens, reference %d", c.Text, got, c.Tokens)
		if c.Tokens > 0 {
			sumAbsPct += math.Abs(float64(delta)) / float64(c.Tokens) * 100
			pctCount++
		}
	}

	mape := sumAbsPct / float64(pctCount)
	t.Logf("mean absolute percentage error: %.2f%% over %d cases; worst case %+d tokens on %q",
		mape, len(cases), worstDelta, worstCase.Text)
	assert.LessOrEqual(t, mape, maxMeanAbsolutePercentageError)
}

// The scanner's digit rule is not an approximation, so it is asserted exactly:
// a regression here would mean numeric prompts are mispriced.
func TestApproxDigitGrouping(t *testing.T) {
	var tok Approx
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"1", 1}, {"12", 1}, {"123", 1}, {"1234", 2}, {"123456", 2},
		{"1234567", 3}, {"1234567890", 4},
	} {
		assert.Equalf(t, tc.want, tok.CountTokens(tc.in), "digits %q", tc.in)
	}
}

func TestApproxProperties(t *testing.T) {
	var tok Approx

	t.Run("never negative and zero only for empty", func(t *testing.T) {
		for _, s := range []string{"", "x", " ", "\x00", "日本語", "🙂"} {
			n := tok.CountTokens(s)
			assert.GreaterOrEqual(t, n, 0, "%q", s)
			assert.Equalf(t, s == "", n == 0, "%q counted %d", s, n)
		}
	})

	t.Run("subadditive over concatenation", func(t *testing.T) {
		// Splitting text can only create tokens, never destroy them: a merge
		// that spans the split point is lost. Any counter that violated this
		// would produce a cache-hit cost lower than the miss it replaced.
		a, b := "the quick brown", " fox jumps over"
		assert.LessOrEqual(t, tok.CountTokens(a+b), tok.CountTokens(a)+tok.CountTokens(b))
	})

	t.Run("scales with length", func(t *testing.T) {
		short := tok.CountTokens("the model")
		long := tok.CountTokens("the model produced a considerably longer response than expected")
		assert.Greater(t, long, short)
	})
}

func TestCountMessagesIncludesFraming(t *testing.T) {
	var tok Approx
	msgs := []Message{
		{Role: RoleSystem, Content: "You are a helpful assistant."},
		{Role: RoleUser, Content: "hello"},
	}
	bare := tok.CountTokens("You are a helpful assistant.") + tok.CountTokens("hello")
	framed := tok.CountMessages(msgs)
	assert.Greater(t, framed, bare, "framing must be charged for")
	assert.Less(t, framed, bare+40, "framing must not dominate")
}

func TestCountRequestCountsTools(t *testing.T) {
	var tok Approx
	req := Request{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "what is the weather"}},
	}
	withoutTools := tok.CountRequest(req)
	req.Tools = []Tool{{
		Name:        "get_weather",
		Description: "Look up the current weather for a city",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
	}}
	assert.Greater(t, tok.CountRequest(req), withoutTools)
}

func TestEstimateUsageMarksEstimated(t *testing.T) {
	req := Request{Model: "gpt-4o", Messages: []Message{{Role: RoleUser, Content: "hello world"}}}
	resp := Response{Message: Message{Role: RoleAssistant, Content: "hello back"}}
	u := EstimateUsage(DefaultTokenizer(), req, resp)
	assert.True(t, u.Estimated)
	assert.Positive(t, u.InputTokens)
	assert.Positive(t, u.OutputTokens)
}

// A non-Approx tokenizer must still work through EstimateUsage; the fallback
// path is easy to break because Approx is the only implementation in-tree.
type fixedTokenizer int

func (f fixedTokenizer) CountTokens(string) int { return int(f) }

func TestEstimateUsageWithForeignTokenizer(t *testing.T) {
	req := Request{Messages: []Message{{Role: RoleUser, Content: "x"}, {Role: RoleAssistant, Content: "y"}}}
	resp := Response{Message: Message{Content: "z"}}
	u := EstimateUsage(fixedTokenizer(7), req, resp)
	assert.Equal(t, 7, u.OutputTokens)
	assert.Equal(t, 2*(tokensPerMessage+7)+tokensReplyPrime, u.InputTokens)
}

func BenchmarkApproxCountTokens(b *testing.B) {
	var tok Approx
	text := "The quick brown fox jumps over the lazy dog. " +
		"We need to know how much this costs before we can control it. 1234567890"
	b.SetBytes(int64(len(text)))
	for i := 0; i < b.N; i++ {
		_ = tok.CountTokens(text)
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
