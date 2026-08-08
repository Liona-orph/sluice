package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/sluice-gw/sluice/internal/audit"
	"github.com/sluice-gw/sluice/internal/config"
	"github.com/sluice-gw/sluice/pkg/llm"
)

// runReplay re-prices an audit log.
//
// This is the question the audit log exists to make answerable. A cost report
// tells you what you spent; it does not tell you whether you should have spent
// it. Replay answers "what would this traffic have cost on the other model",
// using the token counts that were actually measured rather than a projection,
// so the comparison is arithmetic rather than a guess.
//
// What it can answer honestly:
//
//   - what the same traffic would have cost at a different price, whether that
//     price comes from a different model in the table or from a negotiated rate
//     stated in a config file;
//   - what the cache saved, by pricing the hits at what they would have cost
//     had they gone upstream;
//   - which routes and which keys the spend is concentrated in.
//
// What it cannot: predict the output length a different model would have
// produced. A terser model produces fewer completion tokens for the same
// prompt and would cost less than this arithmetic says; a chattier one more.
// The replay holds the token counts fixed and says so in its output, because a
// tool that silently modelled output length would be presenting a guess with
// the authority of a measurement.
func runReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: sluice replay --audit <file> [flags]

Re-prices a recorded audit log so that you can answer "what would this have
cost on the other model". Token counts are held at their recorded values; only
the price changes. See the note at the end of the output.

flags:
`)
		fs.PrintDefaults()
	}
	var (
		auditPath = fs.String("audit", "", "audit log to read, or - for stdin (required)")
		cfgPath   = fs.String("config", "", "config file whose pricing overrides to apply")
		asModel   = fs.String("as", "", "price every request as if it had been served by this model")
		jsonOut   = fs.Bool("json", false, "emit JSON instead of a table")
		byKey     = fs.Bool("by-key", false, "group by API key instead of by model")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *auditPath == "" {
		fs.Usage()
		return errors.New("replay: --audit is required")
	}

	pricing := llm.DefaultPricing()
	if *cfgPath != "" {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			return err
		}
		pricing = cfg.PricingTable()
	}
	if *asModel != "" {
		if _, ok := pricing.Price(*asModel); !ok {
			return fmt.Errorf("replay: no price for model %q; known models are %v", *asModel, pricing.Models())
		}
	}

	records, err := audit.ReadFile(*auditPath)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return audit.ErrNoRecords
	}

	rep := buildReport(records, pricing, *asModel, *byKey)
	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return rep.write(os.Stdout)
}

// Group is one row of the replay report.
type Group struct {
	Name string `json:"name"`
	// Requests counts records in this group; Served excludes cache hits and
	// failures, because those are the ones that actually cost money.
	Requests     int `json:"requests"`
	Served       int `json:"served"`
	CacheHits    int `json:"cache_hits"`
	Errors       int `json:"errors"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// RecordedCost is what the gateway charged at the time.
	RecordedCost llm.Cost `json:"recorded_cost_nanodollars"`
	// ReplayCost is what the same tokens cost under the replay pricing.
	ReplayCost llm.Cost `json:"replay_cost_nanodollars"`
	// CacheSaved prices the cache hits as if they had gone upstream.
	CacheSaved llm.Cost `json:"cache_saved_nanodollars"`
	// Unpriced counts records whose model has no price under the replay
	// pricing, so that a zero in a column is never mistaken for free.
	Unpriced int `json:"unpriced"`
	// Estimated counts records whose token counts came from the tokenizer
	// rather than from the provider.
	Estimated int `json:"estimated"`
}

// Report is the whole replay result.
type Report struct {
	Records int    `json:"records"`
	AsModel string `json:"as_model,omitempty"`
	GroupBy string `json:"group_by"`
	Groups  []Group
	Total   Group `json:"total"`
}

