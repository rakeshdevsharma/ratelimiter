// Command loadtest sends traffic to the Nginx rate limiter from many fake
// client IPs. Each simulated client sets X-Forwarded-For, which Nginx's realip
// module (trusted from localhost only) turns into $remote_addr / X-Real-IP.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"text/tabwriter"
	"time"
)

type client struct {
	ip    string
	class string  // "heavy", "light" or "-"
	rps   float64 // steady/mixed send rate
}

type result struct {
	ip      string
	second  int // seconds since start when the request was sent
	status  int // 0 on transport error
	err     error
	latency time.Duration
}

type ipStats struct {
	sent, ok, limited, other int
}

type secondStats struct {
	ok, limited       int
	firstOK, firstLim int // for the first IP only
}

func main() {
	target := flag.String("target", "http://localhost:8080/", "URL to hit (Nginx)")
	mode := flag.String("mode", "steady", "traffic pattern: steady, burst or mixed")
	numIPs := flag.Int("ips", 10, "number of simulated client IPs")
	duration := flag.Duration("duration", 5*time.Second, "test length (steady, mixed)")
	rps := flag.Float64("rps", 20, "requests/second per IP (steady)")
	burst := flag.Int("burst", 50, "requests per IP fired at once (burst)")
	heavy := flag.Int("heavy", 2, "number of heavy IPs (mixed)")
	heavyRPS := flag.Float64("heavy-rps", 50, "requests/second per heavy IP (mixed)")
	lightRPS := flag.Float64("light-rps", 2, "requests/second per light IP (mixed)")
	timeline := flag.Bool("timeline", false, "print a per-second allowed/limited breakdown")
	timeout := flag.Duration("timeout", 2*time.Second, "per-request timeout")
	flag.Parse()

	if *numIPs < 1 || *numIPs > 65534 {
		log.Fatal("-ips must be between 1 and 65534")
	}

	clients := make([]client, *numIPs)
	for i := range clients {
		n := i + 1
		clients[i] = client{ip: fmt.Sprintf("10.0.%d.%d", n/256, n%256), class: "-", rps: *rps}
		if *mode == "mixed" {
			if i < *heavy {
				clients[i].class, clients[i].rps = "heavy", *heavyRPS
			} else {
				clients[i].class, clients[i].rps = "light", *lightRPS
			}
		}
	}

	httpClient := &http.Client{
		Timeout: *timeout,
		Transport: &http.Transport{
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 256,
			MaxConnsPerHost:     256,
			IdleConnTimeout:     30 * time.Second,
		},
	}

	results := make(chan result, 1024)
	start := time.Now()
	var inflight sync.WaitGroup

	fire := func(ip string) {
		defer inflight.Done()
		sent := time.Now()
		r := result{ip: ip, second: int(sent.Sub(start) / time.Second)}
		req, err := http.NewRequest(http.MethodGet, *target, nil)
		if err != nil {
			log.Fatal(err)
		}
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := httpClient.Do(req)
		r.latency = time.Since(sent)
		if err != nil {
			r.err = err
		} else {
			io.Copy(io.Discard, resp.Body) // drain so the connection is reused
			resp.Body.Close()
			r.status = resp.StatusCode
		}
		results <- r
	}

	// Collector: the only goroutine touching the aggregates, so no locks.
	perIP := map[string]*ipStats{}
	perSec := map[int]*secondStats{}
	var latencies []time.Duration
	var firstErr string
	collected := make(chan struct{})
	go func() {
		for r := range results {
			s := perIP[r.ip]
			if s == nil {
				s = &ipStats{}
				perIP[r.ip] = s
			}
			sec := perSec[r.second]
			if sec == nil {
				sec = &secondStats{}
				perSec[r.second] = sec
			}
			s.sent++
			latencies = append(latencies, r.latency)
			first := r.ip == clients[0].ip
			switch {
			case r.status == http.StatusOK:
				s.ok++
				sec.ok++
				if first {
					sec.firstOK++
				}
			case r.status == http.StatusTooManyRequests:
				s.limited++
				sec.limited++
				if first {
					sec.firstLim++
				}
			default:
				s.other++
				if firstErr == "" {
					if r.err != nil {
						firstErr = r.err.Error()
					} else {
						firstErr = fmt.Sprintf("HTTP %d", r.status)
					}
				}
			}
		}
		close(collected)
	}()

	switch *mode {
	case "steady", "mixed":
		fmt.Printf("%s: %d IPs for %s against %s\n", *mode, len(clients), *duration, *target)
		deadline := time.After(*duration)
		var senders sync.WaitGroup
		stop := make(chan struct{})
		for _, c := range clients {
			if c.rps <= 0 {
				continue
			}
			senders.Add(1)
			go func(c client) {
				defer senders.Done()
				ticker := time.NewTicker(time.Duration(float64(time.Second) / c.rps))
				defer ticker.Stop()
				for {
					select {
					case <-stop:
						return
					case <-ticker.C:
						// Each request in its own goroutine so slow responses
						// never lower the send rate.
						inflight.Add(1)
						go fire(c.ip)
					}
				}
			}(c)
		}
		<-deadline
		close(stop)
		senders.Wait()
	case "burst":
		fmt.Printf("burst: %d IPs x %d requests at once against %s\n", len(clients), *burst, *target)
		gate := make(chan struct{})
		var ready sync.WaitGroup
		for _, c := range clients {
			for range *burst {
				inflight.Add(1)
				ready.Add(1)
				go func(ip string) {
					ready.Done()
					<-gate
					fire(ip)
				}(c.ip)
			}
		}
		ready.Wait()
		close(gate) // release all requests together
	default:
		log.Fatalf("unknown -mode %q (valid: steady, burst, mixed)", *mode)
	}

	inflight.Wait()
	elapsed := time.Since(start)
	close(results)
	<-collected

	report(clients, perIP, perSec, latencies, firstErr, *mode, *duration, elapsed, *timeline)
}

