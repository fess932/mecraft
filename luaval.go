package main

// Reading and writing the Lua literals produced and accepted by the
// OpenOS `serialization` library, so the OC side needs no extra code:
// it sends serialization.serialize(msg) and reads with unserialize.

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// ParseLua parses one value written by serialization.serialize:
// nil, booleans, numbers (including 0/0, math.huge, -math.huge),
// quoted strings and tables. Tables with keys 1..n only become []any,
// other tables map[string]any, empty tables nil.
func ParseLua(src string) (any, error) {
	p := &luaParser{s: src}
	p.space()
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.space()
	if p.i != len(p.s) {
		return nil, p.errf("unexpected %q after value", p.rest())
	}
	return v, nil
}

// UnmarshalLua parses a Lua literal into a Go value the way
// encoding/json would parse the equivalent JSON.
func UnmarshalLua(src string, out any) error {
	v, err := ParseLua(src)
	if err != nil {
		return err
	}
	b, err := json.Marshal(jsonSafe(v))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// jsonSafe replaces NaN and infinities, which JSON cannot hold.
func jsonSafe(v any) any {
	switch t := v.(type) {
	case float64:
		switch {
		case math.IsNaN(t):
			return 0.0
		case math.IsInf(t, 1):
			return math.MaxFloat64
		case math.IsInf(t, -1):
			return -math.MaxFloat64
		}
	case []any:
		for i := range t {
			t[i] = jsonSafe(t[i])
		}
	case map[string]any:
		for k := range t {
			t[k] = jsonSafe(t[k])
		}
	}
	return v
}

type luaParser struct {
	s string
	i int
}

func (p *luaParser) errf(format string, args ...any) error {
	return fmt.Errorf("lua literal at %d: %s", p.i, fmt.Sprintf(format, args...))
}

func (p *luaParser) rest() string {
	r := p.s[p.i:]
	if len(r) > 20 {
		r = r[:20] + "…"
	}
	return r
}

func (p *luaParser) space() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *luaParser) peek() byte {
	if p.i < len(p.s) {
		return p.s[p.i]
	}
	return 0
}

func (p *luaParser) value() (any, error) {
	switch c := p.peek(); {
	case c == '{':
		return p.table()
	case c == '"' || c == '\'':
		return p.str()
	case c == '-' || c == '.' || (c >= '0' && c <= '9'):
		return p.number()
	case isIdentStart(c):
		id := p.ident()
		switch id {
		case "nil":
			return nil, nil
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "math":
			if strings.HasPrefix(p.s[p.i:], ".huge") {
				p.i += len(".huge")
				return math.Inf(1), nil
			}
		}
		return nil, p.errf("unexpected identifier %q", id)
	case c == 0:
		return nil, p.errf("unexpected end of input")
	default:
		return nil, p.errf("unexpected %q", p.rest())
	}
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdent(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func (p *luaParser) ident() string {
	start := p.i
	for p.i < len(p.s) && isIdent(p.s[p.i]) {
		p.i++
	}
	return p.s[start:p.i]
}

func (p *luaParser) number() (any, error) {
	neg := false
	if p.peek() == '-' {
		neg = true
		p.i++
		p.space()
		if strings.HasPrefix(p.s[p.i:], "math.huge") {
			p.i += len("math.huge")
			return math.Inf(-1), nil
		}
	}
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == 'x' || c == 'X' ||
			(c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') ||
			((c == '+' || c == '-') && p.i > start && (p.s[p.i-1] == 'e' || p.s[p.i-1] == 'E')) {
			p.i++
			continue
		}
		break
	}
	f, err := parseLuaNumber(p.s[start:p.i])
	if err != nil {
		return nil, p.errf("bad number %q", p.s[start:p.i])
	}
	// 0/0 is how serialize writes NaN.
	if p.peek() == '/' {
		p.i++
		d := p.i
		for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			p.i++
		}
		den, err := strconv.ParseFloat(p.s[d:p.i], 64)
		if err != nil {
			return nil, p.errf("bad divisor")
		}
		f /= den
		if den == 0 && f == 0 {
			f = math.NaN()
		}
	}
	if neg {
		f = -f
	}
	return f, nil
}

func parseLuaNumber(s string) (float64, error) {
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		n, err := strconv.ParseUint(s[2:], 16, 64)
		return float64(int64(n)), err
	}
	return strconv.ParseFloat(s, 64)
}

