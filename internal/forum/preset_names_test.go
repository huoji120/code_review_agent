package forum

import (
	"fmt"
	"sync"
	"testing"
)

func TestPresetNamesRoundsAndConcurrentUniqueness(t *testing.T) {
	b := New(0)
	for i := 0; i < 65; i++ {
		id := fmt.Sprintf("a%d", i)
		b.Register(id, "audit")
		name := b.EnsurePresetName(id)
		base := presetNames[i%32]
		want := base
		if i >= 32 {
			want = fmt.Sprintf("%s%03d", base, i/32)
		}
		if name != want {
			t.Fatalf("member %d name=%q want=%q", i, name, want)
		}
	}
	var wg sync.WaitGroup
	for i := 65; i < 193; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("a%d", i)
			b.Register(id, "audit")
			first := b.EnsurePresetName(id)
			if first == "" || b.EnsurePresetName(id) != first {
				t.Error("name not stable")
			}
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := 0; i < 193; i++ {
		name := b.Name(fmt.Sprintf("a%d", i))
		if seen[name] {
			t.Fatalf("duplicate %q", name)
		}
		seen[name] = true
	}
}
func TestPresetAllocationHonorsHistoricalSuffixes(t *testing.T) {
	b := New(0)
	for i := 0; i < 32; i++ {
		id := fmt.Sprintf("old%d", i)
		b.Register(id, "audit")
		if err := b.RegisterName(id, presetNames[i]); err != nil {
			t.Fatal(err)
		}
	}
	b.Register("suffix", "audit")
	if err := b.RegisterName("suffix", presetNames[0]+"001"); err != nil {
		t.Fatal(err)
	}
	b.Register("new", "audit")
	if got := b.EnsurePresetName("new"); got != presetNames[1]+"001" {
		t.Fatalf("occupied suffix reused: %q", got)
	}
	if b.EnsurePresetName("suffix") != presetNames[0]+"001" {
		t.Fatal("historical name changed")
	}
}
