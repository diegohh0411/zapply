-- The owner's TheirStack key, sealed, so the admin page can show the team's
-- live credit balance. TheirStack publishes no prices over its API, so the
-- credit price stays an owner setting; the balance is what can be read.

-- +goose Up
-- +goose StatementBegin
SET LOCAL ROLE zapply_platform;
CREATE FUNCTION platform.search_key() RETURNS TABLE(key_id uuid,ciphertext bytea,encryption_version text) LANGUAGE plpgsql SECURITY DEFINER
SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 RETURN QUERY SELECT k.id,k.ciphertext,k.encryption_version FROM platform.search_settings s
  JOIN platform.system_key k ON k.id=s.key_id AND k.kind='theirstack';
END $$;
REVOKE EXECUTE ON FUNCTION platform.search_key() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.search_key() TO zapply_app;
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION platform.search_key();
-- +goose StatementEnd
