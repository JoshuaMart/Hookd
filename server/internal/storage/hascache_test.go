package storage

import (
	"fmt"
	"testing"
	"time"
)

func TestHasCache_Disabled(t *testing.T) {
	for _, c := range []*hasCache{newHasCache(0, time.Minute), newHasCache(10, 0)} {
		c.add("a")
		if c.has("a") || c.len() != 0 {
			t.Error("a disabled cache must remember nothing")
		}
		c.forget([]string{"a"})
	}
}

func TestHasCache_Expiry(t *testing.T) {
	now := time.Unix(0, 0)
	c := newHasCache(10, time.Second)
	c.now = func() time.Time { return now }

	c.add("a")
	if !c.has("a") || c.has("b") {
		t.Fatal("expected only the added id")
	}
	now = now.Add(time.Second)
	if c.has("a") {
		t.Error("an entry must expire after its TTL")
	}
}

func TestHasCache_BoundedAndKeepsHotEntries(t *testing.T) {
	c := newHasCache(10, time.Hour)
	c.add("hot")
	for i := 0; i < 1000; i++ {
		c.add(fmt.Sprint(i))
		// A lookup moves the entry to the current generation.
		if !c.has("hot") {
			t.Fatalf("a frequently used entry was evicted after %d adds", i)
		}
	}
	if n := c.len(); n > 10 {
		t.Errorf("expected at most 10 entries, got %d", n)
	}
}

func TestHasCache_Forget(t *testing.T) {
	c := newHasCache(4, time.Hour)
	c.add("a")
	c.add("b")
	c.add("c") // a and b move to the previous generation
	c.forget([]string{"a", "c"})
	if c.has("a") || c.has("c") || !c.has("b") {
		t.Error("forget must drop ids from both generations, and only those")
	}
}
