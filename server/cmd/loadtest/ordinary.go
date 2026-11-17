package main

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// ordinaryAPIEndpoints are the staff read paths a dashboard hits on refresh.
// They carry no per-request writes, so an elevated error rate points at
// connection-pool, query or index pressure rather than at business conflicts.
var ordinaryAPIEndpoints = []string{
	"/api/v1/projects?limit=20",
	"/api/v1/deliveries?limit=20",
	"/api/v1/notifications?limit=20",
}

func runOrdinaryAPI(ctx context.Context, client *apiClient, token string, cfg config) (*report, error) {
	rep := newReport(cfg)
	m := newMetrics()
	started := time.Now()
	deadline := started.Add(cfg.Duration)

	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for n := 0; time.Now().Before(deadline); n++ {
				if err := ctx.Err(); err != nil {
					return
				}
				path := ordinaryAPIEndpoints[(worker+n)%len(ordinaryAPIEndpoints)]
				begin := time.Now()
				status, _, err := client.do(ctx, http.MethodGet, path, token, nil)
				elapsed := time.Since(begin)
				switch {
				case err != nil:
					m.record(path, elapsed, "error")
				case status == http.StatusOK:
					m.record(path, elapsed, "success")
				case status == http.StatusTooManyRequests:
					m.record(path, elapsed, "rate_limited")
					time.Sleep(200 * time.Millisecond)
				default:
					m.record(path, elapsed, "error")
					time.Sleep(50 * time.Millisecond)
				}
				if cfg.Think > 0 {
					time.Sleep(cfg.Think)
				}
			}
		}(i)
	}
	wg.Wait()
	rep.finish(m, started, cfg)
	if rep.Success == 0 {
		rep.fail("no read request succeeded")
		rep.Passed = false
	}
	return rep, nil
}
