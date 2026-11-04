package main

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// Rule keeps an item stocked: when the network holds less than Target,
// a crafting order for Batch items is placed (or for the shortfall when
// Batch is 0).
type Rule struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Damage  int    `json:"damage"`
	Label   string `json:"label"`
	Target  int64  `json:"target"`
	Batch   int64  `json:"batch"`
	Enabled bool   `json:"enabled"`
	Fluid   bool   `json:"fluid,omitempty"`
}

func (r Rule) Item() Item {
	return Item{Name: r.Name, Damage: r.Damage, Label: r.Label, Craftable: true, Fluid: r.Fluid}
}

// ruleState is runtime only: the order in flight and when to try again.
type ruleState struct {
	JobID   int64
	NextTry time.Time
	Note    string
	Ordered time.Time
}

// RuleView is a rule with its live state, for the UI.
type RuleView struct {
	Rule
	Have      int64
	InNet     bool
	Craftable bool
	Note      string
	JobID     int64
	Ordered   time.Time
}

// Rules holds the stock levels and writes them back into the config file
// together with the other settings.
type Rules struct {
	mu    sync.Mutex
	path  string
	cfg   Config // settings other than Rules, kept to rewrite the file
	rules map[string]*Rule
	state map[string]*ruleState
}

func NewRules(path string, cfg Config) *Rules {
	rs := &Rules{path: path, cfg: cfg, rules: map[string]*Rule{}, state: map[string]*ruleState{}}
	for _, r := range cfg.Rules {
		r := r
		rs.rules[r.Key] = &r
	}
	rs.cfg.Rules = nil
	return rs
}

// save writes the config with the current rules. Caller holds mu.
func (rs *Rules) save() error {
	list := make([]Rule, 0, len(rs.rules))
	for _, r := range rs.rules {
		list = append(list, *r)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Label != list[j].Label {
			return list[i].Label < list[j].Label
		}
		return list[i].Key < list[j].Key
	})
	cfg := rs.cfg
	cfg.Rules = list
	return writeConfig(rs.path, cfg)
}

func (rs *Rules) Get(key string) (Rule, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	r, ok := rs.rules[key]
	if !ok {
		return Rule{}, false
	}
	return *r, true
}

// Levels returns the live state of every rule by key.
func (rs *Rules) Levels(store *Store) map[string]*RuleView {
	views := rs.Views(store)
	out := make(map[string]*RuleView, len(views))
	for i := range views {
		out[views[i].Key] = &views[i]
	}
	return out
}

func (rs *Rules) Put(r Rule) error {
	if r.Target <= 0 {
		return errors.New("уровень должен быть больше нуля")
	}
	if r.Batch < 0 {
		return errors.New("партия не может быть отрицательной")
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	old, existed := rs.rules[r.Key]
	rs.rules[r.Key] = &r
	if err := rs.save(); err != nil {
		if existed {
			rs.rules[r.Key] = old
		} else {
			delete(rs.rules, r.Key)
		}
		return err
	}
	// A changed rule gets a fresh chance right away.
	if st := rs.state[r.Key]; st != nil {
		st.NextTry = time.Time{}
	}
	return nil
}

func (rs *Rules) Delete(key string) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	old, ok := rs.rules[key]
	if !ok {
		return nil
	}
	delete(rs.rules, key)
	if err := rs.save(); err != nil {
		rs.rules[key] = old
		return err
	}
	delete(rs.state, key)
	return nil
}

func (rs *Rules) SetEnabled(key string, on bool) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	r, ok := rs.rules[key]
	if !ok {
		return errors.New("правило не найдено")
	}
	prev := r.Enabled
	r.Enabled = on
	if err := rs.save(); err != nil {
		r.Enabled = prev
		return err
	}
	if st := rs.state[key]; st != nil {
		st.NextTry = time.Time{}
	}
	return nil
}

