package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestStatsSumsAndReloads(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Date(2026, 11, 7, 12, 0, 10, 0, time.UTC)}
	s, err := openStats(dir, c.now)
	if err != nil {
		t.Fatal(err)
	}
	odd := "x:y|0|Две\nстроки"
	s.Record(map[string]Flow{"a|0|A": {In: 10, Out: 3}, odd: {In: 1}})
	c.t = c.t.Add(time.Minute)
	s.Record(map[string]Flow{"a|0|A": {In: 5, Fix: -2}})
	c.t = c.t.Add(time.Minute)
	s.Record(map[string]Flow{"b|0|B": {Out: 7}})

	all := s.Since(c.t.Add(-time.Hour))
	if a := all["a|0|A"]; a.In != 15 || a.Out != 3 || a.Fix != -2 || a.Net() != 10 {
		t.Fatalf("a = %+v", a)
	}
	if all[odd].In != 1 || all["b|0|B"].Out != 7 {
		t.Fatalf("sums %+v", all)
	}
	// only the last minute and the one in progress
	if a := s.Since(c.t.Add(-time.Minute))["a|0|A"]; a.In != 5 {
		t.Fatalf("last minute a = %+v", a)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// a crash cut the last block short: the whole ones still load
	day := filepath.Join(dir, "2026-11-07.min")
	f, _ := os.OpenFile(day, os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte{0xff, 0xff})
	f.Close()

	s2, err := openStats(dir, c.now)
	if err != nil {
		t.Fatal(err)
	}
	re := s2.Since(c.t.Add(-time.Hour))
	if re["a|0|A"] != all["a|0|A"] || re[odd].In != 1 || re["b|0|B"].Out != 7 {
		t.Fatalf("reloaded %+v, want %+v", re, all)
	}
	// a week back is summed per hour
	if w := s2.Since(c.t.Add(-7 * 24 * time.Hour))["a|0|A"]; w.Net() != 10 {
		t.Fatalf("week a = %+v", w)
	}
	s2.Close()
}

func TestStatsCompactsOldDays(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Date(2026, 11, 1, 10, 5, 0, 0, time.UTC)}
	s, _ := openStats(dir, c.now)
	s.Record(map[string]Flow{"a|0|A": {In: 4}})
	c.t = c.t.Add(30 * time.Minute)
	s.Record(map[string]Flow{"a|0|A": {In: 6}})
	s.Close()

	c.t = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	s2, err := openStats(dir, c.now)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := os.Stat(filepath.Join(dir, "2026-11-01.min")); !os.IsNotExist(err) {
		t.Fatal("old minute file kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-11-01.hour")); err != nil {
		t.Fatal(err)
	}
	if a := s2.Since(c.t.Add(-30 * 24 * time.Hour))["a|0|A"]; a.In != 10 {
		t.Fatalf("compacted a = %+v", a)
	}
}

func TestFullSyncDiff(t *testing.T) {
	s := NewStore()
	s.BeginItems()
	s.AddItems([]Item{{Name: "a", Label: "A", Size: 10}, {Name: "b", Label: "B", Size: 5}})
	if d := s.EndItems(); d != nil {
		t.Fatalf("first list diff %v", d)
	}
	s.ApplyItemDelta([]Item{{Name: "a", Label: "A", Size: -4}})
	s.BeginItems()
	s.AddItems([]Item{{Name: "a", Label: "A", Size: 7}, {Name: "c", Label: "C", Size: 2}})
	d := s.EndItems()
	if len(d) != 3 || d["a|0|A"] != 1 || d["b|0|B"] != -5 || d["c|0|C"] != 2 {
		t.Fatalf("diff %v", d)
	}
	if fd := s.SetFluids([]Fluid{{Name: "w", Amount: 100}}); fd != nil {
		t.Fatalf("first fluids diff %v", fd)
	}
	s.ApplyFluidDelta([]Fluid{{Name: "w", Amount: 50}})
	if fd := s.SetFluids([]Fluid{{Name: "w", Amount: 140}}); fd["fluid|w"] != -10 {
		t.Fatalf("fluids diff %v", fd)
	}
}

func TestAddFlow(t *testing.T) {
	f := map[string]Flow{}
	addFlow(f, "k", -7, 10) // came 3, left 10
	addFlow(f, "k", -2, 0)  // old script: net only
	if g := f["k"]; g.In != 3 || g.Out != 12 {
		t.Fatalf("%+v", g)
	}
}
