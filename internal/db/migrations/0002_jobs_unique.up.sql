-- Prevent duplicate job definitions: a job is uniquely identified by the
-- combination of its name, command, and schedule expression.
ALTER TABLE jobs
    ADD CONSTRAINT jobs_name_command_schedule_key UNIQUE (name, command, schedule_expr);
