package main

import (
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Lines below were produced by OpenOS serialization.serialize (Lua 5.4).
func TestParseLuaSerializeOutput(t *testing.T) {
	cases := []struct {
		src  string
		want any
	}{
		{
			`{items={{name="gregtech:gt.metaitem.01",isCraftable=true,damage=2086,hasTag=false,size=1234567,label="Титановый слиток"}},type="items"}`,
			map[string]any{"type": "items", "items": []any{map[string]any{
				"name": "gregtech:gt.metaitem.01", "isCraftable": true, "damage": 2086.0,
				"hasTag": false, "size": 1234567.0, "label": "Титановый слиток",
			}}},
		},
		{
			`{message="quote \" backslash \\ nl\nnl tab\9 cr\13 nul\0 bell\7 end",type="log"}`,
			map[string]any{"type": "log", "message": "quote \" backslash \\ nl\nnl tab\t cr\r nul\x00 bell\a end"},
		},
		{
			`{["end"]=2,[true]="yes",["weird key"]=1,[3]="three"}`,
			map[string]any{"end": 2.0, "true": "yes", "weird key": 1.0, "3": "three"},
		},
		{`{1,2,3,"four"}`, []any{1.0, 2.0, 3.0, "four"}},
		{`{}`, nil},
		{
			`{list={{x=1},{x=2}},nested={a={b={c={}}}}}`,
			// empty tables become nil, so the whole "nested" chain disappears
			map[string]any{"list": []any{map[string]any{"x": 1.0}, map[string]any{"x": 2.0}}},
		},
		{`{neg=-42,int=3,frac=0.125}`, map[string]any{"neg": -42.0, "int": 3.0, "frac": 0.125}},
		{`{[1]="a",[2]="b"}`, []any{"a", "b"}},
		{`{"a",[3]="c",[2]="b"}`, []any{"a", "b", "c"}},
	}
	for _, c := range cases {
		got, err := ParseLua(c.src)
		if err != nil {
			t.Errorf("ParseLua(%s): %v", c.src, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseLua(%s)\n got %#v\nwant %#v", c.src, got, c.want)
		}
	}
}

func TestParseLuaSpecialNumbers(t *testing.T) {
	v, err := ParseLua(`{injection=-math.huge,max=0/0,usage=math.huge,stored=1.5e+15}`)
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if !math.IsInf(m["injection"].(float64), -1) || !math.IsInf(m["usage"].(float64), 1) ||
		!math.IsNaN(m["max"].(float64)) || m["stored"].(float64) != 1.5e15 {
		t.Errorf("got %#v", m)
	}
}

func TestParseLuaErrors(t *testing.T) {
	for _, src := range []string{`{a=}`, `{a="x}`, `{a=1`, `{a=1} junk`, `{a=os.exit()}`, `"\q"`} {
		if _, err := ParseLua(src); err == nil {
			t.Errorf("ParseLua(%s): want error", src)
		}
	}
}

func TestUnmarshalLuaIntoMessage(t *testing.T) {
	var m inMsg
	src := `{items={{name="a:b",damage=3,label="X",size=-7,isCraftable=false}},type="item_delta"}`
	if err := UnmarshalLua(src, &m); err != nil {
		t.Fatal(err)
	}
	if m.Type != "item_delta" || len(m.Items) != 1 || m.Items[0].Size != -7 || m.Items[0].Damage != 3 {
		t.Errorf("got %+v", m)
	}
	// An empty list arrives as {} and must not break decoding.
	if err := UnmarshalLua(`{fluids={},type="fluids"}`, &m); err != nil || len(m.Fluids) != 0 {
		t.Errorf("empty table: %v %+v", err, m.Fluids)
	}
}

func TestMarshalLuaKeepsOneLine(t *testing.T) {
	s, err := MarshalLua(map[string]any{
		"type": "craft", "label": "a\"b\\c\nd\x00e\r", "amount": int64(64), "end": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(s, "\n\r\x00") {
		t.Fatalf("raw control byte in %q", s)
	}
	back, err := ParseLua(s)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"type": "craft", "label": "a\"b\\c\nd\x00e\r", "amount": 64.0, "end": true}
	if !reflect.DeepEqual(back, want) {
		t.Errorf("round trip\n got %#v\nwant %#v", back, want)
	}
}

// TestLuaRoundTrip feeds Go output through the real OpenOS serialization
// library and parses its answer back. Skipped without a lua binary.
func TestLuaRoundTrip(t *testing.T) {
	lua, err := exec.LookPath("lua")
	if err != nil {
		t.Skip("lua not installed")
	}
	msg := map[string]any{
		"type": "craft", "job": int64(12), "name": "gregtech:gt.metaitem.01", "damage": 2086,
		"label": "Титан \"кавычки\" \\ слэш\nперенос\x00ноль\x07", "amount": int64(1 << 40),
		"list": []any{"a", 2.5, true}, "end": "keyword key", "weird key": 1,
	}
	line, err := MarshalLua(msg)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(in, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `function checkArg() end
local ser = dofile(arg[1])
local f = assert(io.open(arg[2], "rb"))
local line = f:read("l")
f:close()
local v = assert(ser.unserialize(line))
io.write(ser.serialize(v))`
	sp := filepath.Join(dir, "rt.lua")
	if err := os.WriteFile(sp, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(lua, sp, filepath.Join("testdata", "serialization.lua"), in).Output()
	if err != nil {
		t.Fatalf("lua: %v", err)
	}
	if strings.Contains(string(out), "\n") {
		t.Errorf("serialize produced a raw newline: %q", out)
	}
	back, err := ParseLua(string(out))
	if err != nil {
		t.Fatalf("parse %q: %v", out, err)
	}
	want := map[string]any{
		"type": "craft", "job": 12.0, "name": "gregtech:gt.metaitem.01", "damage": 2086.0,
		"label": "Титан \"кавычки\" \\ слэш\nперенос\x00ноль\x07", "amount": float64(1 << 40),
		"list": []any{"a", 2.5, true}, "end": "keyword key", "weird key": 1.0,
	}
	if !reflect.DeepEqual(back, want) {
		t.Errorf("round trip\n got %#v\nwant %#v", back, want)
	}
}
