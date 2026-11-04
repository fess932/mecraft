package main

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Item is one stack type stored in (or craftable by) the ME network.
type Item struct {
	Name      string
	Damage    int
	Label     string
	Size      int64
	HasTag    bool
	Craftable bool
	Fluid     bool // a fluid from the ME network; Size is in litres
}

const fluidKeyPrefix = "fluid|"

// Key identifies an item type. The label separates NBT variants that share
// name and damage, because OC does not send the NBT itself. Fluids are
// identified by name alone.
func (it Item) Key() string {
	if it.Fluid {
		return fluidKeyPrefix + it.Name
	}
	return it.Name + "|" + strconv.Itoa(it.Damage) + "|" + it.Label
}

// Unit is how the amount is counted in the UI.
func (it Item) Unit() string {
	if it.Fluid {
		return "л"
	}
	return "шт."
}

type Fluid struct {
	Name      string
	Label     string
	Amount    int64
	Craftable bool
}

// Item lets a fluid go through the same craft window, orders and stock
// levels as items.
func (f Fluid) Item() Item {
	return Item{Name: f.Name, Label: f.Label, Size: f.Amount, Craftable: f.Craftable, Fluid: true}
}

type CPU struct {
	Name         string
	Storage      int64
	Coprocessors int
	Busy         bool
}

type Power struct {
	Stored    float64
	Max       float64
	Usage     float64
	Injection float64
	// Memory of the OC computer, bytes, as computer.freeMemory/totalMemory.
	MemFree  float64
	MemTotal float64
}

type Job struct {
	ID      int64
	Name    string
	Damage  int
	Label   string
	Amount  int64
	State   string
	Reason  string
	Auto    bool
	Fluid   bool
	Created time.Time
	Updated time.Time
}

const maxJobs = 500

// Store keeps the latest picture of the ME network reported by OC.
type Store struct {
	mu        sync.RWMutex
	items     map[string]*Item
	staging   map[string]*Item
	fluids    map[string]*Fluid
	cpus      []CPU
	power     Power
	jobs      []*Job
	nextJob   int64
	connected bool
	client    string
	lastSync  time.Time
	lastSeen  time.Time
	failure   string // why the OC script stopped, until the next full sync
}

func NewStore() *Store {
	return &Store{
		items:  map[string]*Item{},
		fluids: map[string]*Fluid{},
	}
}

func (s *Store) SetConnected(ok bool, client string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected = ok
	s.client = client
	if ok {
		s.lastSeen = time.Now()
	}
}

func (s *Store) Touch() {
	s.mu.Lock()
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

func (s *Store) BeginItems() {
	s.mu.Lock()
	s.staging = map[string]*Item{}
	s.mu.Unlock()
}

func (s *Store) AddItems(items []Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.staging == nil {
		s.staging = map[string]*Item{}
	}
	for _, it := range items {
		it := it
		k := it.Key()
		if cur, ok := s.staging[k]; ok {
			cur.Size += it.Size
			cur.Craftable = cur.Craftable || it.Craftable
			continue
		}
		s.staging[k] = &it
	}
}

// SetFailure records why the OC script stopped.
func (s *Store) SetFailure(msg string) {
	s.mu.Lock()
	s.failure = msg
	s.mu.Unlock()
}

func (s *Store) EndItems() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failure = ""
	if s.staging != nil {
		s.items = s.staging
		s.staging = nil
	}
	s.lastSync = time.Now()
}

// ApplyItemDelta adds signed size changes from network_item_changed events.
func (s *Store) ApplyItemDelta(items []Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range items {
		k := it.Key()
		cur, ok := s.items[k]
		if !ok {
			if it.Size > 0 {
				n := it
				s.items[k] = &n
			}
			continue
		}
		cur.Size += it.Size
		if cur.Size <= 0 {
			cur.Size = 0
			if !cur.Craftable {
				delete(s.items, k)
			}
		}
	}
}

func (s *Store) SetFluids(fluids []Fluid) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fluids = make(map[string]*Fluid, len(fluids))
	for _, f := range fluids {
		f := f
		s.fluids[f.Name] = &f
	}
}

func (s *Store) ApplyFluidDelta(fluids []Fluid) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range fluids {
		cur, ok := s.fluids[f.Name]
		if !ok {
			if f.Amount > 0 {
				n := f
				s.fluids[f.Name] = &n
			}
			continue
		}
		cur.Amount += f.Amount
		if cur.Amount <= 0 {
			cur.Amount = 0
			if !cur.Craftable {
				delete(s.fluids, f.Name)
			}
		}
	}
}

