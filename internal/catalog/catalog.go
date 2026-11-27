// Package catalog keeps every posting the platform sees once, whatever found
// it, and labels each with what it is: seniority, area, countries, languages
// and the like (migration 00043, docs/parts/11-posting-catalog.md).
//
// The board and search polls add their postings (Add) after filing alerts,
// so delivering alerts is unchanged. A posting is resolved to an existing
// one by its identities (Keys): its ATS posting, canonical URL, and company,
// title and country. New postings wait for classification, which the sweep
// sends through the worker as two model operations charged to the catalog's
// own member (SystemMember).
package catalog

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/diegohh0411/zapply/internal/logx"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/diegohh0411/zapply/internal/worker"
)

// SystemMember is the catalog's member (migration 00043): its ledger pays for
// classification and its tenant holds the model operations.
var SystemMember = uuid.MustParse("00000000-0000-7000-8000-0000000ca7a1")

// Sources.
const (
	SourceATS    = "ats"
	SourceSearch = "search"
	SourceAlert  = "alert"
)

// Sighting is one posting as a source saw it.
type Sighting struct {
	Source   string
	ATSKey   string // <ats>:<board>:<posting>, for board postings
	Company  string
	Title    string
	Location string
	URL      string
	Remote   bool
	// Country is the source's ISO 3166-1 alpha-2 code, when it gives one.
	Country       string
	PublishedAt   *time.Time
	Description   string
	SeniorityHint string
	// Quiet stores a new posting as already routed, so it alerts no one: a
	// board's baseline is what was open before anyone was watching it.
	Quiet bool
}

// valid reports whether the catalog can store s.
func (s Sighting) valid() bool {
	return strings.HasPrefix(s.URL, "https://") && len(s.URL) <= 2048 && strings.TrimSpace(s.Title) != ""
}

type Catalog struct {
	store *store.Store
	queue *worker.Queue
	log   *slog.Logger
	// Labeled runs in the transaction that finishes a posting's
	// classification stage, so whatever waits on labels (alert routing) wakes.
	Labeled func(ctx context.Context, tx pgx.Tx) error
}

// New returns a catalog writing through st. Without a queue postings are
// stored but nothing wakes the classifier until its next sweep.
func New(st *store.Store, q *worker.Queue, log *slog.Logger) *Catalog {
	return &Catalog{store: st, queue: q, log: log}
}

func (c *Catalog) logger(ctx context.Context) *slog.Logger {
	if c.log != nil {
		return c.log
	}
	return logx.From(ctx)
}

// chunk bounds the postings one transaction resolves, and so the advisory
// locks it holds.
const chunk = 100

// Add stores each sighting as a posting, or as another sighting of one it
// already holds, and wakes the classifier when any is new. It returns the
// posting each sighting resolved to (uuid.Nil for one it cannot store).
func (c *Catalog) Add(ctx context.Context, ss []Sighting) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, len(ss))
	for start := 0; start < len(ss); start += chunk {
		end := min(start+chunk, len(ss))
		err := pgx.BeginFunc(ctx, c.store.Pool(), func(tx pgx.Tx) error {
			created := false
			for i := start; i < end; i++ {
				id, isNew, err := resolve(ctx, tx, ss[i])
				if err != nil {
					return err
				}
				ids[i], created = id, created || isNew
			}
			if created && c.queue != nil {
				return c.queue.InsertTx(ctx, tx, SweepArgs{})
			}
			return nil
		})
		if err != nil {
			return ids, err
		}
	}
	return ids, nil
}

