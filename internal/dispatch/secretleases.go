package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/idgen"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/secretsource"
)

const (
	// secretLeaseSweepInterval is how often a control node checks the recorded secrets of runs for
	// claims that have ended. It matches the relay's own check of what it holds in memory.
	secretLeaseSweepInterval = 30 * time.Second
	// secretLeaseSweepBatch bounds how many records one sweep reads. A record stands for a secret
	// out with a running run, so the bound is far above any real fleet's count.
	secretLeaseSweepBatch = 5000
	// secretLeaseMaxBackoff caps the wait between tries of a revoke that keeps failing.
	secretLeaseMaxBackoff = time.Hour
)

// runLeases collects what opening one claimed run's secrets minted, records a revoke handle for
// each secret whose engine gives one, and hands every secret back when the claim ends. It serves a
// run this process executes and a relay run whose secrets this control node opens for a worker.
//
// Both used to keep only the revoke funcs, in the memory of the process that minted them, so a
// process that stopped, crashed, or restarted while the run was out forgot them and the secrets
// lived out their engine's TTL. A recorded handle lets any replica's sweep revoke the secret once
// the claim ends, whichever process minted it and whether or not that process is still running.
type runLeases struct {
	// d is the dispatcher that opened the secrets.
	d *Dispatcher
	// r is the run they were opened for, nil for secrets opened outside any claim.
	r *run.Run
	// minted is every secret minted so far.
	minted []mintedLease
}

// mintedLease is one secret a run's opening minted.
type mintedLease struct {
	// lease revokes the secret from this process.
	lease *secretsource.Lease
	// record is the secret's recorded revoke handle, nil when none was recorded.
	record *run.SecretLease
}

// add keeps lease, minted from the source of credential credentialID, and records its revoke handle
// when its engine gives one and the store keeps records. The lease is kept even when the record
// fails, so the release still revokes it, and the failure is returned, because a secret no other
// process could revoke is neither delivered nor handed to a tool.
func (l *runLeases) add(ctx context.Context, credentialID string, lease *secretsource.Lease) error {
	m := mintedLease{lease: lease}
	err := l.record(ctx, credentialID, &m)
	l.minted = append(l.minted, m)
	return err
}

// record seals and saves the revoke handle of m's lease, setting m.record when one is saved.
//
// Only a secret opened under a claim is recorded, because the claim is what a sweep checks to tell
// a secret still in use from one left behind. A secret opened with no claim, as the plan gate opens
// one to plan a run at submission, may belong to a run not stored yet, which a sweep would read as
// gone and revoke mid-plan. It is held for one bounded scan and revoked from memory when the scan
// ends.
func (l *runLeases) record(ctx context.Context, credentialID string, m *mintedLease) error {
	handle, expires := m.lease.Handle()
	keeper, ok := l.d.store.(run.SecretLeases)
	if handle == "" || !ok || l.r == nil || l.r.ClaimSecret == "" {
		return nil
	}
	sealed, err := l.d.sealer.Seal(handle)
	if err != nil {
		return fmt.Errorf("record how to revoke a minted secret: %w", err)
	}
	rec := &run.SecretLease{
		ID: idgen.New("slease_", 12), RunID: l.r.ID, ClaimHash: run.ClaimHash(l.r.ClaimSecret),
		CredentialID: credentialID, Kind: m.lease.Kind(), Handle: sealed, ExpiresAt: expires,
		CreatedAt: time.Now(),
	}
	if err := keeper.SaveSecretLease(ctx, rec); err != nil {
		return fmt.Errorf("record how to revoke a minted secret: %w", err)
	}
	m.record = rec
	return nil
}

// empty reports whether nothing was minted.
func (l *runLeases) empty() bool {
	return len(l.minted) == 0
}

// release revokes every secret minted and deletes its record. A record another process already took
// means that process revoked the secret, so it is not revoked twice. A revoke that fails leaves its
// record in place for a later sweep to try again.
func (l *runLeases) release() {
	keeper, _ := l.d.store.(run.SecretLeases)
	for _, m := range l.minted {
		if m.record == nil || keeper == nil {
			l.d.revokeLease(m.lease)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), revokeTimeout)
		taken, err := keeper.TakeSecretLease(ctx, m.record.ID)
		if err == nil && !taken {
			cancel()
			continue
		}
		if rerr := m.lease.Revoke(ctx); rerr != nil {
			l.d.revokeFailed(ctx, keeper, m.record, rerr, time.Now())
		}
		cancel()
	}
}

