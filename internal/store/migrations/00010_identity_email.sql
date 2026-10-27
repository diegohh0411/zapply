-- Keep the verified email claim from the identity provider for platform
-- administration. It is scoped to the identity record and only exposed to
-- platform owners through the members function.
-- +goose Up
-- +goose StatementBegin
ALTER TABLE public.identity ADD COLUMN email text NOT NULL DEFAULT '';

CREATE FUNCTION public.resolve_identity(p_issuer text, p_subject text, p_new_id uuid, p_email text)
RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE uid uuid;
BEGIN
    uid := public.resolve_identity(p_issuer, p_subject, p_new_id);
    UPDATE identity SET email = btrim(p_email)
     WHERE issuer = p_issuer AND subject = p_subject AND NULLIF(btrim(p_email), '') IS NOT NULL;
    RETURN uid;
END $$;
REVOKE EXECUTE ON FUNCTION resolve_identity(text,text,uuid,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION resolve_identity(text,text,uuid,text) TO zapply_app;

CREATE OR REPLACE FUNCTION platform.members(p_after uuid,p_limit integer) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 RETURN COALESCE((SELECT jsonb_agg(jsonb_build_object('id',b.user_id,'email',COALESCE((SELECT min(email) FROM public.identity i WHERE i.user_id=b.user_id AND email<>''),''),
   'membership',platform.membership_json(b.user_id),'usage',platform.usage_for(b.user_id)) ORDER BY b.user_id)
   FROM (SELECT user_id FROM platform.budget_account WHERE p_after IS NULL OR user_id>p_after ORDER BY user_id LIMIT least(greatest(p_limit,1),100)) b),'[]');
END $$;
ALTER FUNCTION platform.members(uuid,integer) OWNER TO zapply_platform;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION public.resolve_identity(text,text,uuid,text);
ALTER TABLE public.identity DROP COLUMN email;
CREATE OR REPLACE FUNCTION platform.members(p_after uuid,p_limit integer) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, platform, pg_temp AS $$ BEGIN
 PERFORM platform.assert_owner();
 RETURN COALESCE((SELECT jsonb_agg(jsonb_build_object('id',b.user_id,'label',COALESCE((SELECT min(subject) FROM public.identity i WHERE i.user_id=b.user_id),b.user_id::text),
   'membership',platform.membership_json(b.user_id),'usage',platform.usage_for(b.user_id)) ORDER BY b.user_id)
   FROM (SELECT user_id FROM platform.budget_account WHERE p_after IS NULL OR user_id>p_after ORDER BY user_id LIMIT least(greatest(p_limit,1),100)) b),'[]');
END $$;
ALTER FUNCTION platform.members(uuid,integer) OWNER TO zapply_platform;
