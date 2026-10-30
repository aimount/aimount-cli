# Aimount CLI

- This repository owns the standalone Go App Stack CLI, command `aimount`; it does not own the Cloud CLI, runtime SDK or API authorization/storage.
- Follow [ARCHITECTURE.md](ARCHITECTURE.md), [TESTS.md](TESTS.md) and [REVIEW.md](REVIEW.md). [README.md](README.md) owns usage and support.
- In the Aimount workspace, resolve shared policy from logical `wspace/services/AGENTS.md`, not physical worktree nesting. Development writes belong to repo-local `.worktrees/<change>` on `change/<change>`; primary is read-only except sync.
- Active scope/evidence belongs to the non-archived workspace OpenSpec change. Preserve current CLI commands, strict authored input, tolerant response readers, secret redaction, exact manual apply retry and stable-root pull with journaled per-file publication/recovery.
- Prefer standard Go packages and direct functions over speculative abstractions. Use maintained protocol libraries for YAML/JSON Schema, not hand-rolled interpreters.