func report(clients []client, perIP map[string]*ipStats, perSec map[int]*secondStats,
	latencies []time.Duration, firstErr, mode string, duration, elapsed time.Duration, timeline bool) {

	// allowed/s is measured over the configured duration for steady/mixed; a
	// burst has no meaningful duration, so show raw counts only.
	secs := duration.Seconds()

	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "IP\tclass\tsent\t200\t429\tother/err\tallowed/s\t")
	var total ipStats
	var warnings []string
	for _, c := range clients { // clients are generated in IP order
		s := perIP[c.ip]
		if s == nil {
			s = &ipStats{}
		}
		total.sent += s.sent
		total.ok += s.ok
		total.limited += s.limited
		total.other += s.other
		rate := "-"
		if mode != "burst" {
			rate = fmt.Sprintf("%.1f", float64(s.ok)/secs)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%s\t\n", c.ip, c.class, s.sent, s.ok, s.limited, s.other, rate)
		if c.class == "light" && s.limited > 0 {
			warnings = append(warnings, fmt.Sprintf("WARN: light IP %s got %d 429s (limits shared between IPs, or a bug?)", c.ip, s.limited))
		}
	}
	fmt.Fprintf(tw, "TOTAL\t\t%d\t%d\t%d\t%d\t\t\n", total.sent, total.ok, total.limited, total.other)
	tw.Flush()

	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		pct := func(p float64) time.Duration { return latencies[int(p*float64(len(latencies)-1))] }
		fmt.Printf("\nlatency  p50=%s  p90=%s  p99=%s  max=%s   (wall time %s)\n",
			pct(0.50), pct(0.90), pct(0.99), latencies[len(latencies)-1], elapsed.Round(time.Millisecond))
	}

	if timeline {
		fmt.Printf("\nper second (all IPs, and first IP %s):\n", clients[0].ip)
		tw = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
		fmt.Fprintln(tw, "second\t200\t429\tfirst IP 200\tfirst IP 429\t")
		seconds := make([]int, 0, len(perSec))
		for s := range perSec {
			seconds = append(seconds, s)
		}
		sort.Ints(seconds)
		for _, s := range seconds {
			st := perSec[s]
			fmt.Fprintf(tw, "%d\t%d\t%d\t%d\t%d\t\n", s, st.ok, st.limited, st.firstOK, st.firstLim)
		}
		tw.Flush()
	}

	for _, w := range warnings {
		fmt.Println(w)
	}
	if total.other > 0 {
		fmt.Printf("\n%d requests got neither 200 nor 429; first: %s\n", total.other, firstErr)
		fmt.Println("hint: 'connection refused' = Nginx not running; HTTP 500 = Go limiter down or it panicked (check its log)")
	}
}
