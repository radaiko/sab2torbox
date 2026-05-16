package job

import "testing"

func TestCanTransitionTo(t *testing.T) {
	cases := []struct {
		from, to State
		want     bool
	}{
		{StatePending, StateSubmitting, true},
		{StatePending, StateFailed, true},
		{StatePending, StateCompleted, false},
		{StateSubmitting, StateQueued, true},
		{StateQueued, StateDownloading, true},
		{StateQueued, StateCompleted, true},
		{StateDownloading, StateCompleted, true},
		{StateCompleted, StateImported, true},
		{StateImported, StateDeleted, true},
		{StateDownloading, StateFailed, true},
		{StateFailed, StateQueued, false},
		{StateDeleted, StatePending, false},
	}
	for _, c := range cases {
		if got := c.from.CanTransitionTo(c.to); got != c.want {
			t.Errorf("%s -> %s: got %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestNzoID(t *testing.T) {
	if got := (&Job{ID: 0}).NzoID(); got != "sab2tb_0" {
		t.Errorf("NzoID(0): got %q", got)
	}
	if got := (&Job{ID: 12345}).NzoID(); got != "sab2tb_12345" {
		t.Errorf("NzoID(12345): got %q", got)
	}
}

func TestIsTerminal(t *testing.T) {
	if !StateFailed.IsTerminal() || !StateDeleted.IsTerminal() {
		t.Error("failed and deleted must be terminal")
	}
	if StatePending.IsTerminal() || StateCompleted.IsTerminal() {
		t.Error("pending and completed must not be terminal")
	}
}

func TestHealTransitions(t *testing.T) {
	cases := []struct {
		from, to State
		want     bool
	}{
		{StateImported, StateHealing, true},
		{StateHealing, StateImported, true},
		{StateHealing, StateHealFailed, true},
		{StateHealFailed, StateHealing, true},
		{StateHealing, StateDeleted, false},
		{StateHealFailed, StateImported, false},
	}
	for _, c := range cases {
		if got := c.from.CanTransitionTo(c.to); got != c.want {
			t.Errorf("%s -> %s: got %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestManuallyResolvedTransitions(t *testing.T) {
	if !StateHealFailed.CanTransitionTo(StateManuallyResolved) {
		t.Error("heal_failed must be able to transition to manually_resolved")
	}
	if !StateManuallyResolved.IsTerminal() {
		t.Error("manually_resolved must be terminal")
	}
	if StateImported.CanTransitionTo(StateManuallyResolved) {
		t.Error("imported must not transition straight to manually_resolved")
	}
}
