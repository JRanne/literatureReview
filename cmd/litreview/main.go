// Command litreview runs the AI-assisted literature review web application.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"literaturereview/internal/config"
	"literaturereview/internal/dotenv"
	"literaturereview/internal/server"
	"literaturereview/internal/store"
)

func main() {
	dotenv.Load(".env") // optional; the shell environment takes precedence
	cfg := config.FromEnv()
	flag.StringVar(&cfg.Addr, "addr", cfg.Addr, "listen address")
	flag.StringVar(&cfg.DataDir, "data", cfg.DataDir, "data directory for projects and PDFs")
	flag.Parse()

	if err := cfg.Validate(); err != nil {
		log.Printf("WARNING: %v", err)
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	srv, err := server.New(cfg, st)
	if err != nil {
		log.Fatalf("init server: %v", err)
	}
	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("litreview listening on %s (data: %s)", listenURL(cfg.Addr), cfg.DataDir)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Print("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

// listenURL turns a listen address into a URL to open: ":8080" listens on
// every interface and is reached at localhost; "127.0.0.1:9000" is kept.
func listenURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "http://localhost" + addr
	}
	return "http://" + addr
}
