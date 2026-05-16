package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type pingFunc struct {
	calls atomic.Int32
	err   error
}

func (p *pingFunc) Ping(context.Context) error {
	p.calls.Add(1)
	return p.err
}

func TestHealthCachesTorBoxPing(t *testing.T) {
	st := newAPITestStore(t)
	p := &pingFunc{}
	h := NewHealth(st, p, time.Minute)
	for i := 0; i < 3; i++ {
		if err := h.Check(context.Background()); err != nil {
			t.Fatalf("Check: %v", err)
		}
	}
	if got := p.calls.Load(); got != 1 {
		t.Errorf("expected ping cached to 1 call, got %d", got)
	}
}

func TestHealthReportsTorBoxFailure(t *testing.T) {
	st := newAPITestStore(t)
	h := NewHealth(st, &pingFunc{err: errors.New("token rejected")}, time.Minute)
	if err := h.Check(context.Background()); err == nil {
		t.Fatal("expected failure when torbox ping fails")
	}
}
