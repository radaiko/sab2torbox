// Package worker — healer loop (filled in across the heal tasks).
package worker

import "context"

func (w *Workers) healOnce(_ context.Context) error          { return nil }
func (w *Workers) healReconcileOnce(_ context.Context) error { return nil }
