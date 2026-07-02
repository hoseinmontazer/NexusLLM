// Package replicaguard provides an atomic "claim a replica slot" helper that
// prevents concurrent callers from creating more agent_runtimes rows than the
// configured desired_replicas for a model.
//
// # Problem
//
// Every component that can spawn a container — the HA reconciler, the proxy
// activator, the admin DeployModel handler, and the idle-manager restore path
// — each performs a read-then-insert pattern:
//
//  1. Read: COUNT(non-terminal rows) for the model.
//  2. Decide: if count < desired, insert a new row.
//
// Because steps 1 and 2 are not atomic, a TOCTOU race exists: two goroutines
// (or two replicas of the gateway/admin process) can both read count=0, both
// decide "there is capacity", and both INSERT — producing 2 rows when only 1
// was desired.  Under load this compounds: a 30-second reconciler sweep fires
// while an in-flight activator has not yet committed its INSERT, so both spawn
// containers.
//
// # Solution
//
// ClaimSlot wraps both steps inside a single PostgreSQL transaction that first
// acquires a per-model advisory lock (pg_advisory_xact_lock), then does the
// count, and returns true only if count < desired.  The caller then performs
// the INSERT inside the same transaction.  The advisory lock is released when
// the transaction commits or rolls back, so no slot can be double-claimed.
//
// Usage pattern (callers MUST follow this exactly):
//
//	tx, err := db.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
//	if err != nil { ... }
//	defer tx.Rollback()
//
//	ok, err := replicaguard.ClaimSlot(ctx, tx, modelID, desiredReplicas)
//	if err != nil  { return err }
//	if !ok         { return ErrAtCapacity }
//
//	// INSERT agent_runtimes here, inside the same tx
//	_, err = tx.ExecContext(ctx, `INSERT INTO agent_runtimes ...`, ...)
//	if err != nil  { return err }
//
//	return tx.Commit()
package replicaguard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// ErrAtCapacity is returned by ClaimSlot when the model already has
// desired_replicas (or more) non-terminal rows in agent_runtimes.
var ErrAtCapacity = errors.New("replica slot unavailable: model already at desired_replicas capacity")

// ClaimSlot atomically checks whether a new replica slot is available for
// modelID and, if so, acquires the slot by holding the advisory lock for the
// duration of tx.
//
//   - Returns (true, nil)  when count(non-terminal, non-draining) < desiredReplicas + maxSurge.
//     The caller MUST insert the new agent_runtimes row and commit tx.
//   - Returns (false, nil) when the model is already at capacity.
//     The caller MUST NOT insert and should rollback or return ErrAtCapacity.
//   - Returns (false, err) on any DB error.
//
// maxSurge (≥1) controls how many extra replicas are allowed during rolling
// replacement. Pass 1 for normal operation; pass max_surge from model_replica_specs
// for rolling replacement.
//
// tx MUST be an open, uncommitted transaction.  The advisory lock is held until
// tx is committed or rolled back — do not re-use tx for other unrelated work.
func ClaimSlot(ctx context.Context, tx *sqlx.Tx, modelID string, desiredReplicas int, maxSurge ...int) (bool, error) {
	surge := 1
	if len(maxSurge) > 0 && maxSurge[0] > 1 {
		surge = maxSurge[0]
	}
	var available bool
	err := tx.QueryRowContext(ctx,
		`SELECT claim_replica_slot($1::uuid, $2, $3)`,
		modelID, desiredReplicas, surge,
	).Scan(&available)
	if err != nil {
		return false, fmt.Errorf("claim_replica_slot(%s, %d, %d): %w", modelID, desiredReplicas, surge, err)
	}
	return available, nil
}

// DesiredReplicas fetches the configured desired_replicas for a model from the
// DB.  Returns 1 if the model has no model_replica_specs row (the safe default).
// Uses a plain *sqlx.DB so callers that don't have an open transaction can still
// call it before opening one.
func DesiredReplicas(ctx context.Context, db *sqlx.DB, modelID string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT desired_replicas($1::uuid)`, modelID,
	).Scan(&n)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 1, nil
		}
		return 1, fmt.Errorf("desired_replicas(%s): %w", modelID, err)
	}
	if n <= 0 {
		return 1, nil
	}
	return n, nil
}
