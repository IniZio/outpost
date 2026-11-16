package lifecycle

import (
	"context"
	"fmt"
)

// Run drives tasks until ctx ends: take the lease, replay what the last process left, recover
// every unfinished task, then run the wake loop, the scheduler and one feed per machine.
//
// A deploy cancels the old process's context on SIGTERM. It stops answering and finishes the
// command in hand, releases the lease, and the new process takes over: its Recovered asks again
// what the old one left in flight.
func (r *Runner) Run(ctx context.Context) error {
	release, err := r.cfg.Store.Lease(ctx)
	if err != nil {
		return fmt.Errorf("lifecycle: take the lease: %w", err)
	}
	defer release()
	r.lead.Store(true)
	defer r.drain()
	defer r.bg.Wait()
	if err := r.recoverAll(ctx); err != nil {
		return err
	}
	r.startFeeds(ctx)
	r.bg.Go(func() { r.wakeLoop(ctx) })
	r.bg.Go(func() { r.passLoop(ctx) })
	close(r.started)
	<-ctx.Done()
	return nil
}

// drain stops this process leading. Jobs end without answering, queued effects are left to the
// outbox and to Recovered, and the commands in hand finish within their budget.
func (r *Runner) drain() {
	r.lead.Store(false)
	r.stop(errDraining)
	ctx, cancel := context.WithTimeout(r.base, r.cfg.Budgets.Command)
	defer cancel()
	if err := r.quiesce(ctx); err != nil {
		r.cfg.Log.WarnContext(ctx, "drain gave up waiting", "err", err)
	}
}
