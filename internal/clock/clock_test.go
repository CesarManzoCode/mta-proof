package clock

import (
	"testing"
	"time"
)

func TestVirtualOnlyMovesForward(t *testing.T) {
	v := NewVirtual(time.UnixMilli(0))
	v.Advance(1500 * time.Millisecond)
	v.Advance(-time.Hour)
	v.Set(time.UnixMilli(1000))
	if got := Ms(v.Now()); got != 1500 {
		t.Fatalf("virtual clock at %d ms, want 1500", got)
	}
	v.Set(time.UnixMilli(86_400_000))
	if got := Ms(v.Now()); got != 86_400_000 {
		t.Fatalf("virtual clock at %d ms, want 86400000", got)
	}
}

func TestRandStreams(t *testing.T) {
	draw := func(seed uint64, comp string) [4]uint64 {
		r := Rand(seed, comp)
		return [4]uint64{r.Uint64(), r.Uint64(), r.Uint64(), r.Uint64()}
	}
	if draw(7, "engine.retry") != draw(7, "engine.retry") {
		t.Fatal("same seed and component gave different streams")
	}
	if draw(7, "engine.retry") == draw(8, "engine.retry") {
		t.Fatal("different seeds gave the same stream")
	}
	if draw(7, "engine.retry") == draw(7, "engine.mx") {
		t.Fatal("different components gave the same stream")
	}
}
