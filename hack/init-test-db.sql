-- Separate database for `make test-pg` so tests can TRUNCATE freely.
CREATE DATABASE agents_operator_test OWNER agents_operator;
