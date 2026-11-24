// Package alerts tells members about new openings on the job boards they
// watch, within PollInterval of publication, so they can apply among the first.
//
// A watch names an employer's board on a hosted ATS (see package ats) plus
// optional filters. The worker polls each watched board once, however many
// members watch it, and the database decides what is new and who it matches
// (migration 00037); this package fetches, files the alerts and manages watches.
package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/diegohh0411/zapply/internal/ats"
	"github.com/diegohh0411/zapply/internal/library"
	"github.com/diegohh0411/zapply/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PollInterval is how often each watched board is read.
const PollInterval = 30 * time.Minute

// Fetcher reads a board; *ats.Client is the real one.
type Fetcher interface {
	Fetch(ctx context.Context, system, board string) (ats.Board, error)
}

type Service struct {
	store *store.Store
	ats   Fetcher
}

func New(st *store.Store, f Fetcher) *Service {
	if f == nil {
		f = &ats.Client{}
	}
	return &Service{store: st, ats: f}
}

// Watch is one board a member follows.
type Watch struct {
	ID            uuid.UUID  `json:"id"`
	ATS           string     `json:"ats"`
	Board         string     `json:"board"`
	BoardURL      string     `json:"board_url"`
	Company       string     `json:"company"`
	Keywords      []string   `json:"keywords"`
	Locations     []string   `json:"locations"`
	RemoteOnly    bool       `json:"remote_only"`
	Paused        bool       `json:"paused"`
	LastCheckedAt *time.Time `json:"last_checked_at"`
	LastError     string     `json:"last_error"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Alert is one new posting that matched a watch.
type Alert struct {
	ID          uuid.UUID  `json:"id"`
	WatchID     uuid.UUID  `json:"watch_id"`
	Company     string     `json:"company"`
	Title       string     `json:"title"`
	Location    string     `json:"location"`
	URL         string     `json:"url"`
	Remote      bool       `json:"remote"`
	PublishedAt *time.Time `json:"published_at"`
	FoundAt     time.Time  `json:"found_at"`
}

// Input creates a watch (URL) or changes one (nil fields stay as they are).
type Input struct {
	URL        string
	Company    *string
	Keywords   *[]string
	Locations  *[]string
	RemoteOnly *bool
	Paused     *bool
}

// MaxWatches bounds one member's watches.
const MaxWatches = 100

func invalid(field, msg string) error {
	return &library.ValidationError{Fields: []library.FieldError{{Field: field, Message: msg}}}
}

// terms trims, drops blanks and duplicates, and bounds a filter list.
func terms(field string, in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.Join(strings.Fields(t), " ")
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		if utf8.RuneCountInString(t) > 100 {
			return nil, invalid(field, "each term is at most 100 characters")
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	if len(out) > 20 {
		return nil, invalid(field, "at most 20 terms")
	}
	return out, nil
}

func company(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > 200 {
		return "", invalid("company", "must be 1 to 200 characters")
	}
	return s, nil
}

const watchColumns = `id,ats,board,company,keywords,locations,remote_only,paused,created_at`

func scanWatch(row pgx.Row) (Watch, error) {
	var w Watch
	err := row.Scan(&w.ID, &w.ATS, &w.Board, &w.Company, &w.Keywords, &w.Locations, &w.RemoteOnly, &w.Paused, &w.CreatedAt)
	w.BoardURL = ats.BoardURL(w.ATS, w.Board)
	return w, err
}

type boardHealth struct {
	ATS       string     `json:"ats"`
	Board     string     `json:"board"`
	LastOKAt  *time.Time `json:"last_ok_at"`
	LastError string     `json:"last_error"`
}

// withHealth adds each board's last poll to the watches.
func withHealth(ctx context.Context, tx pgx.Tx, ws []Watch) error {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT platform.my_ats_boards()`).Scan(&raw); err != nil {
		return err
	}
	var hs []boardHealth
	if err := json.Unmarshal(raw, &hs); err != nil {
		return err
	}
	by := map[string]boardHealth{}
	for _, h := range hs {
		by[h.ATS+"/"+h.Board] = h
	}
	for i := range ws {
		if h, ok := by[ws[i].ATS+"/"+ws[i].Board]; ok {
			ws[i].LastCheckedAt, ws[i].LastError = h.LastOKAt, h.LastError
		}
	}
	return nil
}

// Watches lists the member's watches, newest first.
func (s *Service) Watches(ctx context.Context, u uuid.UUID) ([]Watch, error) {
	out := []Watch{}
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+watchColumns+` FROM job_watch WHERE user_id=$1 ORDER BY created_at DESC,id DESC`, u)
		if err != nil {
			return err
		}
		for rows.Next() {
			w, err := scanWatch(rows)
			if err != nil {
				rows.Close()
				return err
			}
			out = append(out, w)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		return withHealth(ctx, tx, out)
	})
	return out, err
}

