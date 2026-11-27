// Command addref files individual references in a Zotero desktop collection:
// each DOI or title is looked up in OpenAlex, its PDF fetched through the
// access chain (open access, publisher, university proxy, headless browser)
// and the item stored with the PDF via Zotero's local API. It reuses the
// server's code but runs against a scratch data directory, so it can be
// used while the web app is running.
//
//	addref -collection "System 1 paper" 10.2307/249689 "Survey of hallucination in natural language generation"
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"literaturereview/internal/config"
	"literaturereview/internal/dotenv"
	"literaturereview/internal/server"
	"literaturereview/internal/store"
)

func main() {
	dotenv.Load(".env")
	cfg := config.FromEnv()
	var (
		collection = flag.String("collection", "", "Zotero collection to add to (created if missing)")
		title      = flag.String("title", "manual references", "label used for the Zotero tag/citation keys")
		settings   = flag.String("settings", "data", "data directory whose settings.json (proxy) and zotero.json (local key) are reused")
		pdfDir     = flag.String("pdfs", "", "where to keep the downloaded PDFs (default: a temporary directory)")
		refsFile   = flag.String("refs", "", "file with one DOI or title per line (in addition to arguments)")
	)
	flag.Parse()
	refs := flag.Args()
	if *refsFile != "" {
		b, err := os.ReadFile(*refsFile)
		if err != nil {
			log.Fatal(err)
		}
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				refs = append(refs, l)
			}
		}
	}
	if *collection == "" || len(refs) == 0 {
		fmt.Fprintln(os.Stderr, "usage: addref -collection NAME [-refs FILE] [DOI|title ...]")
		os.Exit(2)
	}
	// Scratch data dir: copies of the two settings files, nothing else, so the
	// running web app's project files are never touched.
	scratch, err := os.MkdirTemp("", "addref-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(scratch)
	for _, f := range []string{"settings.json", "zotero.json"} {
		if b, err := os.ReadFile(filepath.Join(*settings, f)); err == nil {
			_ = os.WriteFile(filepath.Join(scratch, f), b, 0o600)
		}
	}
	cfg.DataDir = scratch
	st, err := store.Open(scratch)
	if err != nil {
		log.Fatal(err)
	}
	srv, err := server.New(cfg, st)
	if err != nil {
		log.Fatal(err)
	}
	dir := *pdfDir
	if dir == "" {
		dir = filepath.Join(scratch, "pdfs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := srv.AddReferencesStandalone(ctx, *title, dir, refs, *collection); err != nil {
		log.Fatalf("addref: %v", err)
	}
}
