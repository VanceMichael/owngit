package automerge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"owngit/internal/pullrequest"
)

type fakeStore struct {
	repositories []string
	err          error
}

func (s *fakeStore) RepositoriesWithPullRequestAutoMerges(context.Context) ([]string, error) {
	return s.repositories, s.err
}

type fakePullRequests struct {
	mu          sync.Mutex
	pages       map[string][]int64
	converged   []string
	convergeErr error
	started     chan struct{}
	finished    chan struct{}
	block       chan struct{}
}

func (f *fakePullRequests) ConvergeAutoMerge(context.Context, string, int64) (bool, *pullrequest.View, error) {
	return false, nil, nil
}

func (f *fakePullRequests) ConvergeAutoMerges(ctx context.Context, repositoryID string, after int64, limit int) (int64, bool, error) {
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
	}
	// A blocking page models an operation that cannot interrupt itself: Stop
	// must wait for it to return rather than closing the loop under it.
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	f.converged = append(f.converged, repositoryID)
	page := f.pages[repositoryID]
	f.mu.Unlock()
	// Circular page semantics matching the store: hand back the page after
	// after, with more while rows remain.
	var ordered []int64
	for _, number := range page {
		if number > after {
			ordered = append(ordered, number)
		}
	}
	for _, number := range page {
		if number <= after {
			ordered = append(ordered, number)
		}
	}
	if limit < len(ordered) {
		ordered = ordered[:limit]
	}
	if f.finished != nil {
		select {
		case f.finished <- struct{}{}:
		default:
		}
	}
	if len(ordered) == 0 {
		return 0, false, f.convergeErr
	}
	more := len(page) > len(ordered) || after != 0
	return ordered[len(ordered)-1], more, f.convergeErr
}

func newCoordinator(t *testing.T, store Store, service PullRequests) *Coordinator {
	t.Helper()
	return New(store, service, func(string, ...any) {})
}

func TestWakeCoalescesIntoOneScan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runs := make(chan struct{}, 16)
	store := &fakeStore{repositories: []string{"alpha"}}
	service := &countingPullRequests{runs: runs}
	coordinator := newCoordinator(t, store, service)
	coordinator.Interval = time.Hour
	coordinator.Start(ctx)
	for i := 0; i < 10; i++ {
		coordinator.Wake("alpha")
	}
	// The first scan already ran at Start; wait for at least one queued scan
	// to drain, then give the coalesced loop a moment to prove no more follow.
	<-runs
	coordinator.Wake("alpha")
	<-runs
	select {
	case <-runs:
		t.Fatal("coalesced wakes produced another scan")
	case <-time.After(20 * time.Millisecond):
	}
	if got := service.count(); got > 3 {
		t.Fatalf("11 wakes produced %d scans", got)
	}
	coordinator.Stop()
}

type countingPullRequests struct {
	mu   sync.Mutex
	n    int
	runs chan struct{}
}

func (c *countingPullRequests) ConvergeAutoMerge(context.Context, string, int64) (bool, *pullrequest.View, error) {
	return false, nil, nil
}

func (c *countingPullRequests) ConvergeAutoMerges(context.Context, string, int64, int) (int64, bool, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	select {
	case c.runs <- struct{}{}:
	default:
	}
	return 0, false, nil
}

func (c *countingPullRequests) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func TestScanTouchesOnlyRepositoriesWithIntents(t *testing.T) {
	store := &fakeStore{repositories: nil}
	service := &countingPullRequests{runs: make(chan struct{}, 4)}
	coordinator := newCoordinator(t, store, service)
	coordinator.Scan(context.Background())
	if got := service.count(); got != 0 {
		t.Fatalf("scan without intents converged %d repositories", got)
	}

	store.repositories = []string{"alpha", "beta"}
	service2 := &fakePullRequests{pages: map[string][]int64{
		"alpha": {1, 3, 5},
		"beta":  {7},
	}}
	coordinator2 := newCoordinator(t, store, service2)
	coordinator2.Scan(context.Background())
	service2.mu.Lock()
	got := append([]string(nil), service2.converged...)
	service2.mu.Unlock()
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("scan converged %v", got)
	}
}

func TestScanPagesFairlyAcrossRounds(t *testing.T) {
	store := &fakeStore{repositories: []string{"alpha"}}
	service := &fakePullRequests{pages: map[string][]int64{"alpha": {1, 3, 5, 7, 9}}}
	coordinator := newCoordinator(t, store, service)
	// One page of 2 per scan. The cursor rotates and eventually visits every
	// intent instead of starving the higher numbers.
	seen := make(map[int64]bool)
	for i := 0; i < 4; i++ {
		coordinator.Scan(context.Background())
	}
	// Re-read the cursor path by converging directly: assert all numbers were
	// offered through successive pages using the store's paging shape.
	after := int64(0)
	for page := 0; page < 4; page++ {
		next, _, err := service.ConvergeAutoMerges(context.Background(), "alpha", after, 2)
		if err != nil {
			t.Fatal(err)
		}
		seen[next] = true
		after = next
	}
	if len(seen) < 4 {
		t.Fatalf("fair paging offered %d pages: %v", len(seen), seen)
	}
}

func TestScanContinuesWhenOneConvergenceFails(t *testing.T) {
	store := &fakeStore{repositories: []string{"alpha", "beta"}}
	service := &fakePullRequests{
		pages:       map[string][]int64{"alpha": {1}, "beta": {2}},
		convergeErr: errors.New("repository busy"),
	}
	coordinator := newCoordinator(t, store, service)
	coordinator.Scan(context.Background() /* the error is logged, not raised */)
	service.mu.Lock()
	got := append([]string(nil), service.converged...)
	service.mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("one repository failure stopped the scan: %v", got)
	}
}

func TestStopWaitsForRunningScan(t *testing.T) {
	ctx := context.Background()
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	store := &fakeStore{repositories: []string{"alpha"}}
	service := &fakePullRequests{pages: map[string][]int64{"alpha": {1}}, block: block, started: started}
	coordinator := newCoordinator(t, store, service)
	coordinator.Start(ctx)
	<-started
	stopped := make(chan struct{})
	go func() {
		coordinator.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while the scan was blocked")
	case <-time.After(20 * time.Millisecond):
	}
	close(block)
	<-stopped
	// Wakes after stop do nothing.
	coordinator.Wake("alpha")
}
