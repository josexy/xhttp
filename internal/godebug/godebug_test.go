package godebug

import (
	"sync"
	"testing"
)

func TestSettingValueUsesLastEntry(t *testing.T) {
	t.Setenv("GODEBUG", "http2client=1,other=x,http2client=0")
	if got, want := New("http2client").Value(), "0"; got != want {
		t.Fatalf("Value() = %q, want %q", got, want)
	}
}

func TestSettingValueMissing(t *testing.T) {
	t.Setenv("GODEBUG", "")
	if got := New("http2client").Value(); got != "" {
		t.Fatalf("Value() = %q, want empty string", got)
	}
}

func TestSettingValueStripsBisectPattern(t *testing.T) {
	t.Setenv("GODEBUG", "http2client=0#n10")
	if got, want := New("http2client").Value(), "0"; got != want {
		t.Fatalf("Value() = %q, want %q", got, want)
	}
}

func TestUndocumentedSetting(t *testing.T) {
	t.Setenv("GODEBUG", "custom=value")
	s := New("#custom")
	if !s.Undocumented() {
		t.Fatal("Undocumented() = false, want true")
	}
	if got, want := s.Name(), "custom"; got != want {
		t.Fatalf("Name() = %q, want %q", got, want)
	}
	if got, want := s.String(), "custom=value"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestIncNonDefaultConcurrent(t *testing.T) {
	s := New("http2client")
	const goroutines = 20
	const increments = 50
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range increments {
				s.IncNonDefault()
			}
		}()
	}
	wg.Wait()
	if got, want := s.nonDefault.Load(), uint64(goroutines*increments); got != want {
		t.Fatalf("non-default count = %d, want %d", got, want)
	}
}
