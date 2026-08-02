package server

import (
	_ "embed"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/sluice-gw/sluice/internal/redact"
)

// dashboardHTML is the operator dashboard, compiled into the binary.
//
// One file, no build step, no npm, no CDN. The reasons are operational rather
// than aesthetic: a gateway is often deployed where there is no egress to a CDN,
// a dashboard that needs a build step rots the moment nobody runs the build,
// and a security product that pulls JavaScript from a third party at runtime
// has added a supply chain to the thing it was meant to protect.
//
// It reads /v1/stats, which needs a key, so the page asks for one and keeps it
// in sessionStorage. That is a deliberate choice over serving the dashboard
// unauthenticated: the stats include spend, key identifiers and recent
// redaction counts, and none of that should be readable by anyone who can reach
// the port.
//
//go:embed dashboard.html
var dashboardHTML []byte

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page loads nothing external, so the policy can be maximally strict.
	// 'unsafe-inline' is required because the styles and the script are in the
	// document; a nonce would be better and would mean generating one per
	// request for a page that is otherwise a static byte slice.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src data:")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if _, err := w.Write(dashboardHTML); err != nil {
		s.log.WarnContext(r.Context(), "failed to write the dashboard", "error", err)
	}
}

// redactionHeader renders the per-type counts as "email=2,phone=1", sorted so
// that the header is stable and therefore diffable in a test.
func redactionHeader(counts map[redact.EntityType]int) string {
	types := make([]string, 0, len(counts))
	for t := range counts {
		types = append(types, string(t))
	}
	sort.Strings(types)
	parts := make([]string, 0, len(types))
	for _, t := range types {
		parts = append(parts, t+"="+strconv.Itoa(counts[redact.EntityType(t)]))
	}
	return strings.Join(parts, ",")
}
