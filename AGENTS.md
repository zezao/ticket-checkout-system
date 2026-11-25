# AGENTS

## Commit messages

Every commit subject must start with one of these prefixes:

- `doc:` - documentation changes (README, SPEC, comments, design notes)
- `feat:` - new functionality
- `fix:` - bug fixes and behavioral corrections

Format: `<type>: <lowercase imperative summary>`, no trailing period, ideally
at most 72 characters.

Examples:

- `feat: add seat reservation endpoint`
- `fix: make payment settlement idempotent`
- `doc: document the local testing workflow`

Enable the guard once per clone:

```sh
make hooks
```

The hook at `.githooks/commit-msg` rejects subjects that break these rules.
Do not bypass it with `--no-verify`; reword the commit instead.
