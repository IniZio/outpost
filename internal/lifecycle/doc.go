// Package lifecycle runs tasks by feeding events to domain.Apply and carrying out the effects it
// returns. It holds no rule: every decision is the domain's. What it owns is mechanism.
//
//   - Serial per task. step takes the task's lock, loads, applies, commits and enqueues. Callers
//     (the API, feeds, finished effects, the wake and scheduler loops) call it from their own
//     goroutines. There is no mailbox goroutine: the lock is the queue. A lost compare-and-swap
//     reloads and applies again.
//   - The lane. Each task's effects start in the order Apply emitted them, across Applies. A
//     command, which has no answer, runs to completion before the next effect starts. An ask starts
//     as its own job and the lane moves on, so a minutes-long start never holds up a message and its
//     Progressed events apply while it runs. AbortStart cancels the running StartTask and waits for
//     it, so the Release behind it never races the start it cleans up after.
//   - Answers. Every ask gets exactly one answer. A port error becomes domain.FailedAnswer. A job
//     cancelled by an abort or a drain answers nothing: the domain has moved on, or the next
//     process's Recovered asks again.
//   - The outbox. Commands are stored in the same commit as the task and marked done after they
//     run. Asks are not: the task already records each one. Run replays the rows first at boot.
//   - Wake. One timer over the store's wake index, which every commit keeps exact.
//   - The scheduler. A coalesced pass of domain.PickStarts over the live tasks and online machines.
//   - Feeds. One Boxes.Watch per machine. The adapter owns reconnects and resends levels, and the
//     domain drops what is stale.
package lifecycle
