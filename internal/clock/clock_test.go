package clock

import (
	"sync"
	"testing"
	"time"
)

func TestFakeClock(t *testing.T) {
	initial := time.Date(2026, 1, 2, 3, 4, 5, 6000000, time.FixedZone("offset", 3600))
	c := NewFake(initial)
	var shared Clock = c
	if got := shared.Now(); !got.Equal(initial) || got.Location() != time.UTC {
		t.Fatalf("Now = %v; want UTC instant %v", got, initial.UTC())
	}
	c.Advance(3*time.Second + 2*time.Millisecond)
	if got := shared.Now(); !got.Equal(initial.Add(3002 * time.Millisecond)) {
		t.Fatalf("advanced Now = %v", got)
	}
	reset := initial.Add(-time.Hour)
	c.Set(reset)
	if got := shared.Now(); !got.Equal(reset) || got.Location() != time.UTC {
		t.Fatalf("reset Now = %v", got)
	}
}

func TestFakeClockConcurrent(t *testing.T) {
	c := NewFake(time.Unix(0, 0))
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				c.Advance(time.Millisecond)
				_ = c.Now()
			}
		}()
	}
	wg.Wait()
	if got := c.Now().UnixMilli(); got != 1000 {
		t.Fatalf("Now = %d ms; want 1000", got)
	}
}

func TestRealClockUTC(t *testing.T) {
	var c Clock = Real{}
	before := time.Now()
	got := c.Now()
	after := time.Now()
	if got.Location() != time.UTC || got.Before(before) || got.After(after) {
		t.Fatalf("Now = %v, want UTC between %v and %v", got, before, after)
	}
}
