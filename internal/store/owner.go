package store

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SetOwner is an explicit deployment operation using the migration credentials.
// It cannot be invoked through the runtime database role or HTTP API.
func SetOwner(ctx context.Context, url string, id uuid.UUID, grant bool) error {
	c, err := connectAdmin(ctx, url)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	return pgx.BeginFunc(ctx, c, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT FROM platform.budget_account WHERE user_id=$1)", id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errors.New("user must sign in before owner provisioning")
		}
		action := "owner.revoked"
		if grant {
			_, err = tx.Exec(ctx, "INSERT INTO platform.owner VALUES($1) ON CONFLICT DO NOTHING", id)
			action = "owner.granted"
		} else {
			_, err = tx.Exec(ctx, "DELETE FROM platform.owner WHERE user_id=$1", id)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO platform.audit(action,subject_id) VALUES($1,$2)", action, id)
		return err
	})
}
