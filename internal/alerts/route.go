package alerts

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// Hold is how long routing waits for a posting's labels. Past it, the posting
// goes out on whatever is known: a late alert costs more than a loose one.
const Hold = 10 * time.Minute

// routeBatch bounds the postings one routing transaction locks.
const routeBatch = 50

// RouteArgs wakes routing. There is at most one waiting at a time.
type RouteArgs struct{}

func (RouteArgs) Kind() string { return "alert_route" }

type routeWorker struct {
	river.WorkerDefaults[RouteArgs]
	s *Service
}

func (w *routeWorker) Work(ctx context.Context, _ *river.Job[RouteArgs]) error {
	_, err := w.s.Route(ctx)
	return err
}

// Wake queues routing inside tx, for the catalog's Labeled hook.
func (s *Service) Wake(ctx context.Context, tx pgx.Tx) error {
	if s.queue == nil {
		return nil
	}
	return s.queue.InsertTx(ctx, tx, RouteArgs{})
}

type routable struct {
	ID          uuid.UUID
	Company     string
	Title       string
	Location    string
	URL         string
	Remote      bool
	Country     *string
	PublishedAt *time.Time
	Labels      Posting
}

// Route files an alert for each member every ready posting fits, and marks
// the posting routed. A posting is ready once each classification stage with
// a model has finished (answered or given up), or once it has waited Hold.
// Alerts are filed before the posting is marked, so a crash routes it again,
// and job_alert's unique (user_id, posting_id) files nothing twice. It returns
// how many alerts it filed.
func (s *Service) Route(ctx context.Context) (int, error) {
	total := 0
	for {
		n, more, err := s.routeBatch(ctx)
		total += n
		if err != nil || !more {
			return total, err
		}
	}
}

func (s *Service) routeBatch(ctx context.Context) (int, bool, error) {
	filed, more := 0, false
	err := pgx.BeginFunc(ctx, s.store.Pool(), func(tx pgx.Tx) error {
		filed, more = 0, false
		ps, err := readyPostings(ctx, tx)
		if err != nil || len(ps) == 0 {
			return err
		}
		more = len(ps) == routeBatch
		audience, err := loadAudience(ctx, tx)
		if err != nil {
			return err
		}
		byUser := map[uuid.UUID][]routable{}
		for _, p := range ps {
			for _, a := range audience {
				if Fits(a.Wants, p.Labels) {
					byUser[a.UserID] = append(byUser[a.UserID], p)
				}
			}
		}
		for u, mine := range byUser {
			n, err := s.file(ctx, u, mine)
			if err != nil {
				return err
			}
			filed += n
		}
		ids := make([]uuid.UUID, len(ps))
		for i, p := range ps {
			ids[i] = p.ID
		}
		_, err = tx.Exec(ctx, `UPDATE platform.posting SET routed_at=now() WHERE id=ANY($1)`, ids)
		return err
	})
	return filed, more, err
}

func readyPostings(ctx context.Context, tx pgx.Tx) ([]routable, error) {
	rows, err := tx.Query(ctx, `SELECT p.id,p.company,p.title,p.location,p.url,p.remote,p.country,p.published_at,
  l.seniority,l.areas,l.employment_type,l.remote,l.countries,COALESCE(l.confidence,'{}')
 FROM platform.posting p LEFT JOIN platform.posting_label l ON l.posting_id=p.id, platform.posting_models() m
 WHERE p.routed_at IS NULL AND (p.first_seen_at < now()-$1::interval OR (
  (m.decision IS NULL OR EXISTS(SELECT FROM platform.posting_stage s WHERE s.posting_id=p.id AND s.stage='decision' AND s.done)) AND
  (m.extraction IS NULL OR EXISTS(SELECT FROM platform.posting_stage s WHERE s.posting_id=p.id AND s.stage='extraction' AND s.done))))
 ORDER BY p.first_seen_at,p.id LIMIT $2 FOR UPDATE OF p SKIP LOCKED`, Hold.String(), routeBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []routable
	for rows.Next() {
		var p routable
		var seniority, employment *string
		var remote *bool
		var areas, countries []string
		var raw []byte
		if err := rows.Scan(&p.ID, &p.Company, &p.Title, &p.Location, &p.URL, &p.Remote, &p.Country, &p.PublishedAt,
			&seniority, &areas, &employment, &remote, &countries, &raw); err != nil {
			return nil, err
		}
		conf := map[string]float64{}
		if err := json.Unmarshal(raw, &conf); err != nil {
			return nil, err
		}
		p.Labels = labelsOf(p.Remote, p.Country, seniority, areas, employment, remote, countries, conf)
		out = append(out, p)
	}
	return out, rows.Err()
}

// labelsOf keeps the labels the classifier was sure of, and falls back to
// what the source said for remote and country.
func labelsOf(srcRemote bool, srcCountry, seniority *string, areas []string, employment *string, remote *bool, countries []string, conf map[string]float64) Posting {
	var l Posting
	if seniority != nil && sure(conf, "seniority") {
		l.Seniority = seniority
	}
	if sure(conf, "areas") {
		l.Areas = areas
	}
	if employment != nil && sure(conf, "employment_type") {
		l.EmploymentType = employment
	}
	l.Remote = srcRemote
	if remote != nil && sure(conf, "remote") {
		l.Remote = *remote
	}
	if len(countries) > 0 && sure(conf, "countries") {
		l.Countries = countries
	} else if srcCountry != nil {
		l.Countries = []string{*srcCountry}
	}
	return l
}

type member struct {
	UserID uuid.UUID
	Wants  Wants
}

func loadAudience(ctx context.Context, tx pgx.Tx) ([]member, error) {
	rows, err := tx.Query(ctx, `SELECT user_id,COALESCE(seniority_min,''),COALESCE(seniority_max,''),areas,countries,remote_ok,excluded_employment FROM platform.alert_audience()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []member
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.UserID, &m.Wants.SeniorityMin, &m.Wants.SeniorityMax, &m.Wants.Areas, &m.Wants.Countries, &m.Wants.RemoteOK, &m.Wants.ExcludedEmployment); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// file inserts one member's alerts (the trigger files the notifications) and
// tells their open pages. A posting already one of their jobs is skipped.
func (s *Service) file(ctx context.Context, u uuid.UUID, ps []routable) (int, error) {
	added := 0
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		added = 0
		for _, p := range ps {
			tag, err := tx.Exec(ctx, `INSERT INTO job_alert(id,user_id,posting_id,company,title,location,url,remote,published_at)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9
 WHERE NOT EXISTS(SELECT FROM job WHERE user_id=$2 AND normalized_url<>'' AND normalized_url=$10)
 ON CONFLICT(user_id,posting_id) DO NOTHING`,
				uuid.Must(uuid.NewV7()), u, p.ID, p.Company, p.Title, p.Location, p.URL, p.Remote, p.PublishedAt, normalized(p.URL))
			if err != nil {
				return err
			}
			added += int(tag.RowsAffected())
		}
		if added == 0 {
			return nil
		}
		payload, _ := json.Marshal(map[string]any{"id": u})
		_, err := tx.Exec(ctx, "INSERT INTO outbox(id,user_id,topic,payload) VALUES($1,$2,$3,$4)", uuid.Must(uuid.NewV7()), u, "notification.updated", json.RawMessage(payload))
		return err
	})
	return added, err
}
