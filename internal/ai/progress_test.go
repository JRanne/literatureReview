package ai

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"literaturereview/internal/model"
)

// barrierBackend holds every call until `parties` calls are in flight, so the
// concurrent batches finish together and their progress reports collide.
type barrierBackend struct {
	parties int
	mu      sync.Mutex
	arrived int
	release chan struct{}
}

func newBarrier(n int) *barrierBackend {
	return &barrierBackend{parties: n, release: make(chan struct{})}
}

func (b *barrierBackend) Name() string             { return "barrier" }
func (b *barrierBackend) ModelLabel() string       { return "barrier" }
func (b *barrierBackend) WithModel(string) Backend { return b }
func (b *barrierBackend) Complete(ctx context.Context, _, _ string, _ int) (string, error) {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.parties {
		close(b.release)
	}
	b.mu.Unlock()
	select {
	case <-b.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return "[]", nil
}

// progressChecker fails when progress calls overlap or go backwards. The
// short pause inside the callback is what lets an unserialised caller enter
// twice; it does not wait for any result.
type progressChecker struct {
	inside   atomic.Int32
	last     int
	problems []string
}

func (p *progressChecker) report(done, total int) {
	if p.inside.Add(1) > 1 {
		p.problems = append(p.problems, "overlapping progress calls")
	}
	time.Sleep(5 * time.Millisecond)
	if done < p.last {
		p.problems = append(p.problems, fmt.Sprintf("progress went from %d back to %d", p.last, done))
	}
	p.last = done
	p.inside.Add(-1)
}

func papersN(n int) []*model.Paper {
	var ps []*model.Paper
	for i := 0; i < n; i++ {
		ps = append(ps, &model.Paper{ID: fmt.Sprint(i), Title: fmt.Sprint("paper ", i)})
	}
	return ps
}

func TestProgressIsSerialisedAndMonotonic(t *testing.T) {
	proj := &model.Project{Question: "q"}
	runs := map[string]func(c *Client, pc *progressChecker) error{
		// 60 papers in batches of 20 → 3 concurrent calls.
		"screening": func(c *Client, pc *progressChecker) error {
			return c.ScreenPapers(context.Background(), proj, papersN(60), pc.report)
		},
		// 18 papers in batches of 6 → 3 concurrent calls.
		"extraction": func(c *Client, pc *progressChecker) error {
			return c.ExtractPapers(context.Background(), proj, papersN(18), nil, pc.report)
		},
		"concepts": func(c *Client, pc *progressChecker) error {
			return c.AssignConcepts(context.Background(), proj, []model.Concept{{Name: "x"}}, papersN(assignBatch*3), pc.report)
		},
	}
	for name, run := range runs {
		pc := &progressChecker{}
		_ = run(New(newBarrier(3)), pc)
		if len(pc.problems) > 0 {
			t.Errorf("%s: %v", name, pc.problems)
		}
	}
}
