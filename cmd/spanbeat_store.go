package cmd

import (
	"context"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/spanbeat"
)

// auditBeatStore adapts an audit store to the spanbeat.Store the emitter appends through, mapping the
// chain entry it writes to the minimal beat the emitter needs. The clock-behind error passes through
// unchanged, since it already carries the ClockBehind method the emitter detects.
type auditBeatStore struct {
	// store is the audit chain beats are appended to.
	store audit.Store
}

// AppendSpanBeat writes one beat to the chain and reports the time, seq, hash, and number the emitter
// logs and anchors.
func (a auditBeatStore) AppendSpanBeat(
	ctx context.Context, at time.Time, cadenceSeconds int,
) (spanbeat.AppendedBeat, error) {
	e, err := a.store.AppendSpanBeat(ctx, at, cadenceSeconds)
	if err != nil {
		return spanbeat.AppendedBeat{}, err
	}
	beat, _, _, _ := audit.ParseSpanPath(e.Path)
	return spanbeat.AppendedBeat{At: e.At, Seq: e.Seq, Hash: e.Hash, Beat: beat}, nil
}
