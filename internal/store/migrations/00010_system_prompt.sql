-- +goose Up
-- OpenClaude sessions can carry a custom system prompt, appended to the
-- CLI's own or, with system_prompt_replace, used instead of it, and an
-- agent definition (Markdown with front matter) started with --agent.
ALTER TABLE sessions ADD COLUMN system_prompt text NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN system_prompt_replace boolean NOT NULL DEFAULT false;
ALTER TABLE sessions ADD COLUMN agent_definition text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE sessions DROP COLUMN agent_definition;
ALTER TABLE sessions DROP COLUMN system_prompt_replace;
ALTER TABLE sessions DROP COLUMN system_prompt;