// Views returns rules sorted by label with live stock and state.
func (rs *Rules) Views(store *Store) []RuleView {
	rs.mu.Lock()
	out := make([]RuleView, 0, len(rs.rules))
	for k, r := range rs.rules {
		v := RuleView{Rule: *r}
		if st := rs.state[k]; st != nil {
			v.Note, v.JobID, v.Ordered = st.Note, st.JobID, st.Ordered
		}
		out = append(out, v)
	}
	rs.mu.Unlock()
	for i := range out {
		if it, ok := store.FindItem(out[i].Key); ok {
			out[i].Have, out[i].InNet, out[i].Craftable = it.Size, true, it.Craftable
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Label != out[j].Label {
			return out[i].Label < out[j].Label
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Maintainer orders crafts so that every enabled rule stays at its level.
type Maintainer struct {
	store    *Store
	hub      *Hub
	rules    *Rules
	interval time.Duration
	settle   time.Duration // wait after a finished order for stock to arrive
	backoff  time.Duration // wait after a failed order
}

func NewMaintainer(store *Store, hub *Hub, rules *Rules, interval time.Duration) *Maintainer {
	return &Maintainer{
		store: store, hub: hub, rules: rules, interval: interval,
		settle: interval, backoff: 12 * interval,
	}
}

func (m *Maintainer) Run() {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for range t.C {
		m.Tick(time.Now())
	}
}

type order struct {
	key    string
	item   Item
	amount int64
}

// Tick checks every rule once and places the orders that are due.
func (m *Maintainer) Tick(now time.Time) {
	st := m.store.Status()
	if !st.Connected || st.LastSync.IsZero() {
		m.noteAll("ждёт подключения OC")
		return
	}

	var orders []order
	m.rules.mu.Lock()
	for key, r := range m.rules.rules {
		s := m.rules.state[key]
		if s == nil {
			s = &ruleState{}
			m.rules.state[key] = s
		}
		if !r.Enabled {
			s.Note = "выключено"
			continue
		}

		// Follow the order in flight, if any.
		if s.JobID != 0 {
			job, ok := m.store.Job(s.JobID)
			switch {
			case ok && (job.State == "sent" || job.State == "computing" || job.State == "running"):
				s.Note = fmt.Sprintf("заказ #%d: %s", job.ID, stateText(job.State))
				continue
			case ok && job.State == "done":
				s.NextTry = now.Add(m.settle)
				s.Note = fmt.Sprintf("заказ #%d готов", job.ID)
			default:
				reason := "заказ пропал"
				if ok {
					reason = stateText(job.State)
					if job.Reason != "" {
						reason += ": " + job.Reason
					}
				}
				s.NextTry = now.Add(m.backoff)
				s.Note = fmt.Sprintf("заказ #%d — %s; повтор через %s", s.JobID, reason, m.backoff.Round(time.Second))
			}
			s.JobID = 0
		}
		if now.Before(s.NextTry) {
			continue
		}

		it, inNet := m.store.FindItem(key)
		have := it.Size
		if have >= r.Target {
			s.Note = "в норме"
			continue
		}
		if !inNet || !it.Craftable {
			s.Note = "нет шаблона крафта в ME"
			continue
		}
		amount := r.Batch
		if amount <= 0 {
			amount = r.Target - have
		}
		orders = append(orders, order{key: key, item: it, amount: amount})
	}
	m.rules.mu.Unlock()

	for _, o := range orders {
		job, err := m.hub.Craft(o.item, o.amount, true)
		m.rules.mu.Lock()
		s := m.rules.state[o.key]
		if s != nil {
			if err != nil {
				s.NextTry = now.Add(m.backoff)
				s.Note = "не отправлен: " + err.Error()
			} else {
				s.JobID = job.ID
				s.Ordered = now
				s.Note = fmt.Sprintf("заказ #%d: отправлен", job.ID)
			}
		}
		m.rules.mu.Unlock()
		if err == nil {
			log.Printf("maintain: %s × %d (job #%d)", o.item.Label, o.amount, job.ID)
		}
	}
}

func (m *Maintainer) noteAll(note string) {
	m.rules.mu.Lock()
	defer m.rules.mu.Unlock()
	for key := range m.rules.rules {
		s := m.rules.state[key]
		if s == nil {
			s = &ruleState{}
			m.rules.state[key] = s
		}
		// An order in flight is kept and resolved after reconnecting.
		s.Note = note
	}
}
