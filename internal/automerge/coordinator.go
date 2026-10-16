// Package automerge runs the background convergence of pull requests whose
// owners enabled automatic merging. A wake asks the coordinator to process
// one bounded page of each repository with an enabled intent; the pull
// request service decides, under its repository lock, whether a request is
// still waiting, blocked, or merges now. Repeated wakes coalesce, and a
// minute-granularity ticker retries requests that wait on a conflict or a
// temporarily unavailable repository.
package automerge

import (
	"context"
	"sync"
	"time"

	"owngit/internal/pullrequest"
)

// pageSize bounds how many intents one scan page processes per repository.
// One page covers every normal installation; the cursor rotates fairly past
// larger sets across wakes.
const pageSize = 64

// defaultInterval is the safety-net retry cadence. Event wakes drive the
// normal convergence.
const defaultInterval = 5 * time.Minute

// Store is the state read the coordinator needs. *state.Store satisfies it.
type Store interface {
	RepositoriesWithPullRequestAutoMerges(ctx context.Context) ([]string, error)
}

// PullRequests is the converging part of the pull request service.
// *pullrequest.Service satisfies it.
type PullRequests interface {
	ConvergeAutoMerge(ctx context.Context, repositoryID string, number int64) (bool, *pullrequest.View, error)
	ConvergeAutoMerges(ctx context.Context, repositoryID string, after int64, limit int) (int64, bool, error)
}

// Coordinator coalesces wake signals into bounded convergence scans. Its zero
// value is not ready; construct one with New.
type Coordinator struct {
	Store        Store
	PullRequests PullRequests
	Logf         func(string, ...any)
	Interval     time.Duration

	startOnce sync.Once
	stopOnce  sync.Once
	wake      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}

	cursorsMu sync.Mutex
	cursors   map[string]int64
}

// New builds a coordinator.
func New(store Store, service PullRequests, logf func(string, ...any)) *Coordinator {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Coordinator{
		Store: store, PullRequests: service, Logf: logf,
		Interval: defaultInterval, cursors: make(map[string]int64),
	}
}

// Start launches the loop and queues one immediate scan, which converges
// intents that survived a restart or a restore.
func (c *Coordinator) Start(ctx context.Context) {
	c.startOnce.Do(func() {
		c.ctx, c.cancel = context.WithCancel(ctx)
		c.wake = make(chan struct{}, 1)
		c.done = make(chan struct{})
		c.wake <- struct{}{}
		go c.loop()
	})
}

// Stop ends the loop and waits for the running scan to finish.
func (c *Coordinator) Stop() {
	c.stopOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
			<-c.done
		}
	})
}

// Wake queues a convergence scan. Signals while one is queued or running
// coalesce, and a wake before Start or after Stop is a no-op.
func (c *Coordinator) Wake(string) {
	if c.wake == nil {
		return
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Coordinator) loop() {
	defer close(c.done)
	interval := c.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.wake:
			c.scan(c.ctx)
		case <-ticker.C:
			c.scan(c.ctx)
		}
	}
}

// scan processes one bounded page of every repository with an enabled intent.
// It is also called directly by tests.
func (c *Coordinator) scan(ctx context.Context) {
	repositories, err := c.Store.RepositoriesWithPullRequestAutoMerges(ctx)
	if err != nil {
		c.Logf("auto-merge scan could not list repositories: %v", err)
		return
	}
	for _, repositoryID := range repositories {
		if ctx.Err() != nil {
			return
		}
		after := c.cursor(repositoryID)
		next, more, err := c.PullRequests.ConvergeAutoMerges(ctx, repositoryID, after, pageSize)
		if err != nil {
			c.Logf("auto-merge convergence in repository %s will retry: %v", repositoryID, err)
		}
		if !more {
			c.resetCursor(repositoryID)
			continue
		}
		c.setCursor(repositoryID, next)
	}
}

// Scan exposes one convergence round to callers outside the loop.
func (c *Coordinator) Scan(ctx context.Context) {
	c.scan(ctx)
}

func (c *Coordinator) cursor(repositoryID string) int64 {
	c.cursorsMu.Lock()
	defer c.cursorsMu.Unlock()
	return c.cursors[repositoryID]
}

func (c *Coordinator) setCursor(repositoryID string, after int64) {
	c.cursorsMu.Lock()
	defer c.cursorsMu.Unlock()
	c.cursors[repositoryID] = after
}

func (c *Coordinator) resetCursor(repositoryID string) {
	c.cursorsMu.Lock()
	defer c.cursorsMu.Unlock()
	delete(c.cursors, repositoryID)
}
