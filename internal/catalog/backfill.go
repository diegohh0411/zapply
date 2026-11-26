package catalog

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BackfillResult is what one backfill run did.
type BackfillResult struct {
	Members  int // members whose alerts were all read
	Alerts   int // alerts read
	Postings int // new postings they added
}

// Backfill adds the postings members were already alerted about. Each batch
// reads one member's next alerts and moves their cursor in the same
// transaction, so a run stopped anywhere resumes where it was and a second
// run reads nothing twice; resolving a posting already held only records
// another sighting.
func (c *Catalog) Backfill(ctx context.Context, batch int) (BackfillResult, error) {
	var res BackfillResult
	if batch <= 0 {
		batch = 200
	}
	var after *uuid.UUID
	for {
		var members []uuid.UUID
		if err := c.store.Pool().QueryRow(ctx, `SELECT platform.watching_members($1,200)`, after).Scan(&members); err != nil {
			return res, err
		}
		if len(members) == 0 {
			return res, nil
		}
		for _, u := range members {
			for {
				done, read, created, err := c.backfillBatch(ctx, u, batch)
				if err != nil {
					return res, err
				}
				res.Alerts += read
				res.Postings += created
				if done {
					res.Members++
					break
				}
			}
		}
		last := members[len(members)-1]
		after = &last
	}
}

// backfillBatch reads up to batch of u's alerts after their cursor.
func (c *Catalog) backfillBatch(ctx context.Context, u uuid.UUID, batch int) (done bool, read, created int, err error) {
	err = c.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		done, read, created = false, 0, 0
		var cursor *uuid.UUID
		var finished bool
		// The upsert locks the member's row, so two runs cannot read the same
		// batch at once.
		e := tx.QueryRow(ctx, `INSERT INTO platform.posting_backfill(user_id) VALUES($1) ON CONFLICT(user_id) DO UPDATE SET updated_at=posting_backfill.updated_at
 RETURNING after_id,done`, u).Scan(&cursor, &finished)
		if e != nil {
			return e
		}
		if finished {
			done = true
			return nil
		}
		rows, e := tx.Query(ctx, `SELECT a.id,a.posting_key,a.company,a.title,a.location,a.url,a.remote,a.published_at,w.kind,COALESCE(w.ats,''),COALESCE(w.board,'')
 FROM job_alert a JOIN job_watch w ON w.user_id=a.user_id AND w.id=a.watch_id
 WHERE a.user_id=$1 AND ($2::uuid IS NULL OR a.id>$2) ORDER BY a.id LIMIT $3`, u, cursor, batch)
		if e != nil {
			return e
		}
		var ss []Sighting
		var last uuid.UUID
		for rows.Next() {
			var s Sighting
			var key, kind, system, board string
			var published *time.Time
			if e = rows.Scan(&last, &key, &s.Company, &s.Title, &s.Location, &s.URL, &s.Remote, &published, &kind, &system, &board); e != nil {
				rows.Close()
				return e
			}
			s.Source, s.PublishedAt = SourceAlert, published
			if kind == "board" && system != "" && board != "" {
				s.ATSKey = system + ":" + board + ":" + key
			}
			ss = append(ss, s)
		}
		rows.Close()
		if e = rows.Err(); e != nil {
			return e
		}
		for _, s := range ss {
			_, isNew, e := resolve(ctx, tx, s)
			if e != nil {
				return e
			}
			if isNew {
				created++
			}
		}
		read = len(ss)
		done = read < batch
		if read > 0 {
			if _, e = tx.Exec(ctx, `UPDATE platform.posting_backfill SET after_id=$2,done=$3,updated_at=now() WHERE user_id=$1`, u, last, done); e != nil {
				return e
			}
		} else if _, e = tx.Exec(ctx, `UPDATE platform.posting_backfill SET done=true,updated_at=now() WHERE user_id=$1`, u); e != nil {
			return e
		}
		if created > 0 && c.queue != nil {
			return c.queue.InsertTx(ctx, tx, SweepArgs{})
		}
		return nil
	})
	return done, read, created, err
}
