package main

import (
	"encoding/json"
	"io/fs"
	"log"
	"strings"
)

// Icons maps item labels to icon files produced by cmd/importicons.
type Icons struct {
	byKey   map[string]string
	byLower map[string]string
}

const unknownIcon = "/static/unknown.svg"

func LoadIcons(fsys fs.FS) *Icons {
	ic := &Icons{byKey: map[string]string{}, byLower: map[string]string{}}
	data, err := fs.ReadFile(fsys, "index.json")
	if err != nil {
		log.Printf("icons: no index.json, icons disabled (run cmd/importicons)")
		return ic
	}
	if err := json.Unmarshal(data, &ic.byKey); err != nil {
		log.Printf("icons: bad index.json: %v", err)
		return ic
	}
	for k, v := range ic.byKey {
		lk := strings.ToLower(k)
		if _, ok := ic.byLower[lk]; !ok {
			ic.byLower[lk] = v
		}
	}
	log.Printf("icons: %d labels", len(ic.byKey))
	return ic
}

// URL returns the icon path for an item label, or a placeholder.
func (ic *Icons) URL(label string) string {
	k := IconKey(label)
	if f, ok := ic.byKey[k]; ok {
		return "/icons/" + f
	}
	if f, ok := ic.byLower[strings.ToLower(k)]; ok {
		return "/icons/" + f
	}
	return unknownIcon
}

// IconKey turns a display name into the file name NEI uses for its icon
// dump: formatting codes stripped, then `"<>|:*?\/` and control characters
// replaced with '_' (NEI GuiItemIconDumper.cleanFileName).
func IconKey(label string) string {
	var b strings.Builder
	skip := false
	for _, r := range label {
		if skip {
			skip = false
			continue
		}
		if r == '§' {
			skip = true
			continue
		}
		if r < 32 || strings.ContainsRune(`"<>|:*?\/`, r) {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
