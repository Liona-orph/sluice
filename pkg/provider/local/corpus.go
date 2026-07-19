package local

// defaultCorpus is the sentence pool responses are assembled from.
//
// The sentences are about the gateway's own subject matter for one practical
// reason: generated text ends up in test failure output, and text that reads
// like a plausible answer makes a diff legible in a way that lorem ipsum does
// not. They are also deliberately varied in length, so that chunking and token
// counting are exercised across a range rather than at one size.
var defaultCorpus = []string{
	"The request was handled without contacting an external service.",
	"Costs are computed from token counts rather than estimated after the fact.",
	"A cache hit returns the stored answer and records no new spend.",
	"Sensitive values are replaced before the prompt leaves the building.",
	"Failing over to a second provider keeps the product available.",
	"Rate limits are a normal condition and are retried with backoff.",
	"The answer depends on which model served the request.",
	"Streaming delivers the first token sooner without changing the total.",
	"An unknown model has no price, so the gateway refuses to guess one.",
	"Redaction is reversible, so the caller sees the original values again.",
	"Determinism matters more than realism when the point is a repeatable test.",
	"Two identical requests should not be paid for twice.",
	"Short answers are cheaper and are usually enough.",
	"The context window bounds what can be asked, not what can be answered.",
	"Observability is the difference between a bill and an explanation.",
	"Latency is dominated by the model, not by the gateway in front of it.",
	"A tool call is a request for information the model does not have.",
	"Nothing here reaches the network, which is what makes it testable.",
}

// fillerWords supply deterministic tool-call arguments. They are ordinary words
// so that a generated argument is readable in a test failure.
var fillerWords = []string{
	"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel",
	"india", "juliet", "kilo", "lima", "mike", "november", "oscar", "papa",
}
