package ntgpuimage

import (
	"context"
	"runtime"
	"sync"
)

// executor runs every job on one goroutine that is pinned to an OS thread
// for its whole life, so a Renderer's GPU work is serialized and stays on
// one thread. On wasm LockOSThread is a no-op and the executor simply
// serializes jobs. A direct executor instead serializes jobs on their caller
// goroutines, leaving host thread affinity to the caller.
type executor struct {
	jobs   chan func()
	quit   chan struct{}
	done   chan struct{}
	once   sync.Once
	direct chan struct{} // cancellable serialization on the caller goroutine
}

func newExecutor() *executor {
	e := &executor{
		jobs: make(chan func()),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	go e.loop()
	return e
}

func newDirectExecutor() *executor {
	e := &executor{direct: make(chan struct{}, 1), quit: make(chan struct{})}
	e.direct <- struct{}{}
	return e
}

func (e *executor) loop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(e.done)
	for {
		select {
		case job := <-e.jobs:
			job()
		case <-e.quit:
			return
		}
	}
}

// run executes f on the executor goroutine, or directly for borrowed devices,
// and returns after f returns.
// Without running f, it returns ErrClosed after stop, and ctx.Err() when ctx
// ends while the job waits for the executor. An accepted job always runs to
// completion, so f must observe ctx itself.
func (e *executor) run(ctx context.Context, f func()) error {
	if e.direct != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.quit:
			return ErrClosed
		case <-e.direct:
		}
		defer func() { e.direct <- struct{}{} }()
		select {
		case <-e.quit:
			return ErrClosed
		default:
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		f()
		return nil
	}
	finished := make(chan struct{})
	job := func() {
		defer close(finished)
		f()
	}
	select {
	case e.jobs <- job:
	case <-e.quit:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	<-finished
	return nil
}

// stop ends the loop and waits for it to exit. Safe to call repeatedly.
func (e *executor) stop() {
	e.once.Do(func() { close(e.quit) })
	if e.direct != nil {
		<-e.direct
		e.direct <- struct{}{}
		return
	}
	<-e.done
}
