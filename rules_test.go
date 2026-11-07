package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeCrafter struct {
	store  *Store
	orders []int64 // job ids
	err    error
}

func (f *fakeCrafter) Craft(it Item, amount int64, auto bool) (*Job, error) {
	if f.err != nil {
		return nil, f.err
	}
	j := f.store.NewJob(it, amount, auto)
	f.orders = append(f.orders, j.ID)
	return j, nil
}

func TestRetryDelay(t *testing.T) {
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	for i, w := range want {
		if got := retryDelay(i + 1); got != w {
			t.Errorf("fails %d: got %s, want %s", i+1, got, w)
		}
	}
}

func TestMaintainerReordersAndBacksOff(t *testing.T) {
	store := NewStore()
	store.SetConnected(true, "test")
	plate := Item{Name: "gt:plate", Damage: 7, Label: "Пластина", Size: 0, Craftable: true}
	store.BeginItems()
	store.AddItems([]Item{plate})
	store.EndItems()

	rules := NewRules(filepath.Join(t.TempDir(), "mecraft.json"), defaultConfig())
	if err := rules.Put(Rule{Key: plate.Key(), Name: plate.Name, Damage: plate.Damage, Label: plate.Label, Target: 100, Batch: 32, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	fc := &fakeCrafter{store: store}
	m := NewMaintainer(store, fc, rules, 5*time.Second)
	now := time.Now()
	note := func() string { return rules.Levels(store)[plate.Key()].Note }
	last := func() int64 { return fc.orders[len(fc.orders)-1] }
	expectOrders := func(n int, when string) {
		t.Helper()
		if len(fc.orders) != n {
			t.Fatalf("%s: %d orders, want %d (note %q)", when, len(fc.orders), n, note())
		}
	}

	m.Tick(now)
	expectOrders(1, "first tick")

	// done while still short: the next batch goes in the same tick
	store.ApplyItemDelta([]Item{{Name: plate.Name, Damage: plate.Damage, Label: plate.Label, Size: 32}})
	store.UpdateJob(last(), "done", "")
	m.Tick(now)
	expectOrders(2, "after done")

	// done before the crafted items are counted: wait for them a moment
	store.UpdateJob(last(), "done", "")
	m.Tick(now)
	expectOrders(2, "done without new stock")
	now = now.Add(arrivalWait)
	m.Tick(now)
	expectOrders(3, "after waiting for the stock")

	// failures back off 5s, 10s, ... and never reorder early
	delays := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute}
	for i, d := range delays {
		store.UpdateJob(last(), "failed", "missing resources")
		m.Tick(now)
		if !strings.Contains(note(), "повтор через "+waitText(d)) {
			t.Fatalf("failure %d: note %q, want delay %s", i+1, note(), waitText(d))
		}
		m.Tick(now.Add(d - time.Millisecond))
		expectOrders(3+i, "before the delay")
		now = now.Add(d)
		m.Tick(now)
		expectOrders(4+i, "after the delay")
	}

	// a success resets the delay back to 5s
	store.ApplyItemDelta([]Item{{Name: plate.Name, Damage: plate.Damage, Label: plate.Label, Size: 32}})
	store.UpdateJob(last(), "done", "")
	m.Tick(now)
	n := len(fc.orders)
	store.UpdateJob(last(), "failed", "missing resources")
	m.Tick(now)
	if !strings.Contains(note(), "повтор через 5 с") {
		t.Fatalf("after success: note %q, want 5 с", note())
	}
	now = now.Add(5 * time.Second)
	m.Tick(now)
	expectOrders(n+1, "retry after reset")

	// level reached: nothing more is ordered
	store.ApplyItemDelta([]Item{{Name: plate.Name, Damage: plate.Damage, Label: plate.Label, Size: 100}})
	store.UpdateJob(last(), "done", "")
	m.Tick(now)
	expectOrders(n+1, "level reached")
	if note() != "в норме" {
		t.Fatalf("note %q, want в норме", note())
	}
}

func TestMaintainerBacksOffWhenSendFails(t *testing.T) {
	store := NewStore()
	store.SetConnected(true, "test")
	plate := Item{Name: "gt:plate", Damage: 7, Label: "Пластина", Craftable: true}
	store.BeginItems()
	store.AddItems([]Item{plate})
	store.EndItems()
	rules := NewRules(filepath.Join(t.TempDir(), "mecraft.json"), defaultConfig())
	rules.Put(Rule{Key: plate.Key(), Name: plate.Name, Damage: plate.Damage, Label: plate.Label, Target: 10, Enabled: true})
	fc := &fakeCrafter{store: store, err: errors.New("OC не подключён")}
	m := NewMaintainer(store, fc, rules, 5*time.Second)
	now := time.Now()
	m.Tick(now)
	m.Tick(now.Add(5 * time.Second))
	if n := rules.Levels(store)[plate.Key()].Note; !strings.Contains(n, "повтор через 10 с") {
		t.Fatalf("note %q, want 10 с after two send failures", n)
	}
}

func TestJobEventsWakeTheMaintainer(t *testing.T) {
	store := NewStore()
	j := store.NewJob(Item{Name: "a", Label: "A"}, 1, true)
	store.UpdateJob(j.ID, "done", "")
	store.UpdateJob(j.ID, "done", "") // does not block when nobody listens
	select {
	case <-store.JobEvents():
	default:
		t.Fatal("no job event")
	}
}

func TestMaintainerDrainsResource(t *testing.T) {
	store := NewStore()
	store.SetConnected(true, "test")
	sodium := Item{Name: "gt:dust", Damage: 17, Label: "Натрий", Craftable: true}
	naoh := Item{Name: "gt:dust", Damage: 685, Label: "Гидроксид натрия", Size: 10_100}
	store.BeginItems()
	store.AddItems([]Item{sodium, naoh})
	store.EndItems()

	rules := NewRules(filepath.Join(t.TempDir(), "mecraft.json"), defaultConfig())
	rule := Rule{Key: sodium.Key(), Name: sodium.Name, Damage: sodium.Damage, Label: sodium.Label,
		Mode: ModeDrain, Source: naoh.Key(), SourceLabel: naoh.Label, Target: 10_000, Batch: 64, Enabled: true}
	if err := rules.Put(rule); err != nil {
		t.Fatal(err)
	}
	fc := &fakeCrafter{store: store}
	m := NewMaintainer(store, fc, rules, 5*time.Second)
	now := time.Now()

	m.Tick(now)
	if len(fc.orders) != 1 {
		t.Fatalf("%d orders, want 1 while the resource is above the threshold", len(fc.orders))
	}
	if j, _ := store.Job(fc.orders[0]); j.Amount != 64 || j.Name != sodium.Name {
		t.Fatalf("ordered %+v, want 64 of the product", j)
	}

	// the craft used the resource and made the product; still above 10k
	store.ApplyItemDelta([]Item{{Name: naoh.Name, Damage: naoh.Damage, Label: naoh.Label, Size: -64}, {Name: sodium.Name, Damage: sodium.Damage, Label: sodium.Label, Size: 64}})
	store.UpdateJob(fc.orders[0], "done", "")
	m.Tick(now)
	if len(fc.orders) != 2 {
		t.Fatalf("%d orders, want the next batch at once", len(fc.orders))
	}

	// down to the threshold: stop
	store.ApplyItemDelta([]Item{{Name: naoh.Name, Damage: naoh.Damage, Label: naoh.Label, Size: -36}, {Name: sodium.Name, Damage: sodium.Damage, Label: sodium.Label, Size: 64}})
	store.UpdateJob(fc.orders[1], "done", "")
	m.Tick(now)
	if len(fc.orders) != 2 {
		t.Fatalf("%d orders, want none at the threshold", len(fc.orders))
	}
	lv := rules.Levels(store)[sodium.Key()]
	if lv.Have != 10_000 || lv.Stock != 128 || lv.Note != "ресурс переработан до порога" {
		t.Fatalf("view %+v", lv)
	}
}

func TestDrainRuleValidation(t *testing.T) {
	rules := NewRules(filepath.Join(t.TempDir(), "mecraft.json"), defaultConfig())
	base := Rule{Key: "a|0|A", Name: "a", Label: "A", Mode: ModeDrain, Source: "b|0|B", SourceLabel: "B", Target: 0, Batch: 1}
	if err := rules.Put(base); err != nil {
		t.Fatalf("threshold 0 should be fine: %v", err)
	}
	for name, r := range map[string]Rule{
		"no source": func() Rule { r := base; r.Source = ""; return r }(),
		"self":      func() Rule { r := base; r.Source = r.Key; return r }(),
		"no batch":  func() Rule { r := base; r.Batch = 0; return r }(),
		"negative":  func() Rule { r := base; r.Target = -1; return r }(),
		"bad mode":  func() Rule { r := base; r.Mode = "x"; return r }(),
		"bad cap":   func() Rule { r := base; r.Cap = -1; return r }(),
	} {
		if err := rules.Put(r); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	// switching back to the level mode drops the resource
	lvl := base
	lvl.Mode, lvl.Target = "", 5
	if err := rules.Put(lvl); err != nil {
		t.Fatal(err)
	}
	if r, _ := rules.Get(base.Key); r.Source != "" || r.SourceLabel != "" {
		t.Fatalf("level rule kept the resource: %+v", r)
	}
}

func TestMaintainerDrainStopsAtItemCap(t *testing.T) {
	store := NewStore()
	store.SetConnected(true, "test")
	sodium := Item{Name: "gt:dust", Damage: 17, Label: "Натрий", Size: 100, Craftable: true}
	naoh := Item{Name: "gt:dust", Damage: 685, Label: "Гидроксид натрия", Size: 50_000}
	store.BeginItems()
	store.AddItems([]Item{sodium, naoh})
	store.EndItems()
	rules := NewRules(filepath.Join(t.TempDir(), "mecraft.json"), defaultConfig())
	rule := Rule{Key: sodium.Key(), Name: sodium.Name, Damage: sodium.Damage, Label: sodium.Label,
		Mode: ModeDrain, Source: naoh.Key(), SourceLabel: naoh.Label, Target: 10_000, Batch: 64, Cap: 164, Enabled: true}
	if err := rules.Put(rule); err != nil {
		t.Fatal(err)
	}
	fc := &fakeCrafter{store: store}
	m := NewMaintainer(store, fc, rules, 5*time.Second)
	now := time.Now()
	m.Tick(now)
	if len(fc.orders) != 1 {
		t.Fatalf("%d orders, want 1 below the cap", len(fc.orders))
	}
	store.ApplyItemDelta([]Item{{Name: sodium.Name, Damage: sodium.Damage, Label: sodium.Label, Size: 64}})
	store.UpdateJob(fc.orders[0], "done", "")
	m.Tick(now)
	if len(fc.orders) != 1 {
		t.Fatalf("%d orders, want none at the cap though the resource is plenty", len(fc.orders))
	}
	lv := rules.Levels(store)[sodium.Key()]
	if lv.Note != "предмета уже не меньше лимита" || lv.Busy() {
		t.Fatalf("view %+v busy %v", lv, lv.Busy())
	}

	// cap 0 ignores the item stock
	rule.Cap = 0
	if err := rules.Put(rule); err != nil {
		t.Fatal(err)
	}
	m.Tick(now)
	if len(fc.orders) != 2 {
		t.Fatalf("%d orders, want one more without a cap", len(fc.orders))
	}
}

func TestMaintainerWaitsForCraftInCPU(t *testing.T) {
	store := NewStore()
	store.SetConnected(true, "test")
	plat := Item{Name: "gt:ingot", Damage: 85, Label: "Платиновый слиток", Craftable: true}
	store.BeginItems()
	store.AddItems([]Item{plat})
	store.EndItems()
	rules := NewRules(filepath.Join(t.TempDir(), "mecraft.json"), defaultConfig())
	if err := rules.Put(Rule{Key: plat.Key(), Name: plat.Name, Damage: plat.Damage, Label: plat.Label, Target: 1000, Batch: 64, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	fc := &fakeCrafter{store: store}
	m := NewMaintainer(store, fc, rules, 5*time.Second)
	note := func() string { return rules.Levels(store)[plat.Key()].Note }
	busy := []CPU{{Name: "Большой", Busy: true, Output: plat.Key(), OutputLabel: plat.Label}}

	// after a server restart the rule knows no order, but ME still crafts one
	store.SetCPUs(busy)
	now := time.Now().Add(time.Second)
	m.Tick(now)
	if len(fc.orders) != 0 || !strings.Contains(note(), "уже крафтится в ME (Большой)") {
		t.Fatalf("%d orders, note %q; want to wait for the CPU", len(fc.orders), note())
	}
	store.SetCPUs([]CPU{{Name: "Большой"}})
	m.Tick(now)
	if len(fc.orders) != 1 {
		t.Fatalf("%d orders, want one once the CPU is free", len(fc.orders))
	}

	// a CPU report older than the finished order does not hold the next one
	store.SetCPUs(busy)
	now = now.Add(time.Second)
	store.ApplyItemDelta([]Item{{Name: plat.Name, Damage: plat.Damage, Label: plat.Label, Size: 64}})
	store.UpdateJob(fc.orders[0], "done", "")
	m.Tick(now)
	if len(fc.orders) != 2 {
		t.Fatalf("%d orders, want the next one at once (note %q)", len(fc.orders), note())
	}
}

func TestCraftingInFindsItemsInProgress(t *testing.T) {
	store := NewStore()
	store.SetCPUs([]CPU{{Busy: true, Parts: []string{"a|0|A", "b|0|B"}, PartLabels: []string{"A", "B"}}})
	if cpu, _, ok := store.CraftingIn("b|0|B"); !ok || cpu != "процессор 0" {
		t.Fatalf("got %q %v, want the item in progress to count", cpu, ok)
	}
	if _, _, ok := store.CraftingIn("c|0|C"); ok {
		t.Fatal("an item not in progress counts")
	}
	if d := store.CPUs()[0].Doing(); d != "в работе: A, B" {
		t.Fatalf("doing %q", d)
	}
}
