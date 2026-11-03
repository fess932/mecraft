package main

import (
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
	tpl   *template.Template
}

const pageStep = 300

func NewWeb(store *Store, hub *Hub, icons *Icons, templates fs.FS) (*Web, error) {
	w := &Web{store: store, hub: hub, icons: icons}
	funcs := template.FuncMap{
		"icon":  icons.URL,
		"short": short,
		"group": group,
		"ago":   ago,
		"pct":   pct,
		"state": stateText,
		"itemq": itemQuery,
		"num":   func(n int) string { return group(int64(n)) },
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
	mux.Handle("GET /static/", http.StripPrefix("/static/", cached(http.FileServerFS(static))))
	mux.Handle("GET /icons/", http.StripPrefix("/icons/", cached(http.FileServerFS(icons))))
	mux.Handle("GET /oc/", http.StripPrefix("/oc/", http.FileServerFS(oc)))
	return mux
}

func cached(h http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Cache-Control", "public, max-age=86400")
		h.ServeHTTP(rw, r)
	})
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
			"More": total > len(items), "Step": pageStep,
		})
	}
}

func (w *Web) craftForm(rw http.ResponseWriter, r *http.Request) {
	it, ok := w.store.FindItem(r.URL.Query().Get("key"))
	if !ok {
		w.render(rw, "craft", map[string]any{"Error": "предмет пропал из сети, обнови список"})
		return
	}
	w.render(rw, "craft", map[string]any{"Item": it, "Amount": 1})
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
	amount, err := strconv.ParseInt(strings.TrimSpace(r.PostForm.Get("amount")), 10, 64)
	if err != nil || amount <= 0 {
		w.render(rw, "craft", map[string]any{"Item": it, "Amount": 1, "Error": "введи число больше нуля"})
		return
	}
	job, err := w.hub.Craft(it, amount)
	if err != nil {
		w.render(rw, "craft", map[string]any{"Item": it, "Amount": amount, "Error": err.Error()})
		return
	}
	w.render(rw, "craft", map[string]any{
		"Item": it, "Amount": amount,
		"Done": fmt.Sprintf("Заказ #%d: %s × %s отправлен", job.ID, it.Label, group(amount)),
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