func (s *Store) SetCPUs(cpus []CPU) {
	s.mu.Lock()
	s.cpus = cpus
	s.mu.Unlock()
}

func (s *Store) SetPower(p Power) {
	s.mu.Lock()
	s.power = p
	s.mu.Unlock()
}

func (s *Store) NewJob(it Item, amount int64, auto bool) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextJob++
	now := time.Now()
	j := &Job{
		ID: s.nextJob, Name: it.Name, Damage: it.Damage, Label: it.Label,
		Amount: amount, State: "sent", Auto: auto, Fluid: it.Fluid, Created: now, Updated: now,
	}
	s.jobs = append(s.jobs, j)
	if len(s.jobs) > maxJobs {
		s.jobs = s.jobs[len(s.jobs)-maxJobs:]
	}
	cp := *j
	return &cp
}

func (s *Store) Job(id int64) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := len(s.jobs) - 1; i >= 0; i-- {
		if s.jobs[i].ID == id {
			return *s.jobs[i], true
		}
	}
	return Job{}, false
}

func (s *Store) UpdateJob(id int64, state, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			j.State = state
			j.Reason = reason
			j.Updated = time.Now()
			return
		}
	}
}

// FailOpenJobs marks unfinished jobs as lost when OC disconnects.
func (s *Store) FailOpenJobs(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		switch j.State {
		case "sent", "computing", "running":
			j.State = "lost"
			j.Reason = reason
			j.Updated = time.Now()
		}
	}
}

type ItemQuery struct {
	Text          string
	CraftableOnly bool
	InStockOnly   bool
	SortByName    bool
	Limit         int
}

func matches(text string, fields ...string) bool {
	if text == "" {
		return true
	}
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), text) {
			return true
		}
	}
	return false
}

func (q ItemQuery) keep(it Item, text string) bool {
	if q.CraftableOnly && !it.Craftable {
		return false
	}
	if q.InStockOnly && it.Size <= 0 {
		return false
	}
	return matches(text, it.Label, it.Name)
}

func (s *Store) Items(q ItemQuery) (list []Item, total int) {
	text := strings.ToLower(strings.TrimSpace(q.Text))
	s.mu.RLock()
	for _, it := range s.items {
		if q.keep(*it, text) {
			list = append(list, *it)
		}
	}
	s.mu.RUnlock()
	return q.page(list)
}

// FluidItems is Items for fluids, with the same filters, sort and paging.
func (s *Store) FluidItems(q ItemQuery) (list []Item, total int) {
	text := strings.ToLower(strings.TrimSpace(q.Text))
	s.mu.RLock()
	for _, f := range s.fluids {
		if it := f.Item(); q.keep(it, text) {
			list = append(list, it)
		}
	}
	s.mu.RUnlock()
	return q.page(list)
}

// Filter applies the query to a ready list.
func (q ItemQuery) Filter(in []Item) ([]Item, int) {
	text := strings.ToLower(strings.TrimSpace(q.Text))
	var list []Item
	for _, it := range in {
		if q.keep(it, text) {
			list = append(list, it)
		}
	}
	return q.page(list)
}

// page sorts the filtered list and cuts it to the limit.
func (q ItemQuery) page(list []Item) ([]Item, int) {
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if !q.SortByName && a.Size != b.Size {
			return a.Size > b.Size
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		return a.Key() < b.Key()
	})
	total := len(list)
	if q.Limit > 0 && len(list) > q.Limit {
		list = list[:q.Limit]
	}
	return list, total
}

// FindItem finds an item or, for a fluid key, a fluid.
func (s *Store) FindItem(key string) (Item, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if name, ok := strings.CutPrefix(key, fluidKeyPrefix); ok {
		f, ok := s.fluids[name]
		if !ok {
			return Item{}, false
		}
		return f.Item(), true
	}
	it, ok := s.items[key]
	if !ok {
		return Item{}, false
	}
	return *it, true
}

func (s *Store) CPUs() []CPU {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]CPU(nil), s.cpus...)
}

func (s *Store) Jobs() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Job, 0, len(s.jobs))
	for i := len(s.jobs) - 1; i >= 0; i-- {
		out = append(out, *s.jobs[i])
	}
	return out
}

type Status struct {
	Connected bool
	Client    string
	Items     int
	Fluids    int
	LastSync  time.Time
	LastSeen  time.Time
	Power     Power
	Failure   string
}

func (s *Store) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Status{
		Connected: s.connected,
		Client:    s.client,
		Items:     len(s.items),
		Fluids:    len(s.fluids),
		LastSync:  s.lastSync,
		LastSeen:  s.lastSeen,
		Power:     s.power,
		Failure:   s.failure,
	}
}
