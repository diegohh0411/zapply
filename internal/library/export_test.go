package library

import (
	"bytes"
	"context"

	"github.com/google/uuid"

	"github.com/diegohh0411/zapply/internal/store/db"
)

// StaleHashes returns evidence whose stored hashes (or any of its
// highlights') differ from hashes recomputed from current content.
func (s *Service) StaleHashes(ctx context.Context, uid uuid.UUID) ([]uuid.UUID, error) {
	var stale []uuid.UUID
	err := s.tx(ctx, uid, func(q *db.Queries) error {
		rows, err := q.ListEvidences(ctx, db.ListEvidencesParams{UserID: uid, IncludeArchived: true, RowLimit: 10000})
		if err != nil {
			return err
		}
		docs, err := loadDocuments(ctx, q, uid, rows)
		if err != nil {
			return err
		}
		for _, d := range docs {
			var org string
			if d.OrganizationID != nil {
				o, err := q.GetOrganization(ctx, db.GetOrganizationParams{UserID: uid, ID: *d.OrganizationID})
				if err != nil {
					return err
				}
				org = o.Name
			}
			ok := bytes.Equal(evidenceHash(d, org), d.ContentHash)
			for _, h := range d.Highlights {
				ok = ok && bytes.Equal(highlightHash(h), h.ContentHash)
			}
			if !ok {
				stale = append(stale, d.ID)
			}
		}
		return nil
	})
	return stale, err
}
