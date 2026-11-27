package alerts

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/diegohh0411/zapply/internal/ats"
	"github.com/diegohh0411/zapply/internal/catalog"
	"github.com/diegohh0411/zapply/internal/logx"
)

// BoardFetcher reads a public job board; *ats.Client is the real one.
type BoardFetcher interface {
	Fetch(ctx context.Context, system, board string) (ats.Board, error)
}

// WithBoards adds the ATS board supplier, reading boards with f.
func (s *Service) WithBoards(f BoardFetcher) *Service {
	s.boards = f
	s.boardSlots = map[string]chan struct{}{}
	for _, a := range []string{ats.Greenhouse, ats.Lever, ats.Ashby} {
		s.boardSlots[a] = make(chan struct{}, boardsPerATS)
	}
	return s
}

// SupplierBoards is the board supplier's row in platform.supplier: it holds
// the interval every active board is polled at.
const SupplierBoards = "ats:boards"

const (
	// boardsPerTick bounds the polls one recovery tick queues.
	boardsPerTick = 200
	// boardsPerATS bounds the reads running at once against one ATS.
	boardsPerATS = 4
	// BaselineGap is how long a board may go unread before its next read is a
	// baseline again: what opened in between can't be told from what was open.
	BaselineGap = 72 * time.Hour
	// notFoundLimit disables a board its ATS has denied for this long.
	notFoundLimit = 7 * 24 * time.Hour
	// idleLimit disables a discovered board that opened nothing for this long.
	idleLimit = 60 * 24 * time.Hour
	// maxPostings bounds what one board read may add.
	maxPostings = 5000
	// ingestChunk bounds the sightings in one ingest job.
	ingestChunk = 50
	// maxDescription is what the catalog keeps of a description.
	maxDescription = 20000
)

// PollArgs reads one board. Tick makes each due slot its own unique job.
type PollArgs struct {
	ATS   string `json:"ats" river:"unique"`
	Board string `json:"board" river:"unique"`
	Tick  string `json:"tick" river:"unique"`
}

func (PollArgs) Kind() string { return "ats_board_poll" }

type pollWorker struct {
	river.WorkerDefaults[PollArgs]
	s *Service
}

func (w *pollWorker) Work(ctx context.Context, j *river.Job[PollArgs]) error {
	return w.s.Poll(ctx, j.Args.ATS, j.Args.Board)
}

