-- Local Supabase only (`make supa-restore`). Dumps never carry role passwords,
-- so the app roles get the development passwords .env.supabase.example uses.
ALTER ROLE zapply_app PASSWORD 'zapply_app';
ALTER ROLE zapply_relay PASSWORD 'zapply_relay';
ALTER ROLE zapply_worker PASSWORD 'zapply_worker';

-- Users are keyed by (token issuer, subject). Local Auth keeps each user's ID
-- (the subject) but issues tokens as itself, so map the local issuer to the
-- users the hosted issuer already had.
INSERT INTO public.identity (issuer, subject, user_id, email)
SELECT 'http://127.0.0.1:54321/auth/v1', subject, user_id, email
  FROM public.identity
 WHERE issuer ~ '^https://[a-z0-9]+\.supabase\.co/auth/v1$'
ON CONFLICT (issuer, subject) DO UPDATE SET user_id = EXCLUDED.user_id;
