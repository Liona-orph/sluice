// Package llm is the provider-agnostic vocabulary every other part of Sluice
// speaks. Requests, responses, errors, usage and cost are defined here once,
// and adapters translate at the edge.
//
// The organising rule is that nothing provider-specific may appear in these
// types. When a provider offers a knob no other provider has, it goes into
// Request.ProviderOptions keyed by provider name, where it is opaque to the
// gateway: the router, the cache, the redactor and the cost accountant all work
// on the common fields and never grow a switch on vendor. The cost of that rule
// is that a caller who wants a vendor extension has to name the vendor, which
// is exactly the coupling we want to be visible.
//
// A second rule: values in this package are safe to copy and are not mutated
// after they are handed to a Provider. Slices and maps inside a Request are
// owned by the caller; a Provider that needs to modify one must clone it.
package llm

// Ptr returns a pointer to v.
//
// Optional numeric parameters are pointers because zero is a meaningful value
// for temperature and top_p, and "unset" has to be distinguishable from "zero"
// both for provider defaults and for cache keys. Ptr keeps that from turning
// every call site into a temporary variable.
func Ptr[T any](v T) *T { return &v }
