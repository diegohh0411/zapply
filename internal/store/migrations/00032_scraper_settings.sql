-- +goose Up
-- +goose StatementBegin
-- Posting pages are scraped through an ordered list of providers; when one
-- fails the next is tried. The owner edits the list and per-site rules, which
-- name the providers to try for a host (and its subdomains) instead of the
-- default list. Providers are jsonb objects {kind, base_url, key_id}; rules
-- are {host, providers}, where providers are positions in the provider list.
-- A later migration widens the kinds (and replaces the validator) when a new
-- provider is added.
CREATE FUNCTION platform.scraper_settings_valid(p_providers jsonb,p_rules jsonb) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE p jsonb; r jsonb; e jsonb; n int; k text;
BEGIN
 IF jsonb_typeof(p_providers) IS DISTINCT FROM 'array' OR jsonb_typeof(p_rules) IS DISTINCT FROM 'array' THEN RETURN false; END IF;
 n:=jsonb_array_length(p_providers);
 IF n>10 OR jsonb_array_length(p_rules)>100 THEN RETURN false; END IF;
 FOR p IN SELECT * FROM jsonb_array_elements(p_providers) LOOP
  IF jsonb_typeof(p) IS DISTINCT FROM 'object' OR COALESCE(p->>'kind','') NOT IN ('firecrawl') THEN RETURN false; END IF;
  FOR k IN SELECT jsonb_object_keys(p) LOOP
   IF k NOT IN ('kind','base_url','key_id') THEN RETURN false; END IF;
  END LOOP;
  IF jsonb_typeof(p->'base_url') NOT IN ('string','null') OR jsonb_typeof(p->'key_id') NOT IN ('string','null') THEN RETURN false; END IF;
  IF (p->>'base_url') IS NOT NULL AND ((p->>'base_url') !~ '^https?://[^[:space:]]+$' OR length(p->>'base_url')>2048) THEN RETURN false; END IF;
  IF (p->>'key_id') IS NOT NULL AND (p->>'key_id') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN RETURN false; END IF;
 END LOOP;
 FOR r IN SELECT * FROM jsonb_array_elements(p_rules) LOOP
  IF jsonb_typeof(r) IS DISTINCT FROM 'object' THEN RETURN false; END IF;
  FOR k IN SELECT jsonb_object_keys(r) LOOP
   IF k NOT IN ('host','providers') THEN RETURN false; END IF;
  END LOOP;
  IF jsonb_typeof(r->'host') IS DISTINCT FROM 'string' OR length(r->>'host')>253
   OR (r->>'host') !~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$' THEN RETURN false; END IF;
  IF jsonb_typeof(r->'providers') IS DISTINCT FROM 'array' OR jsonb_array_length(r->'providers') NOT BETWEEN 1 AND 10 THEN RETURN false; END IF;
  FOR e IN SELECT * FROM jsonb_array_elements(r->'providers') LOOP
   IF jsonb_typeof(e) IS DISTINCT FROM 'number' THEN RETURN false; END IF;
   IF e::text !~ '^[0-9]{1,2}$' THEN RETURN false; END IF;
   IF (e::text)::int>=n THEN RETURN false; END IF;
  END LOOP;
 END LOOP;
 RETURN true;
END $$;

-- saved_at stays null until the owner saves (or the environment is imported at
-- boot), so a deployment is seeded from SCRAPER_PROVIDER/FIRECRAWL_* once.
CREATE TABLE platform.scraper_settings(
 id boolean PRIMARY KEY DEFAULT true CHECK(id),
 providers jsonb NOT NULL DEFAULT '[]',
 rules jsonb NOT NULL DEFAULT '[]',
 saved_at timestamptz,
 version int NOT NULL DEFAULT 1,
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK(platform.scraper_settings_valid(providers,rules)));
INSERT INTO platform.scraper_settings(id) VALUES(true);
GRANT SELECT,INSERT,UPDATE ON platform.scraper_settings TO zapply_platform;

-- Provider keys share system_key with OpenRouter's. Only an OpenRouter key is
-- ever active (model calls use the active one); a scraper provider names the
-- Firecrawl key it uses in its settings.
ALTER TABLE platform.system_key ADD COLUMN kind text NOT NULL DEFAULT 'openrouter' CHECK(kind IN ('openrouter','firecrawl'));
ALTER TABLE platform.system_key ADD CONSTRAINT system_key_active_kind CHECK(kind='openrouter' OR NOT active);

-- The migration login can SET ROLE but does not inherit platform function ownership.
SET LOCAL ROLE zapply_platform;
CREATE OR REPLACE FUNCTION platform.keys() RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 RETURN COALESCE((SELECT jsonb_agg(jsonb_build_object('id',id,'label',label,'kind',kind,'fingerprint',fingerprint,'active',active,'version',version,'created_at',created_at) ORDER BY created_at DESC) FROM platform.system_key),'[]');
END $$;
DROP FUNCTION platform.add_key(uuid,text,bytea,text,text);
CREATE FUNCTION platform.add_key(p_id uuid,p_label text,p_cipher bytea,p_enc text,p_fingerprint text,p_kind text DEFAULT 'openrouter') RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 INSERT INTO platform.system_key(id,label,ciphertext,encryption_version,fingerprint,kind) VALUES(p_id,p_label,p_cipher,p_enc,p_fingerprint,p_kind);
 PERFORM platform.audit_event('key.created',p_id,jsonb_build_object('label',p_label,'fingerprint',p_fingerprint,'kind',p_kind));
END $$;

