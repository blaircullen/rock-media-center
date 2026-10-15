package metadata

import (
	"context"
	"sync"
	"time"
)

// MBClass is a request's priority class at the MusicBrainz pacer (KTD15, R32).
type MBClass int

const (
	// MBCatalogue is the default: album-module lookups and library cataloguing.
	MBCatalogue MBClass = iota
	// MBSweep is the background upgrade sweep.
	MBSweep
	// MBIntake is a live playlist request; it is served before everything else.
	MBIntake
)

type mbClassKey struct{}

// WithMBClass returns a context whose MusicBrainz requests use the given priority class.
//
// The class travels in the context rather than as a parameter: every existing MusicBrainz
// method already takes a ctx, so the MusicProvider interface and its fakes stay untouched,
// and an unmarked context is the catalogue class — exactly what existing callers want.
func WithMBClass(ctx context.Context, c MBClass) context.Context {
	return context.WithValue(ctx, mbClassKey{}, c)
}

// MBClassOf reads the class from ctx, defaulting to MBCatalogue.
func MBClassOf(ctx context.Context) MBClass {
	if c, ok := ctx.Value(mbClassKey{}).(MBClass); ok && c >= MBCatalogue && c <= MBIntake {
		return c
	}
	return MBCatalogue
}

// mbPacer hands out request slots: at most one per interval overall, highest class first,
// FIFO within a class. It uses no goroutine of its own — each waiter decides for itself
// whether it is at the head of the queue, and waiters are woken by closing a shared channel.
type mbPacer struct {
	mu      sync.Mutex
	queues  [3][]*mbWaiter // indexed by MBClass
	last    time.Time      // when the previous slot was handed out
	changed chan struct{}  // closed and replaced whenever the queue or last changes
	now     func() time.Time
}

type mbWaiter struct{ class MBClass }

func newMBPacer() *mbPacer {
	return &mbPacer{changed: make(chan struct{}), now: time.Now}
}

// head returns the waiter that should be served next, or nil. Caller holds p.mu.
func (p *mbPacer) head() *mbWaiter {
	for c := MBIntake; c >= MBCatalogue; c-- {
		if len(p.queues[c]) > 0 {
			return p.queues[c][0]
		}
	}
	return nil
}

// remove drops w from its queue. Caller holds p.mu.
func (p *mbPacer) remove(w *mbWaiter) {
	q := p.queues[w.class]
	for i, x := range q {
		if x == w {
			p.queues[w.class] = append(q[:i:i], q[i+1:]...)
			return
		}
	}
}

// broadcast wakes every waiter to re-evaluate. Caller holds p.mu.
func (p *mbPacer) broadcast() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// wait blocks until the caller owns the next slot, or ctx ends (in which case the waiter
// leaves the queue and releases anyone behind it). interval is the spacing between slots.
func (p *mbPacer) wait(ctx context.Context, class MBClass, interval time.Duration) error {
	w := &mbWaiter{class: class}
	p.mu.Lock()
	p.queues[class] = append(p.queues[class], w)
	p.broadcast() // a higher-class arrival changes who is head
	p.mu.Unlock()

	for {
		p.mu.Lock()
		var timer <-chan time.Time
		if p.head() == w {
			wait := interval - p.now().Sub(p.last)
			if wait <= 0 {
				p.remove(w)
				p.last = p.now()
				p.broadcast()
				p.mu.Unlock()
				return nil
			}
			timer = time.After(wait)
		}
		changed := p.changed
		p.mu.Unlock()

		select {
		case <-ctx.Done():
			p.mu.Lock()
			p.remove(w)
			p.broadcast()
			p.mu.Unlock()
			return ctx.Err()
		case <-changed:
		case <-timer:
		}
	}
}
