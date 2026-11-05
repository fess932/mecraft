package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Web struct {
	store *Store
	hub   *Hub
	icons *Icons
	rules *Rules
	tpl   *template.Template
}

const pageStep = 300

// NewWeb builds the UI. version tags static and icon URLs so browsers can
// cache them for good and still pick up a new build.
func NewWeb(store *Store, hub *Hub, icons *Icons, rules *Rules, templates fs.FS, version string) (*Web, error) {
	w := &Web{store: store, hub: hub, icons: icons, rules: rules}
	funcs := template.FuncMap{
		"ver":   func() string { return version },
		"icon":  func(label string) string { return icons.URL(label) + "?v=" + version },
		"short": short,
		"group": group,
		"ago":   ago,
		"pct":   pct,
		"state": stateText,
		"itemq": itemQuery,
		"num":   func(n int) string { return group(int64(n)) },
		"float": func(n int64) float64 { return float64(n) },
		"kb":    func(b float64) int64 { return int64(b / 1024) },
		"cap100": func(n int) int {
			return max(0, min(n, 100))
		},
		// mod is the mod id of an item id: "gregtech:gt.metaitem.01" -> "gregtech".
		"mod": func(name string) string {
			if i := strings.IndexByte(name, ':'); i > 0 {
				return name[:i]
			}
			return name
		},
	}
	tpl, err := template.New("").Funcs(funcs).ParseFS(templates, "*.html")
	if err != nil {
		return nil, err
	}
	w.tpl = tpl
	return w, nil
}

func (w *Web) Routes(static, icons, oc fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", w.index)
	mux.HandleFunc("GET /status", w.status)
	mux.HandleFunc("GET /view", w.view)
	mux.HandleFunc("GET /craft-form", w.craftForm)
	mux.HandleFunc("POST /craft", w.craft)
	mux.HandleFunc("POST /resync", w.resync)
	mux.HandleFunc("POST /rules", w.saveRule)
	mux.HandleFunc("POST /rules/delete", w.deleteRule)
	mux.HandleFunc("GET /pick", w.pick)
	mux.Handle("GET /static/", http.StripPrefix("/static/", cached(http.FileServerFS(static))))
	mux.Handle("GET /icons/", http.StripPrefix("/icons/", cached(http.FileServerFS(icons))))
	mux.Handle("GET /oc/", http.StripPrefix("/oc/", http.FileServerFS(oc)))
	return mux
}

// cached lets browsers keep versioned URLs (?v=...) forever and makes them
// revalidate anything else.
func cached(h http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			rw.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			rw.Header().Set("Cache-Control", "no-cache")
		}
		h.ServeHTTP(rw, r)
	})
}

// assetVersion hashes every file of the given trees except icon images
// (their index.json already changes whenever they do).
func assetVersion(trees ...fs.FS) string {
	h := sha256.New()
	for _, t := range trees {
		_ = fs.WalkDir(t, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasSuffix(p, ".png") {
				return err
			}
			data, err := fs.ReadFile(t, p)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%s\x00%d\x00", p, len(data))
			h.Write(data)
			return nil
		})
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}

func (w *Web) render(rw http.ResponseWriter, name string, data any) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := w.tpl.ExecuteTemplate(rw, name, data); err != nil {
		log.Printf("web: template %s: %v", name, err)
	}
}

func (w *Web) index(rw http.ResponseWriter, r *http.Request) {
	w.render(rw, "index", map[string]any{"Step": pageStep})
}

func (w *Web) status(rw http.ResponseWriter, r *http.Request) {
	w.render(rw, "status", w.store.Status())
}

func (w *Web) view(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	text := q.Get("q")
	tab := q.Get("tab")
	switch tab {
	case "cpus":
		w.render(rw, "cpus", map[string]any{"CPUs": w.store.CPUs()})
		return
	case "jobs":
		w.render(rw, "jobs", map[string]any{"Jobs": w.store.Jobs()})
		return
	}

	fluids := tab == "fluids"
	levels := w.rules.Levels(w.store)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = pageStep
	}
	query := ItemQuery{
		Text:          text,
		CraftableOnly: q.Get("craftable") != "",
		InStockOnly:   q.Get("stock") != "",
		SortByName:    q.Get("sort") == "name",
		Limit:         limit,
	}
	var items []Item
	var total int
	switch {
	case q.Get("ruled") != "":
		// The levels filter narrows the list like the others and stacks
		// with them.
		items, total = query.Filter(levelItems(levels, fluids))
	case fluids:
		items, total = w.store.FluidItems(query)
	default:
		items, total = w.store.Items(query)
	}
	w.render(rw, "slots", map[string]any{
		"Items": items, "Total": total, "Shown": len(items),
		"More": total > len(items), "Step": pageStep, "Levels": levels,
		"Fluids": fluids, "Ruled": q.Get("ruled") != "",
	})
}

