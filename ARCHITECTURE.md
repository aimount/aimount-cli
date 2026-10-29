# CLI Architecture

`cmd/aimount` owns process setup, signal cancellation and exit. Internal command orchestration owns existing CLI scenarios; HTTP transport, authored resource files and local state each have their concrete responsibility. Create only packages needed by actual dependencies, not a command bus, generic repository or public SDK.

Cobra owns the command/flag/help tree in `internal/cli/command.go`, producing the
existing `Options` for operation dispatch. Help/completion run offline before
target/environment/auth validation. Keep parser diagnostics secret-safe rather
than forwarding Cobra/pflag errors containing supplied argument values. Explicit
string flags reject empty/repeated values; command-local flags retain their
interspersed placement and ownership restrictions. No parallel handwritten parser,
Viper configuration layer or command-per-file scaffolding is needed.

Hidden shell completion is a protocol boundary, not an operational command:
preflight its arguments through the same Cobra/pflag tree before internal flag
replay, never enter Cobra's raw argument-bearing completion diagnostics, and
keep the directive response raw even after a user-supplied JSON output flag.

`run.go` owns one JSON envelope and existing exit categories; `output.go` owns
human text summaries/tables and control-character escaping. Human presentation
must not change operation results, HTTP requests, auth state or apply retries.

The App API is authoritative for current identity/membership, coherent saved configuration, alias/reference/Policy/lifecycle and omission decisions, semantic planning, atomic effects and replay. The CLI strictly parses complete authored files and explicit environment replacements, sends them directly to the server planner without preliminary export/bindings reads, shows the plan/target, collects explicit consent and sends the approved request once. API infers default retain/remove credential intentions; new keyed members still require replacement material. Unknown apply outcome retains exact route/body/key/read-set in memory for manual identical retry; no replan/rekey/automatic mutation retry.

The concrete client `stack` method owns export fetching, consumed-field validation and safe projection. It returns `graph` directly to pull and plan/apply; commands do not repackage or revalidate that same response. File parsing/rendering remains a separate authored-data boundary.

Plan response read-sets validate consumed resource/binding/credential and membership/Project epoch shapes, preserving exact int64 counters and opaque preconditions while dropping additive fields before output or capture. JSON syntax uses stdlib jsontext before a UseNumber decode. Endpoint canonicalization shares one API key for transport and saved authorization, using numeric ports and maintained IDNA conversion; encoded hostnames and noncanonical numeric IPv4 aliases fail rather than being reinterpreted. YAML scalar classification uses the original YAML 1.2 token, not legacy resolver tags or machine-width conversions; the same classifier determines which rendered string values and keys need quotes.

Keep external I/O cancellable and bounded, refuse credential-bearing redirects, close bodies and render safe errors. Auth and bindings keep their existing locations/shapes. YAML/JSON Schema numeric values must remain lossless; strict authored/local-state parsing and tolerant response projections are different boundaries. Operational secrets never enter exported files, plans or diagnostics.

Tool Schema `pattern` and `patternProperties` keys deliberately use the Go standard library `regexp` dialect, not the former ECMAScript grammar. Validate both in the existing schema checker without modifying the original pattern, and never include the compiler's pattern-bearing error in diagnostics. Unsupported lookaround/backreferences and malformed expressions require manual author repair; no compatibility engine, automatic translation or dropped constraint is allowed. Pull must reject these during staged-root validation before native publication. Go-only syntax is a local capability, not a claim about another server's grammar.

Linux/macOS local state uses secure atomic file writes and fresh auth-map updates under native descriptor locks. Pull validates staging then publishes through one native whole-directory exchange with no per-file fallback. Unsupported capabilities fail with old content intact; arbitrary editors do not join CLI locks.
