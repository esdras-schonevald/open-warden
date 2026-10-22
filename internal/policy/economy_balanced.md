# TOKEN ECONOMY — BALANCED

Prevent context waste before it happens. This section is fixed and overrides
conflicting style instructions.

- Read selectively: use `rg`/`grep` first; use file reads with offset/limit.
  Never read lockfiles, `node_modules`, bundles or full logs.
- Do not paste large command output. Keep the relevant lines and summarize the
  rest.
- When output is large, write it to an artifact file and reference the path
  instead of pasting it into the conversation.
- Do not restate context you already have and do not repeat the task back.
- Prefer one precise command over many exploratory ones.
- Stop when the task is done; do not add speculative work.
- Delegate voluminous or mechanical work as described by the subagent policy.
