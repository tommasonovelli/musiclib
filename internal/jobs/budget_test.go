package jobs

import (
	"sync"
	"sync/atomic"
	"testing"
)

// §11.2: concurrent reservations never spend the same free space. Many
// goroutines race to reserve from the same statfs reading; the sum of the
// reservations granted never exceeds free minus the margin, and every
// release gives the space back.
func TestBudgetConcurrentReservationsNeverOverspend(t *testing.T) {
	const (
		free     = SpaceMargin + 100<<20 // 100 MiB above the margin
		estimate = 7 << 20
		workers  = 64
	)
	for round := range 50 {
		b := NewBudget()
		var (
			wg      sync.WaitGroup
			granted atomic.Int64
			held    []*Reservation
			mu      sync.Mutex
		)
		start := make(chan struct{})
		for range workers {
			wg.Go(func() {
				<-start
				r, _ := b.Reserve(free, estimate)
				if r == nil {
					return
				}
				granted.Add(estimate)
				mu.Lock()
				held = append(held, r)
				mu.Unlock()
			})
		}
		close(start)
		wg.Wait()
		if g := granted.Load(); g > free-SpaceMargin {
			t.Fatalf("round %d: %d bytes granted, only %d available", round, g, free-SpaceMargin)
		}
		if g, want := granted.Load(), int64((free-SpaceMargin)/estimate)*estimate; g != want {
			t.Fatalf("round %d: %d bytes granted, want every fitting reservation (%d)", round, g, want)
		}
		if b.Reserved() != granted.Load() {
			t.Fatalf("round %d: the budget holds %d, %d were granted", round, b.Reserved(), granted.Load())
		}
		for _, r := range held {
			r.Release()
			r.Release() // idempotent
		}
		if b.Reserved() != 0 {
			t.Fatalf("round %d: %d bytes still reserved after every release", round, b.Reserved())
		}
	}
}

// Reserve reports what the job could have had; a nil reservation releases
// as a no-op; a negative estimate is refused.
func TestBudgetReserve(t *testing.T) {
	b := NewBudget()
	r1, avail := b.Reserve(SpaceMargin+10, 6)
	if r1 == nil || avail != 10 {
		t.Fatalf("first reservation: %v, avail %d", r1, avail)
	}
	if r, avail := b.Reserve(SpaceMargin+10, 5); r != nil || avail != 4 {
		t.Fatalf("second reservation: %v, avail %d; want refused with 4", r, avail)
	}
	if r, _ := b.Reserve(SpaceMargin+10, -1); r != nil {
		t.Fatal("a negative estimate was reserved")
	}
	if r, _ := b.Reserve(SpaceMargin-1, 0); r != nil {
		t.Fatal("a reservation below the margin was granted")
	}
	r1.Release()
	if r, _ := b.Reserve(SpaceMargin+10, 10); r == nil {
		t.Fatal("the released space was not given back")
	}
	var none *Reservation
	none.Release()
}
