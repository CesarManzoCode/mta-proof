// Package clock provides the time and randomness sources injected into the
// engine core (contract 2.2, D05). The core never calls time.Now or a global
// random generator: it receives a Clock and seeded math/rand/v2 streams.
package clock

import (
	"crypto/sha256"
	"encoding/binary"
	"math/rand/v2"
	"sync"
	"time"
)

// Clock is the engine's only source of time.
type Clock interface {
	Now() time.Time
}

// Real is the wall clock.
type Real struct{}

// Now returns the wall-clock time.
func (Real) Now() time.Time { return time.Now() }

// Virtual is a clock that only moves when told to. It is safe for
// concurrent use, although the simulated executor drives it from one
// goroutine.
type Virtual struct {
	mu  sync.Mutex
	now time.Time
}

// NewVirtual returns a virtual clock reading start.
func NewVirtual(start time.Time) *Virtual { return &Virtual{now: start} }

// Now returns the current virtual time.
func (v *Virtual) Now() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.now
}

// Advance moves the clock forward by d; a negative d is ignored, so virtual
// time never goes backwards.
func (v *Virtual) Advance(d time.Duration) {
	if d <= 0 {
		return
	}
	v.mu.Lock()
	v.now = v.now.Add(d)
	v.mu.Unlock()
}

// Set moves the clock to t if t is later than the current reading.
func (v *Virtual) Set(t time.Time) {
	v.mu.Lock()
	if t.After(v.now) {
		v.now = t
	}
	v.mu.Unlock()
}

// Ms returns t as Unix milliseconds, the unit of at_ms in every log
// (contract 3.6).
func Ms(t time.Time) int64 { return t.UnixMilli() }

// Rand derives an independent, deterministic stream for one component from
// the run seed: the PCG state is the first 16 bytes of
// sha256(seed || component). Adding a component or drawing more numbers in
// one component does not shift the streams of the others.
func Rand(seed uint64, component string) *rand.Rand {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], seed)
	h := sha256.New()
	h.Write(b[:])
	h.Write([]byte(component))
	sum := h.Sum(nil)
	return rand.New(rand.NewPCG(binary.BigEndian.Uint64(sum[0:8]), binary.BigEndian.Uint64(sum[8:16])))
}