// levelItems lists the items (or fluids) that have a stock level, also
// those missing from the network.
func levelItems(levels map[string]*RuleView, fluids bool) []Item {
	var out []Item
	for _, v := range levels {
		if v.Fluid != fluids {
			continue
		}
		it := v.Item()
		it.Size, it.Craftable = v.Stock, v.Craftable
		out = append(out, it)
	}
	return out
}

// panelItem finds an item by key in the network, falling back to its rule
// so that a rule stays editable while the item is absent.
func (w *Web) panelItem(key string) (Item, bool) {
	if it, ok := w.store.FindItem(key); ok {
		return it, true
	}
	if r, ok := w.rules.Get(key); ok {
		return r.Item(), true
	}
	return Item{}, false
}

func (w *Web) panel(rw http.ResponseWriter, it Item, data map[string]any) {
	data["Item"] = it
	if _, ok := data["Amount"]; !ok {
		data["Amount"] = int64(1)
	}
	if r, ok := w.rules.Get(it.Key()); ok {
		data["Rule"] = r
		data["Level"] = w.rules.Levels(w.store)[it.Key()]
	}
	w.render(rw, "craft", data)
}

func (w *Web) craftForm(rw http.ResponseWriter, r *http.Request) {
	it, ok := w.panelItem(r.URL.Query().Get("key"))
	if !ok {
		w.render(rw, "craft", map[string]any{"Error": "предмет пропал из сети, обнови список"})
		return
	}
	w.panel(rw, it, map[string]any{})
}

func parseCount(s string) (int64, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "")
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

func (w *Web) saveRule(rw http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	it, ok := w.panelItem(r.PostForm.Get("key"))
	if !ok {
		w.render(rw, "craft", map[string]any{"Error": "предмет пропал из сети, обнови список"})
		return
	}
	target, err1 := parseCount(r.PostForm.Get("target"))
	batch, err2 := parseCount(r.PostForm.Get("batch"))
	if err1 != nil || err2 != nil {
		w.panel(rw, it, map[string]any{"Error": "уровень и партия — целые числа"})
		return
	}
	rule := Rule{
		Key: it.Key(), Name: it.Name, Damage: it.Damage, Label: it.Label,
		Target: target, Batch: batch, Enabled: r.PostForm.Get("enabled") != "", Fluid: it.Fluid,
	}
	if r.PostForm.Get("mode") == ModeDrain {
		rule.Mode = ModeDrain
		rule.Source = r.PostForm.Get("source")
		rule.SourceLabel = w.sourceLabel(rule.Key, rule.Source)
	}
	if err := w.rules.Put(rule); err != nil {
		w.panel(rw, it, map[string]any{"Error": err.Error()})
		return
	}
	if rule.Drain() {
		w.closePanel(rw, fmt.Sprintf("Переработка сохранена: %s, пока %s больше %s %s",
			it.Label, rule.SourceLabel, group(target), rule.SourceUnit()))
		return
	}
	w.closePanel(rw, fmt.Sprintf("Уровень сохранён: %s, не меньше %s %s", it.Label, group(target), it.Unit()))
}

// sourceLabel names the resource of a drain rule: from the network, or
// from the saved rule while the resource is absent.
func (w *Web) sourceLabel(key, source string) string {
	if it, ok := w.store.FindItem(source); ok {
		return it.Label
	}
	if r, ok := w.rules.Get(key); ok && r.Source == source {
		return r.SourceLabel
	}
	return source
}

const pickLimit = 8