func buildReport(records []audit.Record, pricing *llm.Pricing, asModel string, byKey bool) Report {
	rep := Report{Records: len(records), AsModel: asModel, GroupBy: "model"}
	if byKey {
		rep.GroupBy = "key"
	}
	groups := map[string]*Group{}
	get := func(name string) *Group {
		g, ok := groups[name]
		if !ok {
			g = &Group{Name: name}
			groups[name] = g
		}
		return g
	}

	for _, r := range records {
		name := r.RequestedModel
		if byKey {
			name = r.KeyID
		}
		if name == "" {
			name = "(none)"
		}
		g := get(name)
		g.Requests++
		rep.Total.Requests++
		if r.ErrorCode != "" {
			g.Errors++
			rep.Total.Errors++
			continue
		}
		if r.Usage.Estimated {
			g.Estimated++
			rep.Total.Estimated++
		}
		g.InputTokens += r.Usage.InputTokens
		g.OutputTokens += r.Usage.OutputTokens
		rep.Total.InputTokens += r.Usage.InputTokens
		rep.Total.OutputTokens += r.Usage.OutputTokens

		model := r.ServedModel
		if asModel != "" {
			model = asModel
		}
		cost, err := pricing.Cost(model, r.Usage)
		if err != nil {
			g.Unpriced++
			rep.Total.Unpriced++
		}

		if r.CacheHit != "" {
			g.CacheHits++
			rep.Total.CacheHits++
			// A hit cost nothing; what it saved is what it would have cost.
			g.CacheSaved += cost
			rep.Total.CacheSaved += cost
			continue
		}
		g.Served++
		rep.Total.Served++
		g.RecordedCost += r.Cost
		g.ReplayCost += cost
		rep.Total.RecordedCost += r.Cost
		rep.Total.ReplayCost += cost
	}

	rep.Groups = make([]Group, 0, len(groups))
	for _, g := range groups {
		rep.Groups = append(rep.Groups, *g)
	}
	sort.Slice(rep.Groups, func(i, j int) bool {
		if rep.Groups[i].ReplayCost != rep.Groups[j].ReplayCost {
			return rep.Groups[i].ReplayCost > rep.Groups[j].ReplayCost
		}
		return rep.Groups[i].Name < rep.Groups[j].Name
	})
	rep.Total.Name = "TOTAL"
	return rep
}

func (r Report) write(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := "MODEL"
	if r.GroupBy == "key" {
		header = "KEY"
	}
	fmt.Fprintf(tw, "%s\tREQS\tSERVED\tHITS\tERRS\tIN\tOUT\tRECORDED\tREPLAY\tDELTA\tSAVED BY CACHE\n", header)
	row := func(g Group) {
		delta := g.ReplayCost - g.RecordedCost
		sign := ""
		if delta > 0 {
			sign = "+"
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\t%s%s\t%s\n",
			g.Name, g.Requests, g.Served, g.CacheHits, g.Errors,
			g.InputTokens, g.OutputTokens,
			g.RecordedCost, g.ReplayCost, sign, delta, g.CacheSaved)
	}
	for _, g := range r.Groups {
		row(g)
	}
	fmt.Fprintf(tw, "\t\t\t\t\t\t\t\t\t\t\n")
	row(r.Total)
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("replay: %w", err)
	}

	fmt.Fprintf(w, "\n%d records", r.Records)
	if r.AsModel != "" {
		fmt.Fprintf(w, ", re-priced as %q", r.AsModel)
	}
	fmt.Fprintln(w, ".")
	if r.Total.Unpriced > 0 {
		fmt.Fprintf(w, "%d records had no price under this pricing table and contributed zero. "+
			"That is a gap in the table, not a free request.\n", r.Total.Unpriced)
	}
	if r.Total.Estimated > 0 {
		fmt.Fprintf(w, "%d records carry estimated token counts (the provider reported none), "+
			"so their cost inherits the tokenizer's measured 0.68%% mean error.\n", r.Total.Estimated)
	}
	fmt.Fprintln(w, "Token counts are held at their recorded values: this prices the same tokens "+
		"differently, it does not predict how many tokens another model would have produced.")
	return nil
}
