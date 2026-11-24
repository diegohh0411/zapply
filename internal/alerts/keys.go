package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/text/unicode/norm"

	"github.com/diegohh0411/zapply/internal/joburl"
)

// candidate is a posting that matched one of a member's watches.
type candidate struct {
	WatchID     uuid.UUID
	PostingKey  string
	Company     string
	Title       string
	Location    string
	URL         string
	Remote      bool
	PublishedAt *time.Time
	// ATSKey is the posting's identity on an ATS board, when known.
	ATSKey string
}

// fileable reports whether job_alert accepts the posting: an https link of at
// most 2048 bytes and bounded text.
func (c candidate) fileable() bool {
	return strings.HasPrefix(c.URL, "https://") && len(c.URL) <= 2048 && c.PostingKey != "" && len(c.PostingKey) <= 300 && c.Title != ""
}

// fingerprintTTL is how long a company+title+city identity holds: past it, the
// same role posted again is a new opening, not a duplicate.
const fingerprintTTL = 60 * 24 * time.Hour

// keys are the identities of a posting that make two sightings the same
// opening: its ATS posting, its canonical URL, and its company, title and city.
func (c candidate) keys() []string {
	var out []string
	if c.ATSKey != "" {
		out = append(out, "ats:"+c.ATSKey)
	}
	if u, err := joburl.Normalize(c.URL); err == nil {
		out = append(out, "url:"+u)
	}
	if fp := fingerprint(c.Company, c.Title, c.Location); fp != "" {
		out = append(out, "fp:"+fp)
	}
	return out
}

// companySuffix drops legal forms, so "Acme, Inc." and "ACME" are one company.
var companySuffix = regexp.MustCompile(`\b(inc|llc|ltd|limited|corp|corporation|co|gmbh|sa|sas|s a|sa de cv|s de rl de cv|sapi de cv|bv|plc|ag)\b\.?$`)

// fingerprint is "company|title|city", folded to lowercase ASCII letters and
// digits; "" without a company or title.
func fingerprint(company, title, location string) string {
	c := companySuffix.ReplaceAllString(fold(company), "")
	c = strings.TrimSpace(c)
	t := fold(title)
	if c == "" || t == "" {
		return ""
	}
	city, _, _ := strings.Cut(location, ",")
	return c + "|" + t + "|" + fold(city)
}

// fold lowercases s, strips accents and punctuation, and collapses spaces.
func fold(s string) string {
	var b strings.Builder
	space := false
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		default:
			space = true
		}
	}
	return b.String()
}

// file inserts one member's alerts (the trigger files the notifications) and
// tells their open pages. A posting whose identity the member was already
// alerted about, or that is already one of their jobs, is skipped. It returns
// how many alerts it filed.
func (s *Service) file(ctx context.Context, u uuid.UUID, cs []candidate) (int64, error) {
	var added int64
	err := s.store.InTenant(ctx, u, func(tx pgx.Tx) error {
		added = 0
		// One filer per member at a time, so two polls cannot both find a
		// posting new and alert it twice.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('job_alert:'||$1::text,0))`, u); err != nil {
			return err
		}
		for _, c := range cs {
			if !c.fileable() {
				continue
			}
			keys := c.keys()
			fromATS := c.ATSKey != ""
			normalized, _ := joburl.Normalize(c.URL)
			// A fingerprint match is a duplicate unless both sightings are
			// ATS postings: then they are two openings with the same title.
			var seen bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM job_alert_key WHERE user_id=$1 AND key=ANY($2)
  AND (left(key,3)<>'fp:' OR (created_at>now()-$3::interval AND NOT ($5 AND from_ats))))
 OR EXISTS(SELECT FROM job WHERE user_id=$1 AND normalized_url=$4 AND normalized_url<>'')`,
				u, keys, fingerprintTTL.String(), normalized, fromATS).Scan(&seen); err != nil {
				return err
			}
			if seen {
				continue
			}
			var id uuid.UUID
			err := tx.QueryRow(ctx, `INSERT INTO job_alert(id,user_id,watch_id,posting_key,company,title,location,url,remote,published_at)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10 WHERE EXISTS(SELECT FROM job_watch WHERE user_id=$2 AND id=$3)
 ON CONFLICT(watch_id,posting_key) DO NOTHING RETURNING id`,
				uuid.Must(uuid.NewV7()), u, c.WatchID, c.PostingKey, c.Company, c.Title, c.Location, c.URL, c.Remote, c.PublishedAt).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO job_alert_key(user_id,key,alert_id,from_ats) SELECT $1,k,$2,$4 FROM unnest($3::text[]) k
 ON CONFLICT(user_id,key) DO UPDATE SET alert_id=EXCLUDED.alert_id,from_ats=EXCLUDED.from_ats,created_at=now()`, u, id, keys, fromATS); err != nil {
				return err
			}
			added++
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