// pick lists items and fluids matching the search, to choose the resource
// of a drain rule.
func (w *Web) pick(rw http.ResponseWriter, r *http.Request) {
	text := strings.TrimSpace(r.URL.Query().Get("source_q"))
	if text == "" {
		w.render(rw, "pick", map[string]any{})
		return
	}
	q := ItemQuery{Text: text}
	items, _ := w.store.Items(q)
	fluids, _ := w.store.FluidItems(q)
	list := append(items, fluids...)
	// Names that start with the search go first ("вод" finds water before
	// wires), then the biggest stock.
	low := strings.ToLower(text)
	starts := func(it Item) bool { return strings.HasPrefix(strings.ToLower(it.Label), low) }
	sort.SliceStable(list, func(i, j int) bool {
		if a, b := starts(list[i]), starts(list[j]); a != b {
			return a
		}
		return list[i].Size > list[j].Size
	})
	if len(list) > pickLimit {
		list = list[:pickLimit]
	}
	w.render(rw, "pick", map[string]any{"Items": list, "Searched": true})
}

func (w *Web) deleteRule(rw http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	key := r.PostForm.Get("key")
	it, ok := w.panelItem(key)
	if err := w.rules.Delete(key); err != nil {
		w.panel(rw, it, map[string]any{"Error": err.Error()})
		return
	}
	if !ok {
		w.closePanel(rw, "Уровень удалён")
		return
	}
	w.closePanel(rw, "Уровень удалён: "+it.Label)
}

func (w *Web) craft(rw http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	it, ok := w.store.FindItem(r.PostForm.Get("key"))
	if !ok {
		w.render(rw, "craft", map[string]any{"Error": "предмет пропал из сети, обнови список"})
		return
	}
	amount, err := parseCount(r.PostForm.Get("amount"))
	if err != nil || amount <= 0 {
		w.panel(rw, it, map[string]any{"Error": "введи число больше нуля"})
		return
	}
	job, err := w.hub.Craft(it, amount, false)
	if err != nil {
		w.panel(rw, it, map[string]any{"Amount": amount, "Error": err.Error()})
		return
	}
	w.closePanel(rw, fmt.Sprintf("Заказ #%d: %s × %s %s отправлен", job.ID, it.Label, group(amount), it.Unit()))
}

// closePanel answers a finished action: the window closes (empty #craft),
// msg goes to the bar and the slot grid reloads to show the change.
func (w *Web) closePanel(rw http.ResponseWriter, msg string) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("HX-Trigger", "reload")
	fmt.Fprintf(rw, `<span id="flash" role="status" hx-swap-oob="true"><span class="flash">%s</span></span>`,
		template.HTMLEscapeString(msg))
}

func (w *Web) resync(rw http.ResponseWriter, r *http.Request) {
	msg := "Запросил полную синхронизацию"
	if err := w.hub.Resync(); err != nil {
		msg = err.Error()
	}
	fmt.Fprintf(rw, `<span class="flash">%s</span>`, template.HTMLEscapeString(msg))
}

func itemQuery(it Item) string {
	return "/craft-form?" + url.Values{"key": {it.Key()}}.Encode()
}

// short formats big counts as 12.3K / 4.56M / 7.8G.
func short(n int64) string {
	a := n
	if a < 0 {
		a = -a
	}
	switch {
	case a < 10_000:
		return strconv.FormatInt(n, 10)
	case a < 1_000_000:
		return trim(float64(n)/1e3) + "K"
	case a < 1_000_000_000:
		return trim(float64(n)/1e6) + "M"
	case a < 1_000_000_000_000:
		return trim(float64(n)/1e9) + "G"
	default:
		return trim(float64(n)/1e12) + "T"
	}
}

func trim(f float64) string {
	var s string
	switch {
	case math.Abs(f) >= 100:
		s = strconv.FormatFloat(f, 'f', 0, 64)
	case math.Abs(f) >= 10:
		s = strconv.FormatFloat(f, 'f', 1, 64)
	default:
		s = strconv.FormatFloat(f, 'f', 2, 64)
	}
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// group formats 1234567 as "1 234 567".
func group(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteRune(' ')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "никогда"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d с назад", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d мин назад", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d ч назад", int(d.Hours()))
	}
}

func pct(a, b float64) int {
	if b <= 0 {
		return 0
	}
	return int(math.Round(a / b * 100))
}

func stateText(s string) string {
	switch s {
	case "sent":
		return "отправлен"
	case "computing":
		return "расчёт"
	case "running":
		return "крафтится"
	case "done":
		return "готово"
	case "failed":
		return "ошибка"
	case "canceled":
		return "отменён"
	case "lost":
		return "потерян"
	}
	return s
}
