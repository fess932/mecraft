// importicons copies the NEI item panel icon dump (Options → Tools →
// Data Dumps → Item Panel, PNG mode) into assets/icons with safe file
// names and writes assets/icons/index.json mapping each NEI file name
// (the cleaned display name) to its new file.
//
// NEI names files after the display name and appends _2, _3, ... when two
// items share a name; those duplicates map to the first file.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var dupSuffix = regexp.MustCompile(`^(.*)_(\d+)$`)

func main() {
	src := flag.String("src", "", "NEI dump folder, e.g. .minecraft/dumps/itempanel_icons")
	dst := flag.String("dst", "assets/icons", "output folder")
	flag.Parse()
	if *src == "" {
		log.Fatal("usage: importicons -src <.minecraft/dumps/itempanel_icons> [-dst assets/icons]")
	}

	entries, err := os.ReadDir(*src)
	if err != nil {
		log.Fatal(err)
	}
	names := map[string]bool{}
	var list []string
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".png") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		names[base] = true
		list = append(list, base)
	}
	sort.Strings(list)

	if err := os.MkdirAll(*dst, 0o755); err != nil {
		log.Fatal(err)
	}
	if err := clean(*dst); err != nil {
		log.Fatal(err)
	}

	index := map[string]string{}
	copied, dups := 0, 0
	for _, base := range list {
		key := base
		if m := dupSuffix.FindStringSubmatch(base); m != nil && names[m[1]] {
			dups++
			continue // same display name as m[1]; keep the first icon only
		}
		file := fmt.Sprintf("%05d.png", copied)
		if err := copyFile(filepath.Join(*src, base+".png"), filepath.Join(*dst, file)); err != nil {
			log.Fatal(err)
		}
		index[key] = file
		copied++
	}

	data, err := json.MarshalIndent(index, "", " ")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*dst, "index.json"), data, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("icons: %d copied, %d duplicate names skipped -> %s\n", copied, dups, *dst)
}

// clean removes previously imported icons but keeps other files (.gitkeep).
func clean(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".png") || n == "index.json" {
			if err := os.Remove(filepath.Join(dir, n)); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
