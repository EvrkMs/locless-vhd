package main

import (
	"strings"
	"testing"
	"time"
)

func TestSpaceWatch(t *testing.T) {
	var s spaceWatch
	t0 := time.Date(2026, 11, 14, 20, 0, 0, 0, time.UTC)
	steps := []struct {
		free  uint64
		after time.Duration
		says  string // part of the message, "" for none
	}{
		{20 << 30, 0, ""},
		{4 << 30, time.Minute, "заканчивается"}, // the first warning
		{3 << 30, 2 * time.Minute, ""},          // once is enough
		{1 << 30, 3 * time.Minute, "перезагрузите"},
		{1 << 30, 4 * time.Minute, ""},              // a minute later: too soon
		{1 << 30, 8 * time.Minute, "перезагрузите"}, // five minutes later: again
		{5<<30 + 512<<20, 9 * time.Minute, ""},      // a little freed: no new warning
		{7 << 30, 10 * time.Minute, ""},             // fine again
		{4 << 30, 11 * time.Minute, "заканчивается"},
	}
	for i, st := range steps {
		got := s.check(st.free, t0.Add(st.after))
		if (st.says == "") != (got == "") || !strings.Contains(got, st.says) {
			t.Fatalf("step %d, %d GiB: got %q, want %q", i, st.free>>30, got, st.says)
		}
	}
}
