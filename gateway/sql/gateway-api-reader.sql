-- The read-only database role for `autopilot-gateway api` (GATEWAY_API_DSN). Run once as the database owner, against the gateway's
-- database (the one GATEWAY_DSN names), and again after `autopilot-gateway migrate` if a release adds a table. Idempotent.
--
--   psql "$OWNER_DSN" -v ON_ERROR_STOP=1 -f gateway/sql/gateway-api-reader.sql
--
-- It makes a NOLOGIN group role, gateway_api_reader, with SELECT on every gateway_* table and nothing else: no write on any table, no
-- sequence, no CREATE, no membership of a role that can write. `api` refuses to start on any role that could write
-- (readapi.PgStore.CheckRole), and this one passes. The login role is the owner's own step, so no password is in this file:
--
--   CREATE ROLE autopilot_reader LOGIN PASSWORD '<the password: Infisical, never this file>' IN ROLE gateway_api_reader;
--
-- and GATEWAY_API_DSN is that role's connection string. A role that is a member of pg_read_all_data (what the Python gateway's
-- deployment uses) also passes the check; this file is the narrower grant (only gateway_* tables are readable).

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'gateway_api_reader') THEN
    CREATE ROLE gateway_api_reader NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
  END IF;
END
$$;

DO $$
DECLARE t text;
BEGIN
  EXECUTE format('GRANT CONNECT ON DATABASE %I TO gateway_api_reader', current_database());
  EXECUTE format('GRANT USAGE ON SCHEMA %I TO gateway_api_reader', current_schema());
  FOR t IN SELECT tablename FROM pg_tables WHERE schemaname = current_schema() AND tablename LIKE 'gateway\_%' LOOP
    EXECUTE format('GRANT SELECT ON %I.%I TO gateway_api_reader', current_schema(), t);
  END LOOP;
END
$$;
