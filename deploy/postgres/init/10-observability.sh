#!/bin/sh
set -eu

psql \
  --set=ON_ERROR_STOP=1 \
  --set=otel_password="${OTEL_POSTGRES_PASSWORD}" \
	  --set=app_user="${PORTAGE_POSTGRES_APP_USER}" \
	  --set=app_password="${PORTAGE_POSTGRES_APP_PASSWORD}" \
	  --set=signer_password="${PORTAGE_POSTGRES_SIGNER_PASSWORD:-portage-signer-local}" \
  --set=database_name="${POSTGRES_DB}" \
  --username "${POSTGRES_USER}" \
  --dbname "${POSTGRES_DB}" <<-'EOSQL'
SELECT format(
  'CREATE ROLE portage_otel LOGIN PASSWORD %L',
  :'otel_password'
)
WHERE NOT EXISTS (
  SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'portage_otel'
)
\gexec

	GRANT pg_monitor TO portage_otel;
	GRANT CONNECT ON DATABASE :"database_name" TO portage_otel;

	SELECT format(
	  'CREATE ROLE portage_signer LOGIN PASSWORD %L',
	  :'signer_password'
	)
	WHERE NOT EXISTS (
	  SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'portage_signer'
	)
	\gexec

	GRANT CONNECT ON DATABASE :"database_name" TO portage_signer;
	GRANT USAGE ON SCHEMA public TO portage_signer;

SELECT format(
  'CREATE ROLE %I LOGIN PASSWORD %L',
  :'app_user',
  :'app_password'
)
WHERE NOT EXISTS (
  SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = :'app_user'
)
\gexec

SELECT format('GRANT CONNECT ON DATABASE %I TO %I', :'database_name', :'app_user')
\gexec
SELECT format('GRANT USAGE ON SCHEMA public TO %I', :'app_user')
\gexec
SELECT format(
  'ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %I',
  current_user,
  :'app_user'
)
\gexec
SELECT format(
  'ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO %I',
  current_user,
  :'app_user'
)
\gexec

-- These two grants make the script safe to re-run against an existing
-- development volume after DB-0 is introduced.
SELECT format(
  'GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %I',
  :'app_user'
)
\gexec
SELECT format(
  'GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %I',
  :'app_user'
)
\gexec
EOSQL
