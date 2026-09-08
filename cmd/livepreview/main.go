package main

import (
	"context"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"time"

	"github.com/edipo/replay-saas/internal/obs"
)

// livepreview: throwaway visual harness for internal/obs/live.go.
// Real obs server on :8899; a proxy on :8890 forces Host=aovivo.local so a
// plain browser sees the live page. Open http://127.0.0.1:8890/?seam=4
func main() {
	sp := os.Args[1]
	db := sp + "/preview.db"
	if len(os.Args) > 2 {
		db = os.Args[2]
	}
	r, err := obs.New(obs.Config{DBPath: db, HTTPAddr: "127.0.0.1:8899", LiveDir: sp + "/live"})
	if err != nil {
		panic(err)
	}
	r.SetTrigger(func(t time.Time) { log.Println("TRIGGER fired for", t.Format(time.RFC3339)) })

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go r.ServeHTTP(ctx)

	target, _ := url.Parse("http://127.0.0.1:8899")
	px := httputil.NewSingleHostReverseProxy(target)
	d := px.Director
	px.Director = func(req *http.Request) { d(req); req.Host = "aovivo.local" }
	srv := &http.Server{Addr: "127.0.0.1:8890", Handler: px}
	go func() { <-ctx.Done(); srv.Close() }()
	log.Println("open http://127.0.0.1:8890/?seam=4")
	_ = srv.ListenAndServe()
}
