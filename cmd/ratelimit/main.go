package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"study/ratelimiter/ratelimit"
	"study/ratelimiter/store"
)

func main() {
	strategy := flag.String("strategy", "", "rate limiting strategy: "+strings.Join(ratelimit.Names(), ", "))
	socket := flag.String("socket", "/tmp/ratelimiter.sock", "unix socket path to listen on")
	limit := flag.Int("limit", 10, "max requests per window / bucket capacity")
	window := flag.Duration("window", time.Second, "window length (fixed/sliding window); token/leaky bucket refill/drain at limit/window")
	flag.Parse()

	if *strategy == "" {
		fmt.Fprintln(os.Stderr, "-strategy is required")
		flag.Usage()
		os.Exit(2)
	}

	cfg := ratelimit.Config{Limit: *limit, Window: *window}
	limiter, err := ratelimit.New(*strategy, store.NewMemoryStore[ratelimit.RateLimitMeta](), cfg)
	if err != nil {
		log.Fatal(err)
	}

	// A socket file left over from a previous run makes Listen fail with "address already in use".
	if err := os.Remove(*socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Fatalf("remove stale socket: %v", err)
	}
	ln, err := net.Listen("unix", *socket)
	if err != nil {
		log.Fatal(err)
	}
	// Nginx workers usually run as a different user; let them connect.
	if err := os.Chmod(*socket, 0o666); err != nil {
		log.Fatalf("chmod socket: %v", err)
	}

	var newConns atomic.Int64
	srv := &http.Server{
		Handler:           newHandler(limiter),
		ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout:      2 * time.Second,
		// Longer than Nginx's upstream keepalive_timeout (60s default), so Go
		// never closes an idle connection that Nginx is about to reuse.
		IdleTimeout: 120 * time.Second,
		// Logging new connections makes keepalive reuse visible: under load this
		// should stay small instead of growing by one per request.
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateNew {
				log.Printf("new connection (total %d)", newConns.Add(1))
			}
		},
	}

	go func() {
		log.Printf("strategy=%s limit=%d window=%s listening on unix:%s",
			*strategy, cfg.Limit, cfg.Window, *socket)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Print("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	os.Remove(*socket)
}

func newHandler(limiter ratelimit.RateLimiter) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /check", func(w http.ResponseWriter, r *http.Request) {
		ip := r.Header.Get("X-Real-IP") // set by Nginx from $remote_addr
		if ip == "" {
			ip = "unknown"
		}
		// auth_request only understands 2xx (allow), 401 and 403 (deny); any
		// other status becomes a 500 for the client. So translate the limiter's
		// 429 into 403, and Nginx maps it back to 429 via error_page.
		switch limiter.Check(ip) {
		case http.StatusOK:
			w.WriteHeader(http.StatusNoContent)
		case http.StatusTooManyRequests:
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
