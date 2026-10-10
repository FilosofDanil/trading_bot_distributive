package runners

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestE2E runs against a real runner (skipped unless RUNNER_E2E_URL is set), e.g.
//
//	CONTROL_TOKEN=dev python -m carry serve   (in funding_carry/)
//	RUNNER_E2E_URL=http://127.0.0.1:8080 RUNNER_E2E_TOKEN=dev go test ./internal/runners -run E2E -v
func TestE2E(t *testing.T) {
	url := os.Getenv("RUNNER_E2E_URL")
	if url == "" {
		t.Skip("RUNNER_E2E_URL not set")
	}
	c := NewClient("e2e", url, os.Getenv("RUNNER_E2E_TOKEN"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	h, err := c.Health(ctx)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	t.Logf("health: %+v", h)
	ss, err := c.Strategies(ctx)
	if err != nil || len(ss) == 0 {
		t.Fatalf("strategies: %v %v", ss, err)
	}
	id := ss[0].ID
	t.Logf("strategy: %+v", ss[0])
	if p, err := c.Plan(ctx, id); err != nil {
		t.Logf("plan: %v", err) // 404 before the first cycle
	} else {
		t.Logf("plan %s: book %v, %d orders, top %v, %d skipped", p.Day, p.Book, len(p.Orders), p.Top, len(p.Skipped))
	}
	if ps, err := c.Positions(ctx, id); err != nil {
		t.Fatalf("positions: %v", err)
	} else {
		t.Logf("positions: %+v", ps)
	}
	if r, err := c.Report(ctx, id); err != nil {
		t.Fatalf("report: %v", err)
	} else {
		t.Logf("report: %+v", r)
	}
	if s, err := c.Pause(ctx, id, 1); err != nil || s.Desired != "paused" {
		t.Fatalf("pause: %+v %v", s, err)
	}
	if s, err := c.Resume(ctx, id, 1); err != nil || s.Desired != "running" {
		t.Fatalf("resume: %+v %v", s, err)
	}
	evs, err := c.Events(ctx, id, -1, 3)
	if err != nil || len(evs) == 0 {
		t.Fatalf("events: %v %v", evs, err)
	}
	newer, err := c.Events(ctx, id, evs[0].Seq, 50)
	if err != nil || len(newer) != len(evs)-1 {
		t.Fatalf("events since %d: %d (want %d) %v", evs[0].Seq, len(newer), len(evs)-1, err)
	}
	for _, e := range evs {
		t.Logf("event %d %s %s %s", e.Seq, e.TS, e.Level, e.Msg)
	}
	if _, err := NewClient("e2e", url, "wrong").Health(ctx); err == nil {
		t.Fatal("a wrong token must be refused")
	}
}
