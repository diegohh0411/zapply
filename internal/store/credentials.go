package store

import (
	"context"
	"errors"
	"github.com/diegohh0411/zapply/internal/secrets"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReencryptCredentials runs explicitly with migration credentials after every
// API/worker has received a ring containing both the old and new master keys.
func ReencryptCredentials(ctx context.Context, url string, ring *secrets.Ring) error {
	if ring == nil {
		return errors.New("encryption key ring is required")
	}
	c, err := connectAdmin(ctx, url)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	return pgx.BeginFunc(ctx, c, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT id FROM platform.provider_lock WHERE id FOR UPDATE"); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "SELECT id,ciphertext,encryption_version FROM platform.system_key ORDER BY id FOR UPDATE")
		if err != nil {
			return err
		}
		type credential struct {
			id        uuid.UUID
			encrypted []byte
			version   string
		}
		var keys []credential
		for rows.Next() {
			var k credential
			if err := rows.Scan(&k.id, &k.encrypted, &k.version); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, k)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, k := range keys {
			plaintext, err := ring.Decrypt(k.id.String(), k.version, k.encrypted)
			if err != nil {
				return err
			}
			cipher, version, err := ring.Encrypt(k.id.String(), plaintext)
			if err != nil {
				return err
			}
			if version == k.version {
				continue
			}
			if _, err := tx.Exec(ctx, "UPDATE platform.system_key SET ciphertext=$1,encryption_version=$2,version=version+1 WHERE id=$3", cipher, version, k.id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "INSERT INTO platform.audit(action,subject_id,detail) VALUES('key.reencrypted',$1,jsonb_build_object('encryption_version',$2::text))", k.id, version); err != nil {
				return err
			}
		}
		return reencryptMail(ctx, tx, ring)
	})
}

// reencryptMail does the same for connected mailboxes' refresh tokens, which
// are sealed with the same ring (part 10).
func reencryptMail(ctx context.Context, tx pgx.Tx, ring *secrets.Ring) error {
	rows, err := tx.Query(ctx, "SELECT account_id,ciphertext,encryption_version FROM platform.mail_credential ORDER BY account_id FOR UPDATE")
	if err != nil {
		return err
	}
	type credential struct {
		id        uuid.UUID
		encrypted []byte
		version   string
	}
	var tokens []credential
	for rows.Next() {
		var k credential
		if err := rows.Scan(&k.id, &k.encrypted, &k.version); err != nil {
			rows.Close()
			return err
		}
		tokens = append(tokens, k)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, k := range tokens {
		plaintext, err := ring.Decrypt(k.id.String(), k.version, k.encrypted)
		if err != nil {
			return err
		}
		cipher, version, err := ring.Encrypt(k.id.String(), plaintext)
		if err != nil {
			return err
		}
		if version == k.version {
			continue
		}
		if _, err := tx.Exec(ctx, "UPDATE platform.mail_credential SET ciphertext=$1,encryption_version=$2,updated_at=now() WHERE account_id=$3", cipher, version, k.id); err != nil {
			return err
		}
	}
	return nil
}
