-- Separate database for `make test-pg` so tests can TRUNCATE freely.
CREATE DATABASE agenthub_test OWNER agenthub;
