package main

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// statPeriod is a period of the statistics tab: how far back it looks,
// the width of one chart point and the spacing of the time labels.
type statPeriod struct {
	Key     string
	Span    time.Duration
	Step    time.Duration
	Tick    time.Duration
	PerHour bool // chart rates per hour instead of per minute
	// Smooth averages each point with the ones before it: machines put
	// out in batches, and short buckets catch a batch or a pause.
	Smooth int
}

var statPeriodList = []statPeriod{
	{"10m", 10 * time.Minute, 10 * time.Second, 2 * time.Minute, false, 3},
	{"1h", time.Hour, time.Minute, 10 * time.Minute, false, 1},
	{"6h", 6 * time.Hour, 5 * time.Minute, time.Hour, false, 1},
	{"24h", 24 * time.Hour, 15 * time.Minute, 4 * time.Hour, false, 1},
	{"7d", 7 * 24 * time.Hour, 2 * time.Hour, 24 * time.Hour, true, 1},
	{"30d", 30 * 24 * time.Hour, 6 * time.Hour, 5 * 24 * time.Hour, true, 1},
}

// sinceStep is the resolution Stats.Since reads for a span.
func sinceStep(span time.Duration) time.Duration {
	if span <= statsMinuteKeep {
		return time.Minute
	}
	return time.Hour
}

// findPeriod returns the period with the given key, an hour by default.
func findPeriod(key string) statPeriod {
	for _, p := range statPeriodList {
		if p.Key == key {
			return p
		}
	}
	return statPeriodList[1]
}

// chartColors tell the lines apart on the dark plot; a selection is capped
// at their number.
var chartColors = []string{
	"#ff5a4f", "#ffb627", "#f4e04d", "#5fd068", "#2ec4d6",
	"#4f8dff", "#b36bff", "#ff6fb5", "#c8a27a", "#e0e0e0",
}

const (
	autoLines = 5   // lines on a chart while nothing is selected
	sideRows  = 100 // rows listed per side
)

// Chart is a line chart drawn as SVG. Coordinates are in viewBox units.
type Chart struct {
	W, H           float64
	X0, Y0, PW, PH float64 // plot area
	Lines          []ChartLine
	Grid           []Tick // horizontal lines with rate labels
	XTicks         []Tick
	Cols           []ChartCol
	Unit           string // "в минуту" or "в час"
	Aria           string
	// NoData is the width of the part on the left the history does not
	// reach yet; zero when it covers the whole chart.
	NoData float64
}

type ChartLine struct {
	Color  string
	Points string
}

// ChartCol is the hover area of one point in time.
type ChartCol struct {
	X, W  float64
	Title string
	Lines string
}

type Tick struct {
	Pos    float64
	Label  string
	Anchor string
}

// SideRow is one item in the list under a chart.
type SideRow struct {
	Item
	Amount int64
	Rate   string
	Bar    int // percent of the biggest amount on the side
	Style  template.CSS
	On     bool // selected by the player
	Auto   bool // drawn because nothing is selected
}

// StatSide is one half of the screen: what came in or what left.
type StatSide struct {
	Kind  string // "in" or "out"
	Title string
	Rows  []SideRow
	More  int
	Chart Chart
	Auto  bool
	Empty bool
}

