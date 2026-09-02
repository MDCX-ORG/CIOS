-- CIOS core store: tickets resource_version — optimistic lock
-- (PRMT-082, eval M3). Mirrors assets.resource_version (PRMT-016b).
--
-- Goal: make POST /v1/tickets/{id}:transition and :assign atomic
-- compare-and-set operations so concurrent state-machine writers
-- cannot last-writer-wins each other's transitions. The
-- application layer reads the current version, mutates locally,
-- and writes back with the version it observed; the WHERE
-- clause on UPDATE rows=0 means another writer slipped in and
-- the caller must 409 (client retries with fresh read).
--
-- DEFAULT 0 keeps the existing rows (no version seen yet)
-- addressable: the create path uses expectVersion=0 so a fresh
-- insert with version=0 still upserts cleanly, and subsequent
-- transitions on a v0 row use the observed (0) version for the
-- CAS. After the first successful UPDATE the column advances
-- to 1, then 2, ...

ALTER TABLE tickets
    ADD COLUMN IF NOT EXISTS resource_version BIGINT NOT NULL DEFAULT 0;
