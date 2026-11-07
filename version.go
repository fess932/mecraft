package main

import (
	"runtime/debug"
	"time"
)

// buildVersion names the commit the binary was built from, as Go records
// it when building inside the git repository: "527d8bf от 07.11.2026 19:59",
// with a mark when the working tree had uncommitted changes.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "неизвестна"
	}
	var rev, at string
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.time":
			if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
				at = t.Local().Format("02.01.2006 15:04")
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "неизвестна (собрано не из git)"
	}
	v := rev[:min(7, len(rev))]
	if at != "" {
		v += " от " + at
	}
	if dirty {
		v += ", с незакоммиченными изменениями"
	}
	return v
}
