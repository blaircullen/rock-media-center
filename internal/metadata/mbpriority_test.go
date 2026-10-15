package metadata

import (
	"context"
	"sync"
	"testing"
	"time"
)

const testSlot = 40 * time.Millisecond

// waitInBackground starts a pacer wait and returns once it is queued behind earlier calls.
func queueWaiter(t *testing.T, p *mbPacer, ctx context.Context, class MBClass, name string, order *[]string, mu *sync.Mutex, wg *sync.WaitGroup) {
	t.Helper()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := p.wait(ctx, class, testSlot); err != nil {
			return
		}
		mu.Lock()
		*order = append(*order, name)
		mu.Unlock()
	}()
	time.Sleep(5 * time.Millisecond) // let it enqueue so FIFO order is the call order
}

func TestPacerServesIntakeBeforeSweepBeforeCatalogue(t *testing.T) {
	p := newMBPacer()
	ctx := context.Background()
	if err := p.wait(ctx, MBCatalogue, testSlot); err != nil { // occupies the current slot
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		order []string
		wg    sync.WaitGroup
	)
	queueWaiter(t, p, ctx, MBCatalogue, "cat1", &order, &mu, &wg)
	queueWaiter(t, p, ctx, MBSweep, "sweep1", &order, &mu, &wg)
	queueWaiter(t, p, ctx, MBCatalogue, "cat2", &order, &mu, &wg)
	queueWaiter(t, p, ctx, MBIntake, "intake1", &order, &mu, &wg)
	queueWaiter(t, p, ctx, MBIntake, "intake2", &order, &mu, &wg)
	wg.Wait()

	want := []string{"intake1", "intake2", "sweep1", "cat1", "cat2"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v (intake > sweep > catalogue, FIFO within a class)", order, want)
		}
	}
}

func TestPacerNeverIssuesTwoSlotsInsideOneInterval(t *testing.T) {
	p := newMBPacer()
	var (
		mu     sync.Mutex
		stamps []time.Time
		wg     sync.WaitGroup
	)
	classes := []MBClass{MBCatalogue, MBIntake, MBSweep, MBCatalogue, MBIntake, MBSweep}
	for _, c := range classes {
		wg.Add(1)
		go func(c MBClass) {
			defer wg.Done()
			if err := p.wait(context.Background(), c, testSlot); err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			stamps = append(stamps, time.Now())
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	if len(stamps) != len(classes) {
		t.Fatalf("got %d slots, want %d", len(stamps), len(classes))
	}
	// Stamps are appended in near-claim order; sort-free check on the span is robust to the
	// few microseconds of reordering: n slots need at least (n-1) intervals end to end.
	first, last := stamps[0], stamps[0]
	for _, s := range stamps {
		if s.Before(first) {
			first = s
		}
		if s.After(last) {
			last = s
		}
	}
	min := time.Duration(len(classes)-1)*testSlot - 5*time.Millisecond
	if last.Sub(first) < min {
		t.Errorf("%d slots spanned %v, want >= %v (one per %v)", len(classes), last.Sub(first), min, testSlot)
	}
}

func TestPacerContextCancelReleasesWaiter(t *testing.T) {
	p := newMBPacer()
	if err := p.wait(context.Background(), MBCatalogue, time.Hour); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.wait(ctx, MBIntake, time.Hour) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter was not released")
	}
	p.mu.Lock()
	n := len(p.queues[MBIntake])
	p.mu.Unlock()
	if n != 0 {
		t.Errorf("cancelled waiter still queued (%d)", n)
	}
}

// A cancelled head must not strand the waiters behind it.
func TestPacerCancelledHeadDoesNotBlockOthers(t *testing.T) {
	p := newMBPacer()
	if err := p.wait(context.Background(), MBCatalogue, testSlot); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = p.wait(ctx, MBIntake, testSlot) }()
	time.Sleep(5 * time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- p.wait(context.Background(), MBCatalogue, testSlot) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("err = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter behind a cancelled head never got a slot")
	}
}

func TestMBClassContextDefaultsToCatalogue(t *testing.T) {
	if got := MBClassOf(context.Background()); got != MBCatalogue {
		t.Errorf("default class = %v, want catalogue", got)
	}
	if got := MBClassOf(WithMBClass(context.Background(), MBIntake)); got != MBIntake {
		t.Errorf("class = %v, want intake", got)
	}
}