CREATE FUNCTION platform.scraper_settings() RETURNS jsonb LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT jsonb_build_object('providers',providers,'rules',rules,'saved',saved_at IS NOT NULL,'version',version,'updated_at',updated_at) FROM platform.scraper_settings WHERE id
$$;
CREATE FUNCTION platform.save_scraper_settings(p_providers jsonb,p_rules jsonb,p_version int) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE p jsonb; BEGIN
 PERFORM platform.assert_owner();
 IF NOT platform.scraper_settings_valid(p_providers,p_rules) THEN RAISE EXCEPTION 'invalid scraper settings' USING ERRCODE='ZA422'; END IF;
 FOR p IN SELECT * FROM jsonb_array_elements(p_providers) LOOP
  IF (p->>'key_id') IS NOT NULL AND NOT EXISTS(SELECT FROM platform.system_key WHERE id=(p->>'key_id')::uuid AND kind='firecrawl') THEN
   RAISE EXCEPTION 'unknown Firecrawl key' USING ERRCODE='ZA422';
  END IF;
 END LOOP;
 UPDATE platform.scraper_settings SET providers=p_providers,rules=p_rules,saved_at=now(),version=version+1,updated_at=now() WHERE id AND version=p_version;
 IF NOT FOUND THEN RAISE EXCEPTION 'settings changed' USING ERRCODE='ZA412'; END IF;
 PERFORM platform.audit_event('scraper_settings.saved',public.app_user_id(),jsonb_build_object('providers',p_providers,'rules',p_rules));
 RETURN platform.scraper_settings();
END $$;
-- Imports the environment's scraper once, while nothing has been saved. It
-- runs at boot without a signed-in owner, so it only ever writes that first
-- state: later calls change nothing.
CREATE FUNCTION platform.seed_scraper_settings(p_providers jsonb,p_key_id uuid,p_cipher bytea,p_enc text,p_fingerprint text) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$ BEGIN
 PERFORM 1 FROM platform.scraper_settings WHERE id AND saved_at IS NULL FOR UPDATE;
 IF NOT FOUND THEN RETURN false; END IF;
 IF p_key_id IS NOT NULL THEN
  INSERT INTO platform.system_key(id,label,ciphertext,encryption_version,fingerprint,kind) VALUES(p_key_id,'Firecrawl (imported from the environment)',p_cipher,p_enc,p_fingerprint,'firecrawl');
  PERFORM platform.audit_event('key.created',p_key_id,jsonb_build_object('label','Firecrawl (imported from the environment)','fingerprint',p_fingerprint,'kind','firecrawl'));
 END IF;
 IF NOT platform.scraper_settings_valid(p_providers,'[]') THEN RAISE EXCEPTION 'invalid scraper settings' USING ERRCODE='ZA422'; END IF;
 UPDATE platform.scraper_settings SET providers=p_providers,saved_at=now(),version=version+1,updated_at=now() WHERE id;
 PERFORM platform.audit_event('scraper_settings.imported',COALESCE(p_key_id,gen_random_uuid()),jsonb_build_object('providers',p_providers));
 RETURN true;
END $$;
-- Decrypts nothing: it hands the sealed Firecrawl key a provider names to the
-- API, which opens it with the key ring.
CREATE FUNCTION platform.scraper_key(p_id uuid) RETURNS TABLE(ciphertext bytea,encryption_version text) LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT k.ciphertext,k.encryption_version FROM platform.system_key k WHERE k.id=p_id AND k.kind='firecrawl'
$$;
REVOKE EXECUTE ON FUNCTION platform.add_key(uuid,text,bytea,text,text,text),platform.scraper_settings(),platform.save_scraper_settings(jsonb,jsonb,int),platform.seed_scraper_settings(jsonb,uuid,bytea,text,text),platform.scraper_key(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.add_key(uuid,text,bytea,text,text,text),platform.save_scraper_settings(jsonb,jsonb,int),platform.seed_scraper_settings(jsonb,uuid,bytea,text,text) TO zapply_app;
GRANT EXECUTE ON FUNCTION platform.scraper_settings(),platform.scraper_key(uuid) TO zapply_app,zapply_worker;
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SET LOCAL ROLE zapply_platform;
DROP FUNCTION platform.scraper_key(uuid),platform.seed_scraper_settings(jsonb,uuid,bytea,text,text),platform.save_scraper_settings(jsonb,jsonb,int),platform.scraper_settings(),platform.add_key(uuid,text,bytea,text,text,text);
CREATE FUNCTION platform.add_key(p_id uuid,p_label text,p_cipher bytea,p_enc text,p_fingerprint text) RETURNS void LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 INSERT INTO platform.system_key(id,label,ciphertext,encryption_version,fingerprint) VALUES(p_id,p_label,p_cipher,p_enc,p_fingerprint);
 PERFORM platform.audit_event('key.created',p_id,jsonb_build_object('label',p_label,'fingerprint',p_fingerprint));
END $$;
CREATE OR REPLACE FUNCTION platform.keys() RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 RETURN COALESCE((SELECT jsonb_agg(jsonb_build_object('id',id,'label',label,'fingerprint',fingerprint,'active',active,'version',version,'created_at',created_at) ORDER BY created_at DESC) FROM platform.system_key),'[]');
END $$;
REVOKE EXECUTE ON FUNCTION platform.add_key(uuid,text,bytea,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION platform.add_key(uuid,text,bytea,text,text) TO zapply_app;
RESET ROLE;
DELETE FROM platform.system_key WHERE kind<>'openrouter';
ALTER TABLE platform.system_key DROP CONSTRAINT system_key_active_kind;
ALTER TABLE platform.system_key DROP COLUMN kind;
DROP TABLE platform.scraper_settings;
DROP FUNCTION platform.scraper_settings_valid(jsonb,jsonb);
-- +goose StatementEnd
