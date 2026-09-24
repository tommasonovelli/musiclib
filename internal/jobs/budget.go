package jobs

import "sync"

// SpaceMargin is the free space that every import and build must leave on
// /data (§11.2: "margine di 1 GiB").
const SpaceMargin = 1 << 30

// Budget is the process-wide space budget of §11.2: it reserves, atomically,
// the conservative estimates of the imports and builds in progress, so that
// two workers never spend the same free space.
//
// It is in memory only. A process that stops loses it, and the next one
// starts empty: that is the "ricostruzione dopo la pulizia dei temp" of
// §11.2, since the boot removes every leftover temporary, staging and
// retired directory (§11.1 step 5) before any worker can reserve. It is not
// a guarantee against writes it does not know about, or against a final
// size larger than the estimate: every write still handles ENOSPC, and
// nothing published is ever removed to make room.
//
// The boot makes the process's one budget with NewBudget and hands it to
// the importer and to the builder.
type Budget struct {
	mu       sync.Mutex
	reserved int64
}

// NewBudget returns an empty budget.
func NewBudget() *Budget { return &Budget{} }

// Reservation is the space held by one job until Release.
type Reservation struct {
	b    *Budget
	n    int64
	once sync.Once
}

// Reserve reserves estimate bytes when free, the bytes available to the
// process on /data (statfs f_bavail, read by the caller just before), minus
// SpaceMargin and minus every reservation still held, is at least
// estimate. Otherwise it returns nil and the bytes this job could have
// had, for the caller's insufficient_space message.
//
// free may be read without any lock: reading it late can only count the
// bytes already written by a job that is still reserved twice, which is
// conservative, never the reverse, because the comparison and the
// reservation happen under the budget's mutex.
func (b *Budget) Reserve(free, estimate int64) (*Reservation, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	avail := free - SpaceMargin - b.reserved
	if estimate < 0 || avail < estimate {
		return nil, avail
	}
	b.reserved += estimate
	return &Reservation{b: b, n: estimate}, avail
}

// Reserved returns the bytes currently reserved, for logs and tests.
func (b *Budget) Reserved() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reserved
}

// Release returns the reservation to the budget. It is idempotent and safe
// on a nil reservation (a removal reserves nothing).
func (r *Reservation) Release() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		r.b.mu.Lock()
		r.b.reserved -= r.n
		r.b.mu.Unlock()
	})
}
