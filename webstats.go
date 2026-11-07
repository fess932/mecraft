package main

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// statsPeriods are the periods of the statistics tab, by form value.
var statsPeriods = map[string]time.Duration{
	"1h":  time.Hour,
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// StatRow is one item of the statistics table.
type StatRow struct {
	Item
	Flow
	PerHour int64 // net change per hour over the period
}

func (r StatRow) Net() int64 { return r.Flow.Net() }

// statsView renders the income and spending of every item over a period,
// filtered by the search and sorted by a column.
func (w *Web) statsView(rw http.ResponseWriter, q url.Values) {
	period, ok := statsPeriods[q.Get("period")]
	if !ok {
		period = time.Hour
	}
	kind := q.Get("kind")
	text := strings.ToLower(strings.TrimSpace(q.Get("q")))
	col, desc := q.Get("ssort"), q.Get("sdir") != "asc"
	if col != "name" && col != "in" && col != "out" {
		col = "net"
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = pageStep
	}

	var rows []StatRow
	if w.hub.Stats != nil {
		for key, f := range w.hub.Stats.Since(time.Now().Add(-period)) {
			if f.In == 0 && f.Out == 0 && f.Fix == 0 {
				continue
			}
			it := w.statItem(key)
			if (kind == "items" && it.Fluid) || (kind == "fluids" && !it.Fluid) {
				continue
			}
			if text != "" && !strings.Contains(strings.ToLower(it.Label), text) && !strings.Contains(strings.ToLower(it.Name), text) {
				continue
			}
			rows = append(rows, StatRow{Item: it, Flow: f, PerHour: int64(float64(f.Net()) / period.Hours())})
		}
	}
	sortStats(rows, col, desc)
	total := len(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	w.render(rw, "stats", map[string]any{
		"Rows": rows, "Total": total, "Shown": len(rows), "More": total > len(rows),
		"Step": pageStep, "Sort": col, "Desc": desc, "Long": period > time.Hour,
		"Searched": text != "" || kind != "",
	})
}

// statItem names a key of the statistics: from the network, or from the
// key itself for what is gone.
func (w *Web) statItem(key string) Item {
	if it, ok := w.store.FindItem(key); ok {
		return it
	}
	if name, ok := strings.CutPrefix(key, fluidKeyPrefix); ok {
		return Item{Name: name, Label: name, Fluid: true}
	}
	parts := strings.SplitN(key, "|", 3)
	if len(parts) != 3 {
		return Item{Name: key, Label: key}
	}
	dmg, _ := strconv.Atoi(parts[1])
	return Item{Name: parts[0], Damage: dmg, Label: parts[2]}
}

// sortStats orders rows by a column: name, in, out or net (the default).
// Ties go by name so the order does not jump between refreshes.
func sortStats(rows []StatRow, col string, desc bool) {
	val := func(r StatRow) int64 {
		switch col {
		case "in":
			return r.In
		case "out":
			return r.Out
		default:
			return r.Net()
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if col == "name" {
			if la, lb := strings.ToLower(a.Label), strings.ToLower(b.Label); la != lb {
				return (la < lb) != desc
			}
			return a.Key() < b.Key()
		}
		if va, vb := val(a), val(b); va != vb {
			return (va < vb) != desc
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		return a.Key() < b.Key()
	})
}
