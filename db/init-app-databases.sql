-- The stable and candidate services must not share writable state, otherwise
-- a shadow write would change what real users see. They get one database
-- each, in the same Postgres container to keep the laptop setup small.
--
-- Runs before init.sql (the entrypoint sorts the files by name).
CREATE DATABASE stable_app;
CREATE DATABASE candidate_sandbox;