// recoverBoards queues a poll for every active board that is due, moving its
// next poll in the same transaction. A little jitter spreads boards approved
// together across the interval.
func (s *Service) recoverBoards(ctx context.Context, client *river.Client[pgx.Tx]) error {
	if s.boards == nil {
		return nil
	}
	tick := time.Now().Truncate(time.Minute).Format(time.RFC3339)
	return pgx.BeginFunc(ctx, s.store.Pool(), func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE platform.ats_board b SET next_poll_at=now()+make_interval(mins=>i.m)*(1+random()*0.1)
 FROM (SELECT COALESCE((SELECT interval_minutes FROM platform.supplier WHERE name=$1),15) m) i,
 (SELECT ats,board FROM platform.ats_board WHERE status='active' AND next_poll_at<=now() ORDER BY next_poll_at LIMIT $2 FOR UPDATE SKIP LOCKED) d
 WHERE b.ats=d.ats AND b.board=d.board RETURNING b.ats,b.board`, SupplierBoards, boardsPerTick)
		if err != nil {
			return err
		}
		type due struct{ ats, board string }
		var ds []due
		for rows.Next() {
			var d due
			if err := rows.Scan(&d.ats, &d.board); err != nil {
				rows.Close()
				return err
			}
			ds = append(ds, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, d := range ds {
			if _, err := client.InsertTx(ctx, tx, PollArgs{ATS: d.ats, Board: d.board, Tick: tick}, &river.InsertOpts{MaxAttempts: 1,
				UniqueOpts: river.UniqueOpts{ByArgs: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending,
					rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}}); err != nil {
				return err
			}
		}
		return nil
	})
}

// Poll reads one active board and queues its postings the catalog doesn't
// hold yet for ingest, in the transaction that records the read.
//
// A board's first good read, or the first after BaselineGap without one, is
// its baseline: those postings were open before anyone was watching, so they
// are cataloged quiet and alert no one. A failed read backs off (doubling, at
// most a day); a board not found for a week, or a discovered one that opened
// nothing for 60 days, is disabled.
func (s *Service) Poll(ctx context.Context, system, board string) error {
	if s.boards == nil {
		return errors.New("board supplier isn't set up")
	}
	var source, company string
	var lastOK *time.Time
	err := s.store.Pool().QueryRow(ctx, `SELECT source,company,last_ok_at FROM platform.ats_board WHERE ats=$1 AND board=$2 AND status='active'`,
		system, board).Scan(&source, &company, &lastOK)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // disabled since it was queued
	}
	if err != nil {
		return err
	}
	if slot := s.boardSlots[system]; slot != nil {
		select {
		case slot <- struct{}{}:
			defer func() { <-slot }()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	read, fetchErr := s.boards.Fetch(ctx, system, board)
	if fetchErr != nil {
		notFound := errors.Is(fetchErr, ats.ErrNotFound)
		msg := "couldn't read the board"
		if notFound {
			msg = "the board wasn't found"
		}
		logx.From(ctx).WarnContext(ctx, "board read failed", "ats", system, "board", board, "error", fetchErr)
		_, err := s.store.Pool().Exec(ctx, `UPDATE platform.ats_board SET last_polled_at=now(),failures=failures+1,
 not_found_since=CASE WHEN $3 THEN COALESCE(not_found_since,now()) END,
 status=CASE WHEN $3 AND not_found_since<now()-$5::interval THEN 'disabled' ELSE status END,
 last_error=CASE WHEN $3 AND not_found_since<now()-$5::interval THEN 'not found for a week' ELSE $4 END,
 next_poll_at=now()+least(make_interval(mins=>COALESCE((SELECT interval_minutes FROM platform.supplier WHERE name=$6),15))*power(2,least(failures,6)),interval '1 day')
 WHERE ats=$1 AND board=$2`, system, board, notFound, msg, notFoundLimit.String(), SupplierBoards)
		return err
	}
	if company == "" {
		company = read.Company
	}
	if company == "" {
		company = ats.CompanyFromSlug(board)
	}
	postings := read.Postings
	if len(postings) > maxPostings {
		postings = postings[:maxPostings]
	}
	keys := make([]string, len(postings))
	for i, p := range postings {
		keys[i] = catalog.KindATS + ":" + system + ":" + board + ":" + p.Key
	}
	rows, err := s.store.Pool().Query(ctx, `SELECT key FROM platform.posting_identity WHERE key=ANY($1)`, keys)
	if err != nil {
		return err
	}
	known, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	held := make(map[string]bool, len(known))
	for _, k := range known {
		held[k] = true
	}
	baseline := lastOK == nil || time.Since(*lastOK) > BaselineGap
	var ss []catalog.Sighting
	for i, p := range postings {
		if held[keys[i]] || p.Key == "" {
			continue
		}
		ss = append(ss, catalog.Sighting{Source: catalog.SourceATS, ATSKey: system + ":" + board + ":" + p.Key, Company: clip(company, 200),
			Title: clip(p.Title, 500), Location: clip(p.Location, 500), URL: p.URL, Remote: p.Remote, PublishedAt: p.PublishedAt,
			Description: clip(p.Description, maxDescription), Quiet: baseline})
	}
	opened := len(ss) > 0 && !baseline
	return pgx.BeginFunc(context.WithoutCancel(ctx), s.store.Pool(), func(tx pgx.Tx) error {
		idle := `source='discovered' AND NOT $5 AND COALESCE(last_new_at,activated_at,created_at)<now()-$6::interval`
		if _, err := tx.Exec(ctx, `UPDATE platform.ats_board SET last_polled_at=now(),last_ok_at=now(),failures=0,not_found_since=NULL,
 open_count=$3,company=CASE WHEN company='' THEN $4 ELSE company END,last_new_at=CASE WHEN $5 THEN now() ELSE last_new_at END,
 status=CASE WHEN `+idle+` THEN 'disabled' ELSE status END,
 last_error=CASE WHEN `+idle+` THEN 'no new postings in 60 days' ELSE '' END
 WHERE ats=$1 AND board=$2`, system, board, len(read.Postings), clip(company, 200), opened, idleLimit.String()); err != nil {
			return err
		}
		if len(ss) == 0 {
			return nil
		}
		if s.queue == nil {
			return errors.New("ingest queue unavailable")
		}
		for start := 0; start < len(ss); start += ingestChunk {
			if err := s.queue.InsertTx(ctx, tx, IngestArgs{Sightings: ss[start:min(start+ingestChunk, len(ss))]}); err != nil {
				return err
			}
		}
		return nil
	})
}

// discoverBoards records the boards that search postings' links name, for
// the owner to approve. A board already known only counts the sighting, so a
// rejected one stays rejected.
func (s *Service) discoverBoards(ctx context.Context, ss []catalog.Sighting) error {
	type found struct {
		ats, board, company string
		n                   int
	}
	byKey := map[string]*found{}
	var order []string
	for _, sg := range ss {
		if sg.Source != catalog.SourceSearch {
			continue
		}
		system, board, err := ats.Parse(sg.URL)
		if err != nil {
			continue
		}
		k := system + ":" + strings.ToLower(board)
		if f, ok := byKey[k]; ok {
			f.n++
			continue
		}
		byKey[k] = &found{ats: system, board: board, company: clip(sg.Company, 200), n: 1}
		order = append(order, k)
	}
	if len(order) == 0 {
		return nil
	}
	var systems, boards, companies []string
	var counts []int32
	for _, k := range order {
		f := byKey[k]
		systems, boards, companies, counts = append(systems, f.ats), append(boards, f.board), append(companies, f.company), append(counts, int32(f.n))
	}
	_, err := s.store.Pool().Exec(ctx, `INSERT INTO platform.ats_board(ats,board,company,source,status,sightings)
 SELECT a,b,c,'discovered','pending',n FROM unnest($1::text[],$2::text[],$3::text[],$4::int[]) x(a,b,c,n)
 ON CONFLICT (ats,lower(board)) DO UPDATE SET sightings=ats_board.sightings+EXCLUDED.sightings,
 company=CASE WHEN ats_board.company='' THEN EXCLUDED.company ELSE ats_board.company END`, systems, boards, companies, counts)
	return err
}