// splitSel reads the selected keys: separated by U+001F, at most one per
// chart colour.
func splitSel(v string) []string {
	var out []string
	seen := map[string]bool{}
	for _, k := range strings.Split(v, "\x1f") {
		if k != "" && !seen[k] && len(out) < len(chartColors) {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// statsCharts renders the two halves of the statistics screen.
func (w *Web) statsCharts(rw http.ResponseWriter, q url.Values) {
	p := findPeriod(q.Get("period"))
	kind := q.Get("kind")
	text := strings.ToLower(strings.TrimSpace(q.Get("q")))
	sel := splitSel(q.Get("sel"))
	now := time.Now()

	var flows map[string]Flow
	// Rates count only the time with data, not the whole period: right
	// after the first start there are a few minutes of history at most.
	covered := p.Span
	chartFrom := time.Time{}
	if w.hub.Stats != nil {
		flows = w.hub.Stats.Since(now.Add(-p.Span))
		if known := w.hub.Stats.Known(sinceStep(p.Span)); now.Sub(known) < covered {
			covered = max(now.Sub(known), time.Minute)
		}
		chartFrom = w.hub.Stats.Known(p.Step)
	}
	items := map[string]Item{}
	itemOf := func(k string) Item {
		it, ok := items[k]
		if !ok {
			it = w.statItem(k)
			items[k] = it
		}
		return it
	}

	sides := []*StatSide{{Kind: "in", Title: "Пришло"}, {Kind: "out", Title: "Ушло"}}
	color := map[string]string{}
	for i, k := range sel {
		color[k] = chartColors[i]
	}
	spanMin := covered.Minutes()
	for _, sd := range sides {
		var rows []SideRow
		for k, f := range flows {
			amount := f.In
			if sd.Kind == "out" {
				amount = f.Out
			}
			if amount <= 0 {
				continue
			}
			it := itemOf(k)
			if (kind == "items" && it.Fluid) || (kind == "fluids" && !it.Fluid) {
				continue
			}
			if text != "" && !strings.Contains(strings.ToLower(it.Label), text) && !strings.Contains(strings.ToLower(it.Name), text) {
				continue
			}
			rows = append(rows, SideRow{Item: it, Amount: amount})
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Amount != rows[j].Amount {
				return rows[i].Amount > rows[j].Amount
			}
			return rows[i].Key() < rows[j].Key()
		})
		sd.Empty = len(rows) == 0
		if len(rows) > sideRows {
			sd.More = len(rows) - sideRows
			rows = rows[:sideRows]
		}
		sd.Auto = len(sel) == 0
		for i := range rows {
			r := &rows[i]
			k := r.Key()
			if p.PerHour {
				r.Rate = rateText(float64(r.Amount)/(spanMin/60)) + " " + r.Unit() + "/ч"
			} else {
				r.Rate = rateText(float64(r.Amount)/spanMin) + " " + r.Unit() + "/мин"
			}
			r.Bar = int(math.Ceil(float64(r.Amount) * 100 / float64(rows[0].Amount)))
			if c, ok := color[k]; ok {
				r.On = true
				r.Style = template.CSS("--c: " + c)
			} else if sd.Auto && i < autoLines {
				r.Auto = true
				r.Style = template.CSS("--c: " + chartColors[i])
			}
		}
		sd.Rows = rows
	}

	// Lines: the selection on both sides, or each side's leaders.
	type line struct{ key, color string }
	lines := make([][]line, len(sides))
	need := map[string]bool{}
	for i, sd := range sides {
		if len(sel) > 0 {
			for _, k := range sel {
				lines[i] = append(lines[i], line{k, color[k]})
			}
		} else {
			for j := 0; j < len(sd.Rows) && j < autoLines; j++ {
				lines[i] = append(lines[i], line{sd.Rows[j].Key(), chartColors[j]})
			}
		}
		for _, l := range lines[i] {
			need[l.key] = true
		}
	}
	keys := make([]string, 0, len(need))
	for k := range need {
		keys = append(keys, k)
	}
	n := int(p.Span / p.Step)
	to := now.Truncate(p.Step)
	var series map[string][]Flow
	if w.hub.Stats != nil {
		series = w.hub.Stats.Series(keys, to, p.Step, n)
	}
	for i, sd := range sides {
		var cl []chartLine
		for _, l := range lines[i] {
			ser := series[l.key]
			vals := make([]float64, n)
			for j := 0; j < n && j < len(ser); j++ {
				v := ser[j].In
				if sd.Kind == "out" {
					v = ser[j].Out
				}
				vals[j] = float64(v)
			}
			it := itemOf(l.key)
			cl = append(cl, chartLine{label: it.Label, unit: it.Unit(), color: l.color, amounts: vals})
		}
		sd.Chart = buildChart(cl, p, to, n, chartFrom)
		sd.Chart.Aria = sd.Title + " за период, " + sd.Chart.Unit
	}

	w.render(rw, "statcharts", map[string]any{"Sides": sides, "Selected": len(sel), "Max": len(chartColors)})
}

type chartLine struct {
	label, unit, color string
	amounts            []float64 // per bucket
}

// buildChart lays out the lines of one chart. Amounts per bucket become
// rates per minute (or per hour for long periods). Buckets that end before
// known, when the history begins, are left out.
func buildChart(lines []chartLine, p statPeriod, to time.Time, n int, known time.Time) Chart {
	c := Chart{W: 560, H: 236, X0: 56, Y0: 10, PW: 494, PH: 198, Unit: "в минуту"}
	per := p.Step.Minutes()
	if p.PerHour {
		per = p.Step.Hours()
		c.Unit = "в час"
	}
	if p.Smooth > 1 {
		c.Unit += ", среднее за " + spanText(time.Duration(p.Smooth)*p.Step)
	}
	from := to.Add(-time.Duration(n) * p.Step)
	first := 0 // first bucket with data
	if known.After(from) {
		first = min(n, int(known.Sub(from)/p.Step))
	}
	rates := make([][]float64, len(lines))
	top := 0.0
	for i, l := range lines {
		rates[i] = make([]float64, n)
		for j := first; j < n && j < len(l.amounts); j++ {
			// trailing average over the buckets with data
			sum, cnt := 0.0, 0
			for k := j; k > j-max(p.Smooth, 1) && k >= first; k-- {
				sum += l.amounts[k]
				cnt++
			}
			v := sum / float64(cnt) / per
			rates[i][j] = v
			top = max(top, v)
		}
	}
	step := niceStep(top / 4)
	top = math.Max(step*4, math.Ceil(top/step)*step)
	for v := 0.0; v <= top+step/2; v += step {
		c.Grid = append(c.Grid, Tick{Pos: c.Y0 + c.PH - v/top*c.PH, Label: rateText(v)})
	}

	xAt := func(t time.Time) float64 {
		return c.X0 + float64(t.Sub(from))/float64(to.Sub(from))*c.PW
	}
	for t := to; !t.Before(from); t = t.Add(-p.Tick) {
		tk := Tick{Pos: xAt(t), Label: "−" + spanText(to.Sub(t)), Anchor: "middle"}
		if t.Equal(to) {
			tk.Label, tk.Anchor = "сейчас", "end"
		} else if t.Sub(from) < p.Tick/2 {
			tk.Anchor = "start"
		}
		c.XTicks = append(c.XTicks, tk)
	}

	bw := c.PW / float64(n)
	c.NoData = float64(first) * bw
	yOf := func(v float64) float64 { return c.Y0 + c.PH - v/top*c.PH }
	for i, l := range lines {
		var b strings.Builder
		for j := first; j < n; j++ {
			fmt.Fprintf(&b, "%.1f,%.1f ", c.X0+(float64(j)+0.5)*bw, yOf(rates[i][j]))
		}
		c.Lines = append(c.Lines, ChartLine{Color: l.color, Points: strings.TrimSpace(b.String())})
	}
	unit := "/мин"
	if p.PerHour {
		unit = "/ч"
	}
	for j := range n {
		start := from.Add(time.Duration(j) * p.Step)
		col := ChartCol{X: c.X0 + float64(j)*bw, W: bw, Title: bucketText(start, p)}
		if j < first {
			col.Lines = "нет данных: сервер ещё не записывал"
			c.Cols = append(c.Cols, col)
			continue
		}
		type val struct {
			text string
			v    float64
		}
		var vs []val
		for i, l := range lines {
			v := rates[i][j]
			vs = append(vs, val{fmt.Sprintf("%s: %s %s%s", l.label, rateText(v), l.unit, unit), v})
		}
		sort.SliceStable(vs, func(a, b int) bool { return vs[a].v > vs[b].v })
		parts := make([]string, len(vs))
		for i, v := range vs {
			parts[i] = v.text
		}
		col.Lines = strings.Join(parts, "\n")
		c.Cols = append(c.Cols, col)
	}
	return c
}

// niceStep rounds a grid step up to 1, 2, 2.5 or 5 times a power of ten.
func niceStep(v float64) float64 {
	if v <= 0 {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if v <= m*p {
			return m * p
		}
	}
	return 10 * p
}

// rateText shows a rate: whole numbers grouped or shortened, small ones
// with a decimal.
func rateText(v float64) string {
	switch {
	case v == 0:
		return "0"
	case v >= 10_000:
		return short(int64(math.Round(v)))
	case v >= 100:
		return group(int64(math.Round(v)))
	case v >= 10:
		return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
	default:
		return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
	}
}

// spanText shows a time span for chart labels: "30 с", "10 мин", "4 ч", "2 д".
func spanText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d с", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d мин", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d ч", int(d.Hours()))
	default:
		return fmt.Sprintf("%d д", int(d.Hours()/24))
	}
}

// bucketText names the time a chart point covers.
func bucketText(start time.Time, p statPeriod) string {
	end := start.Add(p.Step)
	start, end = start.Local(), end.Local()
	switch {
	case p.Step < time.Minute:
		return start.Format("15:04:05") + "–" + end.Format("15:04:05")
	case p.Span <= 24*time.Hour:
		return start.Format("15:04") + "–" + end.Format("15:04")
	default:
		return start.Format("02.01 15:04") + "–" + end.Format("15:04")
	}
}
