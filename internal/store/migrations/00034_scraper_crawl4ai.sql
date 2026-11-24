-- +goose Up
-- +goose StatementBegin
-- Crawl4AI joins Firecrawl as a scraper provider. It has no hosted default, so
-- a crawl4ai provider must name its base_url, and its optional bearer token is
-- a system key of kind 'crawl4ai'. A provider can only name a key of its own
-- kind. Model calls still use only an active OpenRouter key.
CREATE OR REPLACE FUNCTION platform.scraper_settings_valid(p_providers jsonb,p_rules jsonb) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE p jsonb; r jsonb; e jsonb; n int; k text;
BEGIN
 IF jsonb_typeof(p_providers) IS DISTINCT FROM 'array' OR jsonb_typeof(p_rules) IS DISTINCT FROM 'array' THEN RETURN false; END IF;
 n:=jsonb_array_length(p_providers);
 IF n>10 OR jsonb_array_length(p_rules)>100 THEN RETURN false; END IF;
 FOR p IN SELECT * FROM jsonb_array_elements(p_providers) LOOP
  IF jsonb_typeof(p) IS DISTINCT FROM 'object' OR COALESCE(p->>'kind','') NOT IN ('firecrawl','crawl4ai') THEN RETURN false; END IF;
  FOR k IN SELECT jsonb_object_keys(p) LOOP
   IF k NOT IN ('kind','base_url','key_id') THEN RETURN false; END IF;
  END LOOP;
  IF jsonb_typeof(p->'base_url') NOT IN ('string','null') OR jsonb_typeof(p->'key_id') NOT IN ('string','null') THEN RETURN false; END IF;
  IF (p->>'base_url') IS NOT NULL AND ((p->>'base_url') !~ '^https?://[^[:space:]]+$' OR length(p->>'base_url')>2048) THEN RETURN false; END IF;
  IF p->>'kind'='crawl4ai' AND (p->>'base_url') IS NULL THEN RETURN false; END IF;
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

ALTER TABLE platform.system_key DROP CONSTRAINT system_key_kind_check;
ALTER TABLE platform.system_key ADD CONSTRAINT system_key_kind_check CHECK(kind IN ('openrouter','firecrawl','crawl4ai'));

-- The migration login can SET ROLE but does not inherit platform function ownership.
SET LOCAL ROLE zapply_platform;
CREATE OR REPLACE FUNCTION platform.save_scraper_settings(p_providers jsonb,p_rules jsonb,p_version int) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
DECLARE p jsonb; BEGIN
 PERFORM platform.assert_owner();
 IF NOT platform.scraper_settings_valid(p_providers,p_rules) THEN RAISE EXCEPTION 'invalid scraper settings' USING ERRCODE='ZA422'; END IF;
 FOR p IN SELECT * FROM jsonb_array_elements(p_providers) LOOP
  IF (p->>'key_id') IS NOT NULL AND NOT EXISTS(SELECT FROM platform.system_key WHERE id=(p->>'key_id')::uuid AND kind=p->>'kind') THEN
   RAISE EXCEPTION 'unknown % key',p->>'kind' USING ERRCODE='ZA422';
  END IF;
 END LOOP;
 UPDATE platform.scraper_settings SET providers=p_providers,rules=p_rules,saved_at=now(),version=version+1,updated_at=now() WHERE id AND version=p_version;
 IF NOT FOUND THEN RAISE EXCEPTION 'settings changed' USING ERRCODE='ZA412'; END IF;
 PERFORM platform.audit_event('scraper_settings.saved',public.app_user_id(),jsonb_build_object('providers',p_providers,'rules',p_rules));
 RETURN platform.scraper_settings();
END $$;
-- The sealed key a scraper provider names. save_scraper_settings has checked
-- that its kind matches the provider's, and a key's kind never changes.
CREATE OR REPLACE FUNCTION platform.scraper_key(p_id uuid) RETURNS TABLE(ciphertext bytea,encryption_version text) LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT k.ciphertext,k.encryption_version FROM platform.system_key k WHERE k.id=p_id AND k.kind IN ('firecrawl','crawl4ai')
$$;
RESET ROLE;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- A saved crawl4ai provider cannot be expressed by the older validator, and
-- dropping it would renumber the positions that rules use: remove it first.
DO $$ BEGIN
 IF EXISTS(SELECT FROM platform.scraper_settings, jsonb_array_elements(providers) p WHERE p->>'kind'='crawl4ai') THEN
  RAISE EXCEPTION 'remove the Crawl4AI scraper providers in Administration before rolling back';
 END IF;
END $$;
SET LOCAL ROLE zapply_platform;
CREATE OR REPLACE FUNCTION platform.save_scraper_settings(p_providers jsonb,p_rules jsonb,p_version int) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
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
CREATE OR REPLACE FUNCTION platform.scraper_key(p_id uuid) RETURNS TABLE(ciphertext bytea,encryption_version text) LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,platform,pg_temp AS $$
 SELECT k.ciphertext,k.encryption_version FROM platform.system_key k WHERE k.id=p_id AND k.kind='firecrawl'
$$;
RESET ROLE;
DELETE FROM platform.system_key WHERE kind='crawl4ai';
ALTER TABLE platform.system_key DROP CONSTRAINT system_key_kind_check;
ALTER TABLE platform.system_key ADD CONSTRAINT system_key_kind_check CHECK(kind IN ('openrouter','firecrawl'));
CREATE OR REPLACE FUNCTION platform.scraper_settings_valid(p_providers jsonb,p_rules jsonb) RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $$
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
-- +goose StatementEnd
