-- +goose Up
-- SEC-1 adds security-relevant fields to the durable BuildRequest. A
-- mixed-version control-plane replica must not decode an unknown request,
-- erase those fields from its status projection, and provision with its old
-- execution contract. PostgreSQL therefore fences each attempt by the
-- minimum executor protocol recorded when the job is accepted.

ALTER TABLE workers
    ADD COLUMN executor_protocol integer NOT NULL DEFAULT 0
        CHECK (executor_protocol >= 0);

ALTER TABLE build_jobs
    ADD COLUMN minimum_executor_protocol integer NOT NULL DEFAULT 0
        CHECK (minimum_executor_protocol >= 0);

-- +goose StatementBegin
CREATE FUNCTION enforce_build_executor_protocol()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    required_protocol integer;
    worker_protocol integer;
BEGIN
    IF NEW.worker_id IS NULL THEN
        RETURN NEW;
    END IF;

    SELECT minimum_executor_protocol
      INTO required_protocol
      FROM build_jobs
     WHERE id = NEW.job_id;

    SELECT executor_protocol
      INTO worker_protocol
      FROM workers
     WHERE id = NEW.worker_id;

    IF worker_protocol IS NULL OR worker_protocol < required_protocol THEN
        RAISE EXCEPTION
            'executor protocol % does not satisfy job minimum %',
            COALESCE(worker_protocol, -1), required_protocol
            USING ERRCODE = '55000';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER build_attempt_executor_protocol
BEFORE INSERT OR UPDATE OF worker_id, job_id
ON build_attempts
FOR EACH ROW
EXECUTE FUNCTION enforce_build_executor_protocol();

-- +goose Down
DROP TRIGGER IF EXISTS build_attempt_executor_protocol ON build_attempts;
DROP FUNCTION IF EXISTS enforce_build_executor_protocol();

ALTER TABLE build_jobs
    DROP COLUMN IF EXISTS minimum_executor_protocol;

ALTER TABLE workers
    DROP COLUMN IF EXISTS executor_protocol;
