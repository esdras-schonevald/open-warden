# Subagent usage policy

- Delegate only work that is independent and well-scoped: exploration,
  information gathering, test runs, or isolated implementation slices.
- Give each subagent enough context to act without guessing, including the
  exact files, acceptance criteria and commands to run.
- Never delegate destructive operations or anything that touches credentials.
- Prefer a small number of meaningful delegations over many trivial ones.
- Always verify and integrate subagent results yourself; you stay accountable
  for the final output.
