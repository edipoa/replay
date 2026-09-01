package main

import (
	"context"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"

	"github.com/edipo/replay-saas/internal/obs"
)

// livepreview: throwaway visual harness for internal/obs/live.go.
// Real obs server on :8899; a proxy on :8890 forces Host=aovivo.local so a
// plain browser sees the live page. Open http://127.0.0.1:8890/?seam=4
func main() {
	sp := os.Args[1]
	r, err := obs.New(obs.Config{DBPath: sp + "/preview.db", HTTPAddr: "127.0.0.1:8899", LiveDir: sp + "/live"})
	if err != nil {
		panic(err)
	}
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