func (p *luaParser) str() (string, error) {
	q := p.s[p.i]
	p.i++
	var b strings.Builder
	for {
		if p.i >= len(p.s) {
			return "", p.errf("unterminated string")
		}
		c := p.s[p.i]
		p.i++
		switch {
		case c == q:
			return b.String(), nil
		case c == '\\':
			if p.i >= len(p.s) {
				return "", p.errf("unterminated escape")
			}
			e := p.s[p.i]
			p.i++
			switch e {
			case 'n', '\n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'a':
				b.WriteByte('\a')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'v':
				b.WriteByte('\v')
			case '\\', '"', '\'':
				b.WriteByte(e)
			case 'z':
				for p.i < len(p.s) && strings.IndexByte(" \t\n\r\f\v", p.s[p.i]) >= 0 {
					p.i++
				}
			case 'x':
				if p.i+2 > len(p.s) {
					return "", p.errf("bad \\x escape")
				}
				n, err := strconv.ParseUint(p.s[p.i:p.i+2], 16, 8)
				if err != nil {
					return "", p.errf("bad \\x escape")
				}
				b.WriteByte(byte(n))
				p.i += 2
			default:
				if e < '0' || e > '9' {
					return "", p.errf("bad escape \\%c", e)
				}
				n := int(e - '0')
				for k := 0; k < 2 && p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9'; k++ {
					n = n*10 + int(p.s[p.i]-'0')
					p.i++
				}
				if n > 255 {
					return "", p.errf("escape \\%d out of range", n)
				}
				b.WriteByte(byte(n))
			}
		default:
			b.WriteByte(c)
		}
	}
}

func (p *luaParser) table() (any, error) {
	p.i++ // {
	entries := map[string]any{}
	maxInt, ints := 0, 0
	next := 1 // next positional index
	put := func(k any, v any) {
		if v == nil {
			return
		}
		key := luaKey(k)
		if _, dup := entries[key]; !dup {
			if n, err := strconv.Atoi(key); err == nil && n >= 1 && strconv.Itoa(n) == key {
				ints++
				if n > maxInt {
					maxInt = n
				}
			}
		}
		entries[key] = v
	}
	for {
		p.space()
		if p.peek() == '}' {
			p.i++
			break
		}
		var key any
		positional := false
		switch c := p.peek(); {
		case c == '[':
			p.i++
			p.space()
			k, err := p.value()
			if err != nil {
				return nil, err
			}
			p.space()
			if p.peek() != ']' {
				return nil, p.errf("expected ]")
			}
			p.i++
			p.space()
			if p.peek() != '=' {
				return nil, p.errf("expected =")
			}
			p.i++
			key = k
		case isIdentStart(c):
			save := p.i
			id := p.ident()
			p.space()
			if p.peek() == '=' {
				p.i++
				key = id
			} else {
				p.i = save // a value such as true, nil or math.huge
				positional = true
			}
		default:
			positional = true
		}
		p.space()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		if positional {
			key = float64(next)
			next++
		}
		put(key, v)
		p.space()
		if c := p.peek(); c == ',' || c == ';' {
			p.i++
		}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	if ints == len(entries) && maxInt == len(entries) {
		arr := make([]any, maxInt)
		for k, v := range entries {
			n, _ := strconv.Atoi(k)
			arr[n-1] = v
		}
		return arr, nil
	}
	return entries, nil
}

func luaKey(k any) string {
	switch t := k.(type) {
	case string:
		return t
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return fmt.Sprint(k)
}

// MarshalLua writes v as a Lua literal that serialization.unserialize
// reads: maps (string keys, sorted), slices, strings, numbers, bools, nil.
func MarshalLua(v any) (string, error) {
	var b strings.Builder
	if err := writeLua(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

var luaKeywords = map[string]bool{
	"and": true, "break": true, "do": true, "else": true, "elseif": true, "end": true,
	"false": true, "for": true, "function": true, "goto": true, "if": true, "in": true,
	"local": true, "nil": true, "not": true, "or": true, "repeat": true, "return": true,
	"then": true, "true": true, "until": true, "while": true,
}

func plainKey(k string) bool {
	if k == "" || luaKeywords[k] || !isIdentStart(k[0]) {
		return false
	}
	for i := 1; i < len(k); i++ {
		if !isIdent(k[i]) {
			return false
		}
	}
	return true
}

func writeLua(b *strings.Builder, v any) error {
	switch t := v.(type) {
	case nil:
		b.WriteString("nil")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case string:
		writeLuaString(b, t)
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		switch {
		case math.IsNaN(t):
			b.WriteString("0/0")
		case math.IsInf(t, 1):
			b.WriteString("math.huge")
		case math.IsInf(t, -1):
			b.WriteString("-math.huge")
		default:
			b.WriteString(strconv.FormatFloat(t, 'g', -1, 64))
		}
	case []any:
		b.WriteByte('{')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeLua(b, e); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if plainKey(k) {
				b.WriteString(k)
			} else {
				b.WriteByte('[')
				writeLuaString(b, k)
				b.WriteByte(']')
			}
			b.WriteByte('=')
			if err := writeLua(b, t[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("lua: unsupported type %T", v)
	}
	return nil
}

// writeLuaString quotes s so that no raw newline or control byte remains,
// which keeps one message per line.
func writeLuaString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c < 32 || c == 127:
			fmt.Fprintf(b, `\%03d`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}
