package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Flow is how much of an item came into and left the ME network.
type Flow struct {
	In  int64
	Out int64
	// Fix is what a full sync found that the change events missed:
	// the listed stock minus the stock tracked from the events.
	Fix int64
}

// Net is the change of the stock.
func (f Flow) Net() int64 { return f.In - f.Out + f.Fix }

func (f *Flow) add(g Flow) {
	f.In += g.In
	f.Out += g.Out
	f.Fix += g.Fix
}

// Stats keeps the flows of every item per minute on disk and sums them up
// for a period.
//
// Files in dir:
//   - keys.txt: item keys, one quoted key per line; the line number is the
//     id used in the data files;
//   - YYYY-MM-DD.min: the minutes of a UTC day, appended once a minute;
//   - YYYY-MM-DD.hour: the same day summed per hour, made from the .min
//     file once it is older than statsMinuteFiles.
//
// A data file is a sequence of blocks: uvarint time (unix minutes), uvarint
// count, then count entries of uvarint id, varint in, varint out, varint
// fix. In .hour files the time is the first minute of the hour. A block cut
// short by a crash ends the file.
type Stats struct {
	mu   sync.Mutex
	dir  string
	keys []string
	ids  map[string]uint32
	kf   *os.File

	curMin  int64 // unix minute the cur bucket belongs to
	cur     map[uint32]*Flow
	minutes map[int64]map[uint32]Flow // last statsMinuteKeep, per minute
	hours   map[int64]map[uint32]Flow // last statsHourKeep, per unix hour
	// tens holds the last statsTensKeep in 10-second buckets, in memory
	// only, for the shortest chart.
	tens map[int64]map[uint32]Flow

	now func() time.Time
}

const (
	statsMinuteKeep  = 24 * time.Hour      // minute detail kept in memory
	statsHourKeep    = 31 * 24 * time.Hour // history loaded at start
	statsMinuteFiles = 7 * 24 * time.Hour  // .min files older than this become .hour
	statsTensKeep    = time.Hour           // 10-second buckets kept in memory
)

// OpenStats loads the history from dir, creating it if missing.
func OpenStats(dir string) (*Stats, error) {
	return openStats(dir, time.Now)
}

