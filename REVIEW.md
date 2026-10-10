# CLI Review

In the workspace, also follow logical root `wspace/REVIEW.md`; do not resolve it from physical service-worktree nesting. Review the full change and affected callers against accepted OpenSpec and the current CLI contract; report findings, actual checks and gaps before claiming completion.

Check command/format/output compatibility, precise numbers and strict authored input, tolerant consumed responses, target/credential separation, safe Device timing/cancellation, exact manual apply recovery and secret-free diagnostics. Check native lock/atomic-pull behavior, local permissions/symlinks, bounded cancellation/cleanup and no loss of unrelated state. Prefer deletion/reuse/stdlib over new layers. Source movement or fixture success does not prove backend authority or native platform execution.
