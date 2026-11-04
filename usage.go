package main

import (
	"fmt"
	"strconv"
)

// The dashboard's token-usage panel. Everything here is derived from
// request_logs at render time (see getUsageByClient) — usage is captured once,
// on the existing audit path, and this file only reshapes it.

// usageWindows maps the dashboard's window selector to an age cutoff. An
// allow-list rather than parsing whatever arrives: the value reaches a SQL
// comparison, and a closed set keeps it a key lookup instead of an injection
// surface. The zero value is meaningless here — callers go through
// usageWindowFor.
var usageWindows = map[string]struct {
	days  int
	label string
}{
	"24h": {days: 1, label: "24 hours"},
	"7d":  {days: 7, label: "7 days"},
	"30d": {days: 30, label: "30 days"},
}

const usageWindowDefault = "7d"

// usageWindowFor validates a window key, falling back to the default.
func usageWindowFor(key string) string {
	if _, ok := usageWindows[key]; ok {
		return key
	}
	return usageWindowDefault
}

// UsageByModel is one model's share of a client's window.
type UsageByModel struct {
	Model      string
	Requests   int
	InTokens   int
	OutTokens  int
	Streamed   int
	Unreported int
}

// Total is the combined figure the panel ranks and displays.
func (u UsageByModel) Total() int { return u.InTokens + u.OutTokens }

// ClientUsage is one client's usage over the selected window.
//
// Name comes from the request log, not the users table, so a client deleted
// after the window still gets a row; Registered and Status say which rows are
// live history and which belong to someone who is gone.
type ClientUsage struct {
	Name       string
	Registered bool
	Status     string
	Requests   int
	InTokens   int
	OutTokens  int
	Streamed   int
	Unreported int
	Models     []UsageByModel
	Bar        string // quantized ub-* class, see usageBarClass
}

// Total is the combined figure the panel ranks and displays.
func (c ClientUsage) Total() int { return c.InTokens + c.OutTokens }

// UsageTotals is the panel footer.
type UsageTotals struct {
	Clients    int
	Requests   int
	InTokens   int
	OutTokens  int
	Unreported int
}

// Total is the combined figure across every client in the window.
func (t UsageTotals) Total() int { return t.InTokens + t.OutTokens }

// UsagePanel is the dashboard's token-usage card.
type UsagePanel struct {
	Window      string
	WindowLabel string
	Clients     []ClientUsage
	Totals      UsageTotals
	// KnownClients is the registered-client count, so the footer can say how
	// much of the roster the window actually covers — an idle client should
	// read as idle, not as missing.
	KnownClients int
}

// usageBarClass maps a token count to one of eleven fixed widths.
//
// The widths are quantized classes rather than a computed percentage because
// static_css_test forbids style attributes in templates, and a percentage
// would have to be a style attribute. Ten steps keep the ranking legible at
// the width the bar gets; any non-zero count clears the ub-10 floor so the
// smallest non-zero client is still visibly present.
//
// max is the largest client total in the set, passed in by the caller so every
// bar on the page shares one scale.
func usageBarClass(tokens, max int) string {
	if tokens <= 0 || max <= 0 {
		return "ub-0"
	}
	pct := tokens * 100 / max
	if pct < 10 {
		pct = 10
	}
	pct -= pct % 10
	if pct > 100 {
		pct = 100
	}
	return "ub-" + strconv.Itoa(pct)
}

// humanNum abbreviates a token count for the usage table.
//
// Every branch keeps three significant figures, which does two things a plain
// "divide by a thousand" does not: the column stays narrow enough to sit beside
// four others, and a client is rankable against its neighbour at a glance —
// raw digits in the millions defeat both. Fixed decimals per band also make the
// points line up under tabular-nums.
//
// It is lossy on purpose. The exact figures are one click away in the request
// log, and this panel's job is proportion, not accounting.
func humanNum(n int) string {
	switch {
	case n < 1000:
		return strconv.Itoa(n)
	case n < 10_000:
		return fmt.Sprintf("%.2fK", float64(n)/1e3)
	case n < 100_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	case n < 1_000_000:
		return fmt.Sprintf("%.0fK", float64(n)/1e3)
	case n < 10_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n < 100_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	default:
		return fmt.Sprintf("%.0fM", float64(n)/1e6)
	}
}

// buildUsagePanel groups the flat (client, model) aggregates the query returns
// into the nested shape the template walks.
//
// The query orders by descending token total, and appending clients on first
// sighting preserves that order, so no sort is needed here.
func buildUsagePanel(rows []usageRow, window string, knownClients int) *UsagePanel {
	panel := &UsagePanel{
		Window:       window,
		WindowLabel:  usageWindows[window].label,
		KnownClients: knownClients,
	}

	// First pass groups, second pass scales the bars — a bar is only
	// meaningful against the largest total in the set it sits in.
	order := []string{}
	byName := map[string]*ClientUsage{}
	for _, r := range rows {
		c, ok := byName[r.UserName]
		if !ok {
			c = &ClientUsage{Name: r.UserName}
			if r.UserID.Valid {
				c.Registered = true
				c.Status = r.Status.String
			}
			byName[r.UserName] = c
			order = append(order, r.UserName)
		}
		c.Requests += r.Requests
		c.InTokens += r.InTokens
		c.OutTokens += r.OutTokens
		c.Streamed += r.Streamed
		c.Unreported += r.Unreported
		c.Models = append(c.Models, UsageByModel{
			Model: r.Model, Requests: r.Requests, InTokens: r.InTokens,
			OutTokens: r.OutTokens, Streamed: r.Streamed, Unreported: r.Unreported,
		})

		panel.Totals.Requests += r.Requests
		panel.Totals.InTokens += r.InTokens
		panel.Totals.OutTokens += r.OutTokens
		panel.Totals.Unreported += r.Unreported
	}
	panel.Totals.Clients = len(order)

	max := 0
	for _, name := range order {
		if t := byName[name].Total(); t > max {
			max = t
		}
	}
	for _, name := range order {
		c := byName[name]
		c.Bar = usageBarClass(c.Total(), max)
		panel.Clients = append(panel.Clients, *c)
	}
	return panel
}