func openStats(dir string, now func() time.Time) (*Stats, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Stats{
		dir: dir, ids: map[string]uint32{}, cur: map[uint32]*Flow{},
		minutes: map[int64]map[uint32]Flow{}, hours: map[int64]map[uint32]Flow{},
		tens: map[int64]map[uint32]Flow{}, now: now,
	}
	if err := s.loadKeys(); err != nil {
		return nil, err
	}
	kf, err := os.OpenFile(filepath.Join(dir, "keys.txt"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	s.kf = kf
	if err := s.compact(); err != nil {
		log.Printf("stats: compact: %v", err)
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	s.curMin = unixMinute(now())
	return s, nil
}

func unixMinute(t time.Time) int64 { return t.Unix() / 60 }

func (s *Stats) loadKeys() error {
	f, err := os.Open(filepath.Join(s.dir, "keys.txt"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		k, err := strconv.Unquote(sc.Text())
		if err != nil {
			break // a line cut short by a crash
		}
		s.ids[k] = uint32(len(s.keys))
		s.keys = append(s.keys, k)
	}
	return sc.Err()
}

// id returns the id of key, adding it to keys.txt if new. Caller holds mu.
func (s *Stats) id(key string) (uint32, error) {
	if id, ok := s.ids[key]; ok {
		return id, nil
	}
	if _, err := s.kf.WriteString(strconv.Quote(key) + "\n"); err != nil {
		return 0, err
	}
	id := uint32(len(s.keys))
	s.ids[key] = id
	s.keys = append(s.keys, key)
	return id, nil
}

// Record adds flows of the current minute.
func (s *Stats) Record(flows map[string]Flow) {
	if len(flows) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roll()
	ten := s.now().Unix() / 10
	tb := s.tens[ten]
	if tb == nil {
		tb = map[uint32]Flow{}
		s.tens[ten] = tb
	}
	for k, f := range flows {
		id, err := s.id(k)
		if err != nil {
			log.Printf("stats: %v", err)
			return
		}
		c := s.cur[id]
		if c == nil {
			c = &Flow{}
			s.cur[id] = c
		}
		c.add(f)
		g := tb[id]
		g.add(f)
		tb[id] = g
	}
}

// roll writes the current bucket out once its minute is over. Caller
// holds mu.
func (s *Stats) roll() {
	m := unixMinute(s.now())
	if m == s.curMin {
		return
	}
	if len(s.cur) > 0 {
		if err := s.write(s.curMin, s.cur); err != nil {
			log.Printf("stats: %v", err)
		}
		mins := make(map[uint32]Flow, len(s.cur))
		for id, f := range s.cur {
			mins[id] = *f
		}
		s.keep(s.curMin, mins, true)
		s.cur = map[uint32]*Flow{}
	}
	s.curMin = m
	s.prune()
}

// keep adds a bucket to the in-memory history. Caller holds mu.
func (s *Stats) keep(minute int64, flows map[uint32]Flow, detail bool) {
	if detail {
		s.minutes[minute] = flows
	}
	h := s.hours[minute/60]
	if h == nil {
		h = map[uint32]Flow{}
		s.hours[minute/60] = h
	}
	for id, f := range flows {
		g := h[id]
		g.add(f)
		h[id] = g
	}
}

// prune drops what is older than the in-memory windows. Caller holds mu.
func (s *Stats) prune() {
	now := s.now()
	minMin := unixMinute(now.Add(-statsMinuteKeep))
	for m := range s.minutes {
		if m < minMin {
			delete(s.minutes, m)
		}
	}
	minTen := now.Add(-statsTensKeep).Unix() / 10
	for t := range s.tens {
		if t < minTen {
			delete(s.tens, t)
		}
	}
	minHour := unixMinute(now.Add(-statsHourKeep)) / 60
	for h := range s.hours {
		if h < minHour {
			delete(s.hours, h)
		}
	}
}

func dayFile(minute int64, ext string) string {
	return time.Unix(minute*60, 0).UTC().Format("2006-01-02") + ext
}

func encodeBlock(buf *bytes.Buffer, minute int64, flows map[uint32]Flow) {
	var tmp [binary.MaxVarintLen64]byte
	u := func(v uint64) { buf.Write(tmp[:binary.PutUvarint(tmp[:], v)]) }
	i := func(v int64) { buf.Write(tmp[:binary.PutVarint(tmp[:], v)]) }
	u(uint64(minute))
	u(uint64(len(flows)))
	ids := make([]uint32, 0, len(flows))
	for id := range flows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	for _, id := range ids {
		f := flows[id]
		u(uint64(id))
		i(f.In)
		i(f.Out)
		i(f.Fix)
	}
}

// write appends one minute to its day file. Caller holds mu.
func (s *Stats) write(minute int64, cur map[uint32]*Flow) error {
	flows := make(map[uint32]Flow, len(cur))
	for id, f := range cur {
		flows[id] = *f
	}
	var buf bytes.Buffer
	encodeBlock(&buf, minute, flows)
	f, err := os.OpenFile(filepath.Join(s.dir, dayFile(minute, ".min")), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// readBlocks calls fn for every whole block of a data file.
func readBlocks(path string, fn func(minute int64, flows map[uint32]Flow)) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	r := bytes.NewReader(data)
	for r.Len() > 0 {
		minute, err := binary.ReadUvarint(r)
		if err != nil {
			return nil
		}
		n, err := binary.ReadUvarint(r)
		if err != nil {
			return nil
		}
		flows := make(map[uint32]Flow, n)
		for range n {
			id, e1 := binary.ReadUvarint(r)
			in, e2 := binary.ReadVarint(r)
			out, e3 := binary.ReadVarint(r)
			fix, e4 := binary.ReadVarint(r)
			if err := errors.Join(e1, e2, e3, e4); err != nil {
				return nil // cut short: drop the partial block
			}
			flows[uint32(id)] = Flow{In: in, Out: out, Fix: fix}
		}
		fn(int64(minute), flows)
	}
	return nil
}

// dataFiles lists the data files with their UTC day.
func (s *Stats) dataFiles() ([]string, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if !e.IsDir() && (strings.HasSuffix(n, ".min") || strings.HasSuffix(n, ".hour")) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

func fileDay(name string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", strings.TrimSuffix(strings.TrimSuffix(name, ".min"), ".hour"))
	return t, err == nil
}

func (s *Stats) load() error {
	files, err := s.dataFiles()
	if err != nil {
		return err
	}
	now := s.now()
	oldest := now.Add(-statsHourKeep).UTC().Truncate(24 * time.Hour)
	minMin := unixMinute(now.Add(-statsMinuteKeep))
	for _, n := range files {
		day, ok := fileDay(n)
		if !ok || day.Before(oldest) {
			continue
		}
		detail := strings.HasSuffix(n, ".min")
		err := readBlocks(filepath.Join(s.dir, n), func(minute int64, flows map[uint32]Flow) {
			s.keep(minute, flows, detail && minute >= minMin)
		})
		if err != nil {
			return fmt.Errorf("stats: %s: %w", n, err)
		}
	}
	s.prune()
	return nil
}

// compact turns .min files older than statsMinuteFiles into .hour files.
func (s *Stats) compact() error {
	files, err := s.dataFiles()
	if err != nil {
		return err
	}
	limit := s.now().Add(-statsMinuteFiles).UTC().Truncate(24 * time.Hour)
	for _, n := range files {
		day, ok := fileDay(n)
		if !ok || !strings.HasSuffix(n, ".min") || !day.Before(limit) {
			continue
		}
		src := filepath.Join(s.dir, n)
		dst := filepath.Join(s.dir, strings.TrimSuffix(n, ".min")+".hour")
		// A run cut short after the rename left both files; the .min file
		// of an old day gets no more minutes, so the .hour file is whole.
		if _, err := os.Stat(dst); err == nil {
			if err := os.Remove(src); err != nil {
				return err
			}
			continue
		}
		hours := map[int64]map[uint32]Flow{}
		err := readBlocks(src, func(minute int64, flows map[uint32]Flow) {
			h := hours[minute/60]
			if h == nil {
				h = map[uint32]Flow{}
				hours[minute/60] = h
			}
			for id, f := range flows {
				g := h[id]
				g.add(f)
				h[id] = g
			}
		})
		if err != nil {
			return err
		}
		keys := make([]int64, 0, len(hours))
		for h := range hours {
			keys = append(keys, h)
		}
		sort.Slice(keys, func(a, b int) bool { return keys[a] < keys[b] })
		var buf bytes.Buffer
		for _, h := range keys {
			encodeBlock(&buf, h*60, hours[h])
		}
		tmp := dst + ".tmp"
		if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, dst); err != nil {
			return err
		}
		if err := os.Remove(src); err != nil {
			return err
		}
	}
	return nil
}

// Run writes finished minutes out even when nothing changes and compacts
// old files once a day.
func (s *Stats) Run() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	lastCompact := s.now()
	for range t.C {
		s.mu.Lock()
		s.roll()
		if s.now().Sub(lastCompact) >= 24*time.Hour {
			lastCompact = s.now()
			if err := s.compact(); err != nil {
				log.Printf("stats: compact: %v", err)
			}
		}
		s.mu.Unlock()
	}
}

// Since sums the flows of every item from the given time up to now. Within
// statsMinuteKeep it is exact to the minute, further back to the hour.
func (s *Stats) Since(from time.Time) map[string]Flow {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roll()
	sum := map[uint32]Flow{}
	addAll := func(flows map[uint32]Flow) {
		for id, f := range flows {
			g := sum[id]
			g.add(f)
			sum[id] = g
		}
	}
	fm := unixMinute(from)
	if s.now().Sub(from) <= statsMinuteKeep {
		for m, flows := range s.minutes {
			if m >= fm {
				addAll(flows)
			}
		}
	} else {
		for h, flows := range s.hours {
			if h >= fm/60 {
				addAll(flows)
			}
		}
	}
	for id, f := range s.cur {
		g := sum[id]
		g.add(*f)
		sum[id] = g
	}
	out := make(map[string]Flow, len(sum))
	for id, f := range sum {
		if int(id) < len(s.keys) {
			out[s.keys[id]] = f
		}
	}
	return out
}

// Close writes the current minute out.
func (s *Stats) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cur) > 0 {
		if err := s.write(s.curMin, s.cur); err != nil {
			return err
		}
		s.cur = map[uint32]*Flow{}
	}
	return s.kf.Close()
}

// Series sums the flows of the given keys into n buckets of step that end
// at to: bucket i covers [to-(n-i)*step, to-(n-i-1)*step). Steps under a
// minute come from the 10-second buckets of the last hour, steps under an
// hour from the minutes of the last day, longer ones from the hours.
// Buckets outside the kept history stay zero.
func (s *Stats) Series(keys []string, to time.Time, step time.Duration, n int) map[string][]Flow {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roll()
	out := make(map[string][]Flow, len(keys))
	want := map[uint32][]Flow{}
	for _, k := range keys {
		ser := make([]Flow, n)
		out[k] = ser
		if id, ok := s.ids[k]; ok {
			want[id] = ser
		}
	}
	if len(want) == 0 || n <= 0 || step <= 0 {
		return out
	}
	from := to.Add(-time.Duration(n) * step)
	put := func(start time.Time, flows map[uint32]Flow) {
		if start.Before(from) || !start.Before(to) {
			return
		}
		i := int(start.Sub(from) / step)
		for id, ser := range want {
			if f, ok := flows[id]; ok {
				ser[i].add(f)
			}
		}
	}
	cur := make(map[uint32]Flow, len(s.cur))
	for id, f := range s.cur {
		cur[id] = *f
	}
	switch {
	case step < time.Minute:
		for t, flows := range s.tens {
			put(time.Unix(t*10, 0), flows)
		}
	case step < time.Hour:
		for m, flows := range s.minutes {
			put(time.Unix(m*60, 0), flows)
		}
		put(time.Unix(s.curMin*60, 0), cur)
	default:
		for h, flows := range s.hours {
			put(time.Unix(h*3600, 0), flows)
		}
		// the minute in progress is not in the hours yet
		put(time.Unix(s.curMin*60, 0), cur)
	}
	return out
}