// resolve finds the posting s is a sighting of, or creates it, and records
// its identities. Identities are locked first, in a fixed order, so two
// sightings of one new posting cannot both create it.
func resolve(ctx context.Context, tx pgx.Tx, s Sighting) (uuid.UUID, bool, error) {
	if !s.valid() {
		return uuid.Nil, false, nil
	}
	s = s.clipped()
	keys := s.Keys()
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(h) FROM (SELECT hashtextextended('posting_identity:'||k,0) h FROM unnest($1::text[]) k ORDER BY 1) l`, sorted); err != nil {
		return uuid.Nil, false, err
	}
	fromATS := s.ATSKey != ""
	type held struct {
		posting uuid.UUID
		fromATS bool
		fresh   bool
	}
	found := map[string]held{}
	rows, err := tx.Query(ctx, `SELECT key,posting_id,from_ats,seen_at>now()-$2::interval FROM platform.posting_identity WHERE key=ANY($1)`, keys, FingerprintTTL.String())
	if err != nil {
		return uuid.Nil, false, err
	}
	for rows.Next() {
		var k string
		var h held
		if err := rows.Scan(&k, &h.posting, &h.fromATS, &h.fresh); err != nil {
			rows.Close()
			return uuid.Nil, false, err
		}
		found[k] = h
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return uuid.Nil, false, err
	}
	id := uuid.Nil
	for _, k := range keys {
		h, ok := found[k]
		if !ok {
			continue
		}
		// A fingerprint is the weakest identity: it lapses, and two ATS board
		// postings sharing one are two openings with the same title.
		if kindOf(k) == KindFP && (!h.fresh || (fromATS && h.fromATS)) {
			continue
		}
		id = h.posting
		break
	}
	created := id == uuid.Nil
	country := nullable(s.country())
	if created {
		id = uuid.Must(uuid.NewV7())
		if _, err := tx.Exec(ctx, `INSERT INTO platform.posting(id,source,company,title,location,url,remote,country,published_at,description,seniority_hint,routed_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,CASE WHEN $12 THEN now() END)`, id, s.Source, s.Company, s.Title, s.Location, s.URL, s.Remote, country, s.PublishedAt,
			nullable(s.Description), nullable(s.SeniorityHint), s.Quiet); err != nil {
			return uuid.Nil, false, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO platform.posting_stage(posting_id,stage) VALUES($1,'decision'),($1,'extraction')`, id); err != nil {
			return uuid.Nil, false, err
		}
	} else {
		// A later sighting fills in what earlier ones lacked; it never
		// overwrites what a source already gave.
		if _, err := tx.Exec(ctx, `UPDATE platform.posting SET last_seen_at=now(),country=COALESCE(country,$2),published_at=COALESCE(published_at,$3),
 description=COALESCE(description,$4),seniority_hint=COALESCE(seniority_hint,$5),company=CASE WHEN company='' THEN $6 ELSE company END WHERE id=$1`,
			id, country, s.PublishedAt, nullable(s.Description), nullable(s.SeniorityHint), s.Company); err != nil {
			return uuid.Nil, false, err
		}
	}
	// A key another posting holds stays with it, unless it is a lapsed
	// fingerprint: then this sighting is the new opening it names.
	if _, err := tx.Exec(ctx, `INSERT INTO platform.posting_identity(key,posting_id,from_ats) SELECT k,$2,$3 FROM unnest($1::text[]) k
 ON CONFLICT(key) DO UPDATE SET posting_id=EXCLUDED.posting_id,from_ats=EXCLUDED.from_ats OR (posting_identity.posting_id=EXCLUDED.posting_id AND posting_identity.from_ats),seen_at=now()
 WHERE posting_identity.posting_id=EXCLUDED.posting_id OR (left(posting_identity.key,3)='fp:' AND posting_identity.seen_at<=now()-$4::interval)`,
		keys, id, fromATS, FingerprintTTL.String()); err != nil {
		return uuid.Nil, false, err
	}
	return id, created, nil
}

// clipped bounds s to what the catalog's columns hold.
func (s Sighting) clipped() Sighting {
	s.Company = clip(strings.TrimSpace(s.Company), 200)
	s.Title = clip(strings.TrimSpace(s.Title), 500)
	s.Location = clip(strings.TrimSpace(s.Location), 500)
	s.Description = clip(strings.TrimSpace(s.Description), 20000)
	s.SeniorityHint = clip(strings.TrimSpace(s.SeniorityHint), 50)
	if s.Source == "" {
		s.Source = SourceAlert
	}
	return s
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