func (s *Service) get(ctx context.Context, tx pgx.Tx, u, id uuid.UUID) (Watch, error) {
	w, err := scanWatch(tx.QueryRow(ctx, `SELECT `+watchColumns+` FROM job_watch WHERE user_id=$1 AND id=$2`, u, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return w, library.ErrNotFound
	}
	if err != nil {
		return w, err
	}
	ws := []Watch{w}
	err = withHealth(ctx, tx, ws)
	return ws[0], err
}

// Create starts watching the board a careers link names. It reads the board
// once, so a mistyped slug fails here rather than silently never alerting.
func (s *Service) Create(ctx context.Context, u uuid.UUID, in Input) (Watch, error) {
	system, board, err := ats.Parse(in.URL)
	if err != nil {
		return Watch{}, invalid("url", "paste a Greenhouse, Lever or Ashby careers link, like jobs.lever.co/acme")
	}
	keywords, err := terms("keywords", deref(in.Keywords))
	if err != nil {
		return Watch{}, err
	}
	locations, err := terms("locations", deref(in.Locations))
	if err != nil {
		return Watch{}, err
	}
	var name string
	if in.Company != nil && strings.TrimSpace(*in.Company) != "" {
		if name, err = company(*in.Company); err != nil {
			return Watch{}, err
		}
	}
	read, err := s.ats.Fetch(ctx, system, board)
	switch {
	case errors.Is(err, ats.ErrNotFound):
		return Watch{}, invalid("url", "that job board doesn't exist; check the link")
	case err != nil:
		return Watch{}, invalid("url", "couldn't read that job board right now; try again")
	}
	if name == "" {
		name = strings.TrimSpace(read.Company)
		if name == "" || utf8.RuneCountInString(name) > 200 {
			name = ats.CompanyFromSlug(board)
		}
	}
	var w Watch
	err = s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM job_watch WHERE user_id=$1`, u).Scan(&n); err != nil {
			return err
		}
		if n >= MaxWatches {
			return invalid("url", "you can watch at most 100 job boards")
		}
		id := uuid.Must(uuid.NewV7())
		_, err := tx.Exec(ctx, `INSERT INTO job_watch(id,user_id,ats,board,company,keywords,locations,remote_only) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			id, u, system, board, name, keywords, locations, in.RemoteOnly != nil && *in.RemoteOnly)
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return &library.ConflictError{Message: "you already watch this job board"}
		}
		if err != nil {
			return err
		}
		w, err = s.get(ctx, tx, u, id)
		return err
	})
	return w, err
}

// Update changes a watch's name, filters or pause.
func (s *Service) Update(ctx context.Context, u, id uuid.UUID, in Input) (Watch, error) {
	sets, args := []string{}, []any{u, id}
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, col+"=$"+strconv.Itoa(len(args)))
	}
	if in.Company != nil {
		name, err := company(*in.Company)
		if err != nil {
			return Watch{}, err
		}
		add("company", name)
	}
	if in.Keywords != nil {
		k, err := terms("keywords", *in.Keywords)
		if err != nil {
			return Watch{}, err
		}
		add("keywords", k)
	}
	if in.Locations != nil {
		l, err := terms("locations", *in.Locations)
		if err != nil {
			return Watch{}, err
		}
		add("locations", l)
	}
	if in.RemoteOnly != nil {
		add("remote_only", *in.RemoteOnly)
	}
	if in.Paused != nil {
		add("paused", *in.Paused)
	}
	var w Watch
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		if len(sets) > 0 {
			tag, err := tx.Exec(ctx, `UPDATE job_watch SET `+strings.Join(sets, ",")+`,version=version+1,updated_at=now() WHERE user_id=$1 AND id=$2`, args...)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return library.ErrNotFound
			}
		}
		var err error
		w, err = s.get(ctx, tx, u, id)
		return err
	})
	return w, err
}

// Delete stops watching a board. Alerts it made go with it.
func (s *Service) Delete(ctx context.Context, u, id uuid.UUID) error {
	return s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM job_watch WHERE user_id=$1 AND id=$2`, u, id)
		if err == nil && tag.RowsAffected() == 0 {
			err = library.ErrNotFound
		}
		return err
	})
}

// AlertsByID loads alerts for notifications, inside the caller's tenant transaction.
func AlertsByID(ctx context.Context, tx pgx.Tx, u uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]Alert, error) {
	out := map[uuid.UUID]Alert{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT id,watch_id,company,title,location,url,remote,published_at,found_at FROM job_alert WHERE user_id=$1 AND id=ANY($2)`, u, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.WatchID, &a.Company, &a.Title, &a.Location, &a.URL, &a.Remote, &a.PublishedAt, &a.FoundAt); err != nil {
			return nil, err
		}
		out[a.ID] = a
	}
	return out, rows.Err()
}

func deref(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}
