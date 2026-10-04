# Mixed line endings

- **When to read:** Before editing any file in this repository, especially through an agent or editor that rewrites whole files.

## Quick Reference

**Gotcha:** The working tree mixes LF files, CRLF files, and files with both (for example `docs/architecture.md`, `config.example.yaml`). Whole-file editing tools can silently convert a file to one style, which makes `git diff` show every line as changed. HEAD does not reveal the per-line mix of uncommitted files, so a lost mix cannot be rebuilt from Git.

**Action:**

1. Record each file's CRLF and LF-only line counts before editing, and compare afterwards:
   `([regex]::Matches([IO.File]::ReadAllText($p),"`r`n")).Count` and `([regex]::Matches([IO.File]::ReadAllText($p),"(?<!`r)`n")).Count`.
2. Copy files to a scratch snapshot before delegating edits, so a converted file can be rebuilt by reapplying only the content change.
3. Recount delegated edits against your own snapshot; a subagent's self-reported "before" counts may already reflect its own conversion.
4. `gofmt -l` reports every CRLF Go file; check formatting with `tr -d '\r' < file | gofmt -l` instead.