// secretLeaseLoop sweeps the recorded secrets of runs until the dispatcher closes: once as it
// starts, which on a restarted control node is the first moment anything can, and on an interval
// after that.
func (d *Dispatcher) secretLeaseLoop(keeper run.SecretLeases) {
	defer d.wg.Done()
	ticker := time.NewTicker(secretLeaseSweepInterval)
	defer ticker.Stop()
	for {
		d.sweepSecretLeases(d.ctx, keeper, time.Now())
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// sweepSecretLeases revokes each recorded secret whose claim has ended: its run is gone or
// finished, or the run is no longer held under the claim the secret was opened for, because the
// process executing it, or the relay worker it was delivered to, died and the run was requeued,
// interrupted, or claimed again. A run still executing under that claim keeps its secrets, wherever
// it runs. A secret past its own expiry is dropped without a revoke, since its engine has already
// ended it. Each record is taken before
// its secret is revoked, so of two replicas sweeping together, one revokes it, and a record whose
// handle this process cannot open is left for a process that can.
func (d *Dispatcher) sweepSecretLeases(ctx context.Context, keeper run.SecretLeases, now time.Time) {
	records, err := keeper.ListSecretLeases(ctx, secretLeaseSweepBatch)
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("dispatch: list recorded secret leases: " + err.Error())
		}
		return
	}
	for _, rec := range records {
		if ctx.Err() != nil {
			return
		}
		expired := !now.Before(rec.ExpiresAt)
		if !expired && now.Before(rec.RetryAt) {
			continue
		}
		// A record that failed a revoke was taken from a claim already over, so it is not checked
		// again.
		if !expired && rec.Attempts == 0 {
			ended, cerr := d.claimEnded(ctx, rec)
			if cerr != nil || !ended {
				continue
			}
		}
		handle := ""
		if !expired {
			opened, oerr := d.openRecorded(rec)
			if oerr != nil {
				continue
			}
			handle = opened
		}
		taken, terr := keeper.TakeSecretLease(ctx, rec.ID)
		if terr != nil || !taken || expired {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, revokeTimeout)
		if rerr := d.revokeRecorded(rctx, rec, handle); rerr != nil {
			d.revokeFailed(rctx, keeper, rec, rerr, now)
		}
		cancel()
	}
}

// openRecorded opens a record's sealed handle. It fails when this process holds no credential key
// or another one, and the record is then left for a process holding the key it was sealed with.
func (d *Dispatcher) openRecorded(rec *run.SecretLease) (string, error) {
	if d.sealer == nil || !d.sealer.Enabled() {
		return "", credential.ErrNoKey
	}
	return d.sealer.Open(rec.Handle)
}

// claimEnded reports whether the claim a recorded secret was delivered under is over.
func (d *Dispatcher) claimEnded(ctx context.Context, rec *run.SecretLease) (bool, error) {
	stored, err := d.store.Get(ctx, rec.RunID)
	switch {
	case errors.Is(err, run.ErrNotFound):
		return true, nil
	case err != nil:
		return false, err
	case stored.Status.Terminal():
		return true, nil
	}
	return run.ClaimHash(stored.ClaimSecret) != rec.ClaimHash, nil
}

// revokeRecorded ends a recorded secret from its opened handle, reading again the configuration of
// the credential that minted it for what the engine needs besides the handle. A failure that no
// retry can fix wraps secretsource.ErrLeaseHandle.
func (d *Dispatcher) revokeRecorded(ctx context.Context, rec *run.SecretLease, handle string) error {
	config, err := d.openSourceConfig(ctx, rec.CredentialID)
	switch {
	case errors.Is(err, credential.ErrNotFound), errors.Is(err, credential.ErrNoKey):
		return fmt.Errorf("%w: %w", secretsource.ErrLeaseHandle, err)
	case err != nil:
		return err
	}
	return secretsource.RevokeHandle(ctx, rec.Kind, handle, config)
}

// revokeFailed records a failed revoke of rec so a later sweep tries again, backing off between
// tries, until the secret would have expired anyway. A failure no retry can fix is dropped. Only
// the first failure and the giving up are logged, and never the handle.
func (d *Dispatcher) revokeFailed(ctx context.Context, keeper run.SecretLeases, rec *run.SecretLease,
	cause error, now time.Time) {
	log := d.log.With(zap.String("run_id", rec.RunID), zap.String("engine", rec.Kind))
	if errors.Is(cause, secretsource.ErrLeaseHandle) {
		log.Warn("dispatch: a minted secret cannot be revoked and expires on its own: " +
			cause.Error())
		return
	}
	rec.Attempts++
	backoff := secretLeaseSweepInterval << min(rec.Attempts-1, 7)
	rec.RetryAt = now.Add(min(backoff, secretLeaseMaxBackoff))
	if !rec.RetryAt.Before(rec.ExpiresAt) {
		log.Warn("dispatch: gave up revoking a minted secret, which expires on its own: "+
			cause.Error(), zap.Int("attempts", rec.Attempts), zap.Time("expires_at", rec.ExpiresAt))
		return
	}
	if rec.Attempts == 1 {
		log.Warn("dispatch: revoke a minted secret failed, will retry: " + cause.Error())
	}
	// The record is saved on a deadline of its own, because the revoke that failed may have failed
	// by running out of the caller's, and a save on that would lose the record with it.
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
	defer cancel()
	if err := keeper.SaveSecretLease(save, rec); err != nil {
		log.Warn("dispatch: keep a minted secret's handle for another try: " + err.Error())
	}
}
