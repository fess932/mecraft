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
	"time"
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
	httpAddr := flag.String("http", ":8080", "address of the web UI")
	ocAddr := flag.String("oc", ":7777", "address the OpenComputers client connects to")
	token := flag.String("token", "", "shared secret the OC client must send (empty: no check)")
	rulesPath := flag.String("rules", "mecraft-rules.json", "file with stock level rules")
	every := flag.Duration("maintain-every", 5*time.Second, "how often stock levels are checked")
	flag.Parse()

	store := NewStore()
	hub := NewHub(store, *token)
	rules, err := LoadRules(*rulesPath)
	if err != nil {
		log.Fatal(err)
	}
	icons := sub(iconsFS, "assets/icons")
	web, err := NewWeb(store, hub, LoadIcons(icons), rules, sub(templatesFS, "assets/templates"))
	if err != nil {
		log.Fatal(err)
	}
	go NewMaintainer(store, hub, rules, *every).Run()

	go func() {
		if err := hub.Serve(*ocAddr); err != nil {
			log.Fatal(err)
		}
	}()

	log.Printf("web: listening on %s", *httpAddr)
	handler := web.Routes(sub(staticFS, "assets/static"), icons, sub(ocFS, "oc"))
	if err := http.ListenAndServe(*httpAddr, handler); err != nil {
		log.Fatal(err)
	}
}
