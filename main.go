// mecraft shows the contents of a GTNH ME network in the browser and
// places crafting orders. An OpenComputers computer (oc/mecraft.lua)
// connects over TCP and streams the network state; the web UI is htmx.
package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
)

//go:embed assets/templates/*.html
var templatesFS embed.FS

//go:embed assets/static
var staticFS embed.FS

//go:embed all:assets/icons
var iconsFS embed.FS

//go:embed oc/*.lua
var ocFS embed.FS

func sub(f fs.FS, dir string) fs.FS {
	s, err := fs.Sub(f, dir)
	if err != nil {
		log.Fatal(err)
	}
	return s
}

func main() {
	configPath := flag.String("config", "mecraft.json", "settings and stock levels; created with defaults if missing")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	every, _ := cfg.Interval()

	store := NewStore()
	hub := NewHub(store, cfg.Token)
	rules := NewRules(*configPath, cfg)
	icons := sub(iconsFS, "assets/icons")
	web, err := NewWeb(store, hub, LoadIcons(icons), rules, sub(templatesFS, "assets/templates"))
	if err != nil {
		log.Fatal(err)
	}
	go NewMaintainer(store, hub, rules, every).Run()

	go func() {
		if err := hub.Serve(cfg.OC); err != nil {
			log.Fatal(err)
		}
	}()

	log.Printf("web: listening on %s", cfg.HTTP)
	handler := web.Routes(sub(staticFS, "assets/static"), icons, sub(ocFS, "oc"))
	if err := http.ListenAndServe(cfg.HTTP, handler); err != nil {
		log.Fatal(err)
	}
}
