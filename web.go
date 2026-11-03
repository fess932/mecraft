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
	mux.HandleFunc("POST /rules/toggle", w.toggleRule)
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
	switch q.Get("tab") {
	case "fluids":
		w.render(rw, "fluids", map[string]any{"Fluids": w.store.Fluids(text)})
	case "cpus":
		w.render(rw, "cpus", map[string]any{"CPUs": w.store.CPUs()})
	case "jobs":
		w.render(rw, "jobs", map[string]any{"Jobs": w.store.Jobs()})
	case "rules":
		w.render(rw, "rules", map[string]any{"Rules": w.rules.Views(w.store)})
	default:
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit <= 0 {
			limit = pageStep
		}
		items, total := w.store.Items(ItemQuery{
			Text:          text,
			CraftableOnly: q.Get("craftable") != "",
			InStockOnly:   q.Get("stock") != "",
			SortByName:    q.Get("sort") == "name",
			Limit:         limit,
		})
		w.render(rw, "items", map[string]any{
			"Items": items, "Total": total, "Shown": len(items),
			"More": total > len(items), "Step": pageStep, "Ruled": w.rules.Has(),
		})
	}
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
		Target: target, Batch: batch, Enabled: r.PostForm.Get("enabled") != "",
	}
	if err := w.rules.Put(rule); err != nil {
		w.panel(rw, it, map[string]any{"Error": err.Error()})
		return
	}
	w.panel(rw, it, map[string]any{"Done": "Уровень сохранён"})
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
		w.render(rw, "craft", map[string]any{"Done": "Уровень удалён"})
		return
	}
	w.panel(rw, it, map[string]any{"Done": "Уровень удалён"})
}

func (w *Web) toggleRule(rw http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	if err := w.rules.SetEnabled(r.PostForm.Get("key"), r.PostForm.Get("on") == "1"); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	w.render(rw, "rules", map[string]any{"Rules": w.rules.Views(w.store)})
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
	w.panel(rw, it, map[string]any{
		"Amount": amount,
		"Done":   fmt.Sprintf("Заказ #%d: %s × %s отправлен", job.ID, it.Label, group(amount)),
	})
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
