package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/diegohh0411/zapply/internal/joburl"
)

// Migration 6 repairs keys written before escaped paths were preserved. Use the
// same normalizer as job writes, and serialize against writes for the whole
// transaction so a mixed old/new key cannot race duplicate detection.
func backfillJobURLs(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, "LOCK TABLE job IN SHARE ROW EXCLUSIVE MODE"); err != nil {
		return err
	}
	// The schema owner may not have BYPASSRLS. Temporarily lift FORCE only
	// inside this transaction, restoring it before commit; runtime roles still
	// use the existing tenant policy throughout.
	if _, err := tx.ExecContext(ctx, "ALTER TABLE job NO FORCE ROW LEVEL SECURITY"); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id, url, normalized_url FROM job WHERE url <> ''")
	if err != nil {
		return err
	}
	type change struct{ id, normalized string }
	var changes []change
	for rows.Next() {
		var id, raw, previous string
		if err := rows.Scan(&id, &raw, &previous); err != nil {
			rows.Close()
			return err
		}
		normalized, err := joburl.Normalize(raw)
		if err != nil {
			rows.Close()
			return fmt.Errorf("normalize URL for job %s: %w", id, err)
		}
		if normalized != previous {
			changes = append(changes, change{id, normalized})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// Clear changed keys first to avoid transient conflicts between corrected keys.
	// A true duplicate still fails atomically; never discard or merge user data.
	for _, c := range changes {
		if _, err := tx.ExecContext(ctx, "UPDATE job SET normalized_url = '' WHERE id = $1", c.id); err != nil {
			return err
		}
	}
	for _, c := range changes {
		if _, err := tx.ExecContext(ctx, "UPDATE job SET normalized_url = $2 WHERE id = $1", c.id, c.normalized); err != nil {
			return fmt.Errorf("repair URL key for job %s: %w", c.id, err)
		}
	}
	_, err = tx.ExecContext(ctx, "ALTER TABLE job FORCE ROW LEVEL SECURITY")
	return err
}
