-- Protected platform reads expose only function contracts, never tables.
-- name: GetPlatformRole :one
SELECT platform.role()::text AS role;

-- name: GetPlatformUsage :one
SELECT platform.usage()::jsonb AS usage;

-- name: ListPlatformPlans :one
SELECT platform.plans()::jsonb AS plans;

-- name: ListSystemKeys :one
SELECT platform.keys()::jsonb AS keys;
