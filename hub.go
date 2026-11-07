package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// Wire format: one Lua table literal per line, both directions, as written
// and read by the OpenOS serialization library. The "type" field selects
// the message. The json tags name the table fields.

type wireItem struct {
	Name      string  `json:"name"`
	Damage    float64 `json:"damage"`
	Label     string  `json:"label"`
	Size      float64 `json:"size"`
	HasTag    bool    `json:"hasTag"`
	Craftable bool    `json:"isCraftable"`
	Out       float64 `json:"out"` // deltas: how much left within the batch
}

func (w wireItem) item() Item {
	label := w.Label
	if label == "" {
		label = w.Name
	}
	return Item{
		Name: w.Name, Damage: int(w.Damage), Label: label,
		Size: int64(w.Size), HasTag: w.HasTag, Craftable: w.Craftable,
	}
}

type wireFluid struct {
	Name      string  `json:"name"`
	Label     string  `json:"label"`
	Amount    float64 `json:"amount"`
	Craftable bool    `json:"isCraftable"`
	Out       float64 `json:"out"` // deltas: how much left within the batch
}

func (w wireFluid) fluid() Fluid {
	label := w.Label
	if label == "" {
		label = w.Name
	}
	return Fluid{Name: w.Name, Label: label, Amount: int64(w.Amount), Craftable: w.Craftable}
}

type wireCPU struct {
	Name         string     `json:"name"`
	Storage      float64    `json:"storage"`
	Coprocessors float64    `json:"coprocessors"`
	Busy         bool       `json:"busy"`
	Output       *wireItem  `json:"output"`
	Parts        []wireItem `json:"parts"`
}

type inMsg struct {
	Type      string      `json:"type"`
	Token     string      `json:"token"`
	Items     []wireItem  `json:"items"`
	Fluids    []wireFluid `json:"fluids"`
	CPUs      []wireCPU   `json:"cpus"`
	Job       int64       `json:"job"`
	State     string      `json:"state"`
	Reason    string      `json:"reason"`
	Message   string      `json:"message"`
	Stored    float64     `json:"stored"`
	Max       float64     `json:"max"`
	Usage     float64     `json:"usage"`
	Injection float64     `json:"injection"`
	MemFree   float64     `json:"memFree"`
	MemTotal  float64     `json:"memTotal"`
}

// Hub accepts the TCP connection from the OC computer and translates
// messages into Store updates and web requests into OC commands.
type Hub struct {
	store *Store
	token string
	// Stats, when set, records every change of the network.
	Stats *Stats

	mu   sync.Mutex
	conn net.Conn
	wmu  sync.Mutex
}

func NewHub(store *Store, token string) *Hub {
	return &Hub{store: store, token: token}
}

func (h *Hub) Serve(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("oc: listening on %s", addr)
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("oc: accept: %v", err)
			time.Sleep(time.Second)
			continue
		}
		go h.handle(c)
	}
}

func (h *Hub) handle(c net.Conn) {
	addr := c.RemoteAddr().String()
	log.Printf("oc: connection from %s", addr)
	defer c.Close()

	r := bufio.NewReaderSize(c, 64<<10)
	attached := false
	defer func() {
		if attached && h.detach(c) {
			h.store.SetConnected(false, "")
			h.store.FailOpenJobs("OC отключился")
			log.Printf("oc: %s disconnected", addr)
		}
	}()

	for {
		_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
		line, err := r.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("oc: %s: %v", addr, err)
			}
			return
		}
		var m inMsg
		if err := UnmarshalLua(strings.TrimSpace(string(line)), &m); err != nil {
			log.Printf("oc: %s: bad message: %v", addr, err)
			continue
		}
		if !attached {
			if m.Type != "hello" {
				log.Printf("oc: %s: expected hello, got %q", addr, m.Type)
				return
			}
			if h.token != "" && m.Token != h.token {
				_ = writeLine(c, map[string]any{"type": "error", "message": "bad token"})
				log.Printf("oc: %s: bad token", addr)
				return
			}
			h.attach(c)
			attached = true
			h.store.SetConnected(true, addr)
			log.Printf("oc: %s authenticated", addr)
			continue
		}
		h.store.Touch()
		h.dispatch(m)
	}
}

