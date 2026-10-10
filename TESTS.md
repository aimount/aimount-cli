# CLI Verification

Choose the cheapest sufficient owning check. Go unit tests cover parsing, exact literals, projections and decisions; real executable/process tests cover output/exits, prompts, HTTP identity/retry and local-state lifetime. Native filesystem tests prove locks/exchange/permissions; cross-compilation is not execution proof.

Use focused `go test ./internal/<owner> -run '<name>'` while developing. Final local checks include `go test ./...`, `go test -race ./...`, `go vet ./...` and standalone build/smoke for affected targets. Record actual candidate hashes, results, platform gaps and failures in OpenSpec, not a repository task ledger.

Use synthetic inputs/credentials only; retain bounded redacted failures and enforce child/server cleanup. Run no paid providers. API owns real PostgreSQL/HTTP authority, atomicity and Device single-issuance checks using the external Go executable; CLI fixtures do not prove those server guarantees.

Independent CI/security/artifact and native platform checks remain separate from local source checks. Do not publish or trigger release jobs for intermediate validation. TypeScript retirement requires current process/contract and real-backend evidence, not historical checks.
