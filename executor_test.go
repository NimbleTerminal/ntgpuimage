package gpuimage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestExecutorRunsJobsInOrderAndReturnsResults(t *testing.T) {
	e := newExecutor()
	defer e.stop()
	var order []int
	for i := range 5 {
		if err := e.run(context.Background(), func() { order = append(order, i) }); err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	if len(order) != 5 {
		t.Fatalf("ran %d jobs, want 5", len(order))
	}
	for i, v := range order {
		if v != i {
			t.Fatalf("order = %v", order)
		}
	}
}

func TestExecutorSerializesConcurrentCallers(t *testing.T) {
	e := newExecutor()
	defer e.stop()
	var inside, maxInside int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.run(context.Background(), func() {
				mu.Lock()
				inside++
				if inside > maxInside {
					maxInside = inside
				}
				mu.Unlock()
				mu.Lock()
				inside--
				mu.Unlock()
			})
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("max concurrent jobs = %d, want 1", maxInside)
	}
}

func TestExecutorStopIsIdempotentAndRejectsRun(t *testing.T) {
	e := newExecutor()
	e.stop()
	e.stop()
	err := e.run(context.Background(), func() { t.Fatal("job ran after stop") })
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("run after stop = %v, want ErrClosed", err)
	}
}

func TestExecutorRunGivesUpWhileQueued(t *testing.T) {
	e := newExecutor()
	defer e.stop()
	started, release := make(chan struct{}), make(chan struct{})
	go e.run(context.Background(), func() {
		close(started)
		<-release
	})
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := e.run(ctx, func() { t.Error("job ran after its deadline") })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run queued past its deadline = %v, want context.DeadlineExceeded", err)
	}
	close(release)
	// The executor still accepts work.
	ran := false
	if err := e.run(context.Background(), func() { ran = true }); err != nil || !ran {
		t.Fatalf("run after an abandoned job: ran=%v err=%v", ran, err)
	}
}