func (h *Hub) dispatch(m inMsg) {
	switch m.Type {
	case "items_begin":
		h.store.BeginItems()
	case "items":
		h.store.AddItems(convertItems(m.Items))
	case "items_end":
		h.recordFix(h.store.EndItems())
	case "item_delta":
		h.store.ApplyItemDelta(convertItems(m.Items))
		if h.Stats != nil {
			flows := map[string]Flow{}
			for _, w := range m.Items {
				if w.Name != "" {
					addFlow(flows, w.item().Key(), int64(w.Size), int64(w.Out))
				}
			}
			h.Stats.Record(flows)
		}
	case "fluids":
		h.recordFix(h.store.SetFluids(convertFluids(m.Fluids)))
	case "fluid_delta":
		h.store.ApplyFluidDelta(convertFluids(m.Fluids))
		if h.Stats != nil {
			flows := map[string]Flow{}
			for _, w := range m.Fluids {
				if w.Name != "" {
					addFlow(flows, fluidKeyPrefix+w.Name, int64(w.Amount), int64(w.Out))
				}
			}
			h.Stats.Record(flows)
		}
	case "cpus":
		cpus := make([]CPU, 0, len(m.CPUs))
		for _, c := range m.CPUs {
			cpu := CPU{Name: c.Name, Storage: int64(c.Storage), Coprocessors: int(c.Coprocessors), Busy: c.Busy}
			if c.Busy && c.Output != nil && c.Output.Name != "" {
				out := c.Output.item()
				cpu.Output, cpu.OutputLabel = out.Key(), out.Label
			}
			if c.Busy {
				for _, it := range convertItems(c.Parts) {
					cpu.Parts = append(cpu.Parts, it.Key())
					cpu.PartLabels = append(cpu.PartLabels, it.Label)
				}
			}
			cpus = append(cpus, cpu)
		}
		h.store.SetCPUs(cpus)
	case "power":
		h.store.SetPower(Power{
			Stored: m.Stored, Max: m.Max, Usage: m.Usage, Injection: m.Injection,
			MemFree: m.MemFree, MemTotal: m.MemTotal,
		})
	case "job":
		h.store.UpdateJob(m.Job, m.State, m.Reason)
	case "log":
		log.Printf("oc says: %s", m.Message)
	case "fail":
		log.Printf("oc stopped: %s", m.Message)
		h.store.SetFailure(m.Message)
	case "pong", "status":
	default:
		log.Printf("oc: unknown message type %q", m.Type)
	}
}

func convertItems(in []wireItem) []Item {
	out := make([]Item, 0, len(in))
	for _, w := range in {
		if w.Name == "" {
			continue
		}
		out = append(out, w.item())
	}
	return out
}

func convertFluids(in []wireFluid) []Fluid {
	out := make([]Fluid, 0, len(in))
	for _, w := range in {
		if w.Name == "" {
			continue
		}
		out = append(out, w.fluid())
	}
	return out
}

func (h *Hub) attach(c net.Conn) {
	h.mu.Lock()
	old := h.conn
	h.conn = c
	h.mu.Unlock()
	if old != nil {
		log.Printf("oc: replacing previous connection %s", old.RemoteAddr())
		old.Close()
	}
}

// detach clears the active connection if it is still c.
func (h *Hub) detach(c net.Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn != c {
		return false
	}
	h.conn = nil
	return true
}

func writeLine(c net.Conn, msg map[string]any) error {
	s, err := MarshalLua(msg)
	if err != nil {
		return err
	}
	_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = c.Write([]byte(s + "\n"))
	return err
}

var errNotConnected = errors.New("OC не подключён")

func (h *Hub) send(msg map[string]any) error {
	h.mu.Lock()
	c := h.conn
	h.mu.Unlock()
	if c == nil {
		return errNotConnected
	}
	h.wmu.Lock()
	defer h.wmu.Unlock()
	return writeLine(c, msg)
}

// Craft queues a crafting request for it on the OC side.
func (h *Hub) Craft(it Item, amount int64, auto bool) (*Job, error) {
	if !it.Craftable {
		return nil, fmt.Errorf("%s нельзя скрафтить", it.Label)
	}
	if amount <= 0 {
		return nil, errors.New("количество должно быть больше нуля")
	}
	job := h.store.NewJob(it, amount, auto)
	msg := map[string]any{
		"type": "craft", "job": job.ID,
		"name": it.Name, "damage": it.Damage, "label": it.Label, "amount": amount,
	}
	if it.Fluid {
		msg["fluid"] = true
	}
	err := h.send(msg)
	if err != nil {
		h.store.UpdateJob(job.ID, "failed", err.Error())
		return nil, err
	}
	return job, nil
}

func (h *Hub) Resync() error {
	return h.send(map[string]any{"type": "resync"})
}

// addFlow turns a delta into income and spending. Scripts older than the
// out field send only the net change.
func addFlow(flows map[string]Flow, key string, net, out int64) {
	if out == 0 && net < 0 {
		out = -net
	}
	f := flows[key]
	f.add(Flow{In: net + out, Out: out})
	flows[key] = f
}

// recordFix stores what a full sync found that the change events missed.
func (h *Hub) recordFix(diff map[string]int64) {
	if h.Stats == nil || len(diff) == 0 {
		return
	}
	flows := make(map[string]Flow, len(diff))
	for k, d := range diff {
		flows[k] = Flow{Fix: d}
	}
	h.Stats.Record(flows)
}
