# Aimount App Stack CLI

Standalone Go command `aimount` for the App API v0 control plane. No Bun, Node.js,
SDK checkout or API source is needed to run the binary. This is not the shipped
Cloud CLI, Runtime API or Agent API CLI.

## Build and install locally

Go 1.27.3 is the current build baseline. Linux and macOS, amd64 and arm64, are the
supported build targets; Windows is not supported. Building a target is not native
execution evidence for it.

```sh
make build
./dist/aimount help
# Optional local installation, choosing a directory already on your PATH:
install -m 0755 dist/aimount "$HOME/.local/bin/aimount"
make candidates       # Four CGO_ENABLED=0 binaries plus dist/SHA256SUMS
make test             # Unit/process/native tests, race detector and vet
```

Candidate preparation does not publish releases, tags, packages or deployments.

## Commands

Normal workflow (API selection and login are separate):

```sh
mkdir aimount-stack
cd aimount-stack
aimount link                    # Remember Project ID for this directory only
aimount pull                    # Download complete configuration
# Edit models/, tools/, agents/, applications/
aimount plan
aimount apply                   # Review and confirm; there is no push command
aimount pull --force            # Replace local edits with current saved state
```

`--root` defaults to cwd without searching parents. `.aimount/` is local binding
and recovery metadata, not the resource root. `link` downloads nothing; `pull`
preserves that binding. Commands from outside can select the same directory with
`--root ./aimount-stack`.

```sh
aimount                     # Root help; no login required
aimount auth                # Authentication subcommands
aimount auth login --help   # Command-specific flags and examples
aimount auth login --api-url https://api.aimount.io
aimount auth status --output json
aimount auth logout
aimount switch --organization org_0000000000000001
aimount projects list
aimount link --project prj_0000000000000001 --root ./stack
aimount unlink --root ./stack
aimount status --root ./stack
aimount plan --root ./stack
aimount apply --root ./stack --prune --auto-approve
aimount pull --root ./stack --force
aimount pull --root ./stack --recover # Offline recovery only, not a new download
aimount completion zsh      # Generate an offline shell completion script
```

Commands and flags use Cobra. `help [command]`, `-h` and `--help` work at every
level without reading credentials/configuration or contacting the API, even when
target environment values are invalid. Bare `auth` and `projects` show their
subcommands. Completion supports bash, zsh, fish and PowerShell; generated scripts
contain no resource discovery or stored credentials. Install a generated script
using your shell's normal completion setup.

Flags may be interspersed with command arguments. Common flags are `--root`,
`--api-url`, `--project`, `--organization`, `--output text|json`,
`--non-interactive` and `--no-color`. `--prune` belongs to plan/apply;
`--auto-approve` to apply, `--force` to pull and `--yes` to link.

API precedence: `--api-url`, `AIMOUNT_API_URL`, global configuration,
`https://api.aimount.io`. Project precedence: `--project`, `AIMOUNT_PROJECT`,
root `.aimount/project.json`. Explicit `AIMOUNT_TOKEN` overrides the selected
API's saved PAT without fallback. Empty or invalid explicit values fail. Every
authenticated command gets fresh identity and relies on current API membership.
Selecting an Organization does not replace a Project binding or grant authority.
API hostnames use IDNA ASCII canonicalization and numeric default-port removal,
so equivalent Unicode/ASCII hosts and zero-padded ports select the same saved PAT.
Use ordinary DNS/IDNA, dotted decimal IPv4 or bracketed IPv6. Percent-encoded
hostnames and ambiguous numeric forms such as `127.1`, `0177.0.0.1`, `0x7f.1`
and `2130706433` are rejected before transport or credential selection. Replace
these spellings manually; no auth/config migration runs automatically.

Plan/apply strictly parse the complete authored files, then let Stack API resolve
aliases, references, lifecycle and omission/prune against current state.
They do not download a preliminary export. API derives missing retain/remove
credential intentions; new keyed members still need explicit environment-backed
replacement values. Pull retains staged complete-graph validation and atomic
`--force` replacement, including matching local `from_env` and unmanaged files.

Device login requires a real terminal and validated HTTPS, even locally. It opens
the OS browser, prints the manual URL/code, polls serially and refuses redirects.
The standard Go TLS roots support `SSL_CERT_FILE` for a trusted local CA; never
disable certificate validation. SIGINT/SIGTERM cancellation stops local token
publication and attempts the cancellation endpoint once, for at most three
seconds. Cancellation does not revoke a PAT already issued by the server. Lost
token delivery is not retried or replaced automatically.

## Authored files

```text
models/*.yaml
tools/*/tool.yaml
tools/*/contracts/*.yaml
agents/*/agent.yaml
agents/*/instruction.md
applications/*.yaml
```

Every YAML envelope contains `version: 1`, lowercase `kind`, technical `name`,
optional parent `archived`, and `spec`. Agents describe behavior only:
`display_name`, optional `description`, and Tools as names or `{name, contract}`.
Tool `current_contract` selects a discovered `tool_contract`. Models and Plans
belong to Applications. Agent `spec.models`, `agents/*/policies/` directories and
Application `agents` reference arrays from the previous format fail with a
diagnostic; nothing is converted, so move them into `applications/*.yaml` by hand.

Applications use `kind: application`, an immutable technical name and a `spec`
with `display_name`, `models` and `agents` dictionaries, optional `default_model`,
and optional `plans` with `default_plan`. Dictionary keys are Application-local
(`^[a-z][a-z0-9_]{0,62}$`, at most 100 each, no rename or aliases); values refer
to Project Models, Agents, Storages and Tools by technical name. One Project Agent
may appear under several local keys with different settings.

```yaml
version: 1
kind: application
name: paperdesk
spec:
  display_name: Paperdesk Assistant
  models:
    fast:
      model: economy_model
    smart:
      model: reasoning_model
  default_model: fast            # required with two or more Models
  agents:
    form:
      agent: form_assistant
      models:                    # explicit set; does not follow later additions
        - fast
      storages:
        - document_templates
    main:
      agent: main_assistant      # omitted models inherit every Application Model
      storages:
        - product_documentation
      memory: read_write         # none (default), read or read_write
  default_plan: free             # required with two or more Plans
  plans:
    free:
      agents:
        main:
          input: false
      limits:
        - agents:
            - form
          requests: 10
          period: 1m             # positive s/m/h/d window, at most 30d
    plus:
      display_name: Plus
      agents:
        main:
          models:
            allowed:
              - fast
          default_model: fast
          tools:
            allowed:             # empty list denies every Tool
              - documents_read
          memory: read           # narrows the local Agent ceiling
  tool_endpoint:
    url: https://tools.paperdesk.example/aimount   # or from_env: VARIABLE
```

Local validation checks structure, key grammar, unique sets, bounds, that every
local key reference names a key of the same Application, and effective defaults.
A local Agent default is its explicit `default_model` (inside its `models`), else
`spec.default_model` when in its set, else its sole Model. A Plan `models.allowed`
narrows that set; a Plan `default_model` must be in the local Agent's Models and,
unless `input: false`, in `models.allowed`. Excluding the inherited default needs
an explicit Plan `default_model` unless `input: false`; there is no sole-key
fallback at Plan level. Local Agent `memory` (`none`, `read` or `read_write`;
omitted means `none`) is its User Memory ceiling, and a Plan `memory` only narrows
it; see `app/reference/memory.md` in the docs repository. The API decides Project
references, lifecycle and Plan
Tool membership in the complete resulting graph. Plans have no `archived` field and
the file carries no Users, assignments, Memory records or runtime data. Pull sorts
dictionaries and sets, and keeps omitted local Agent `models`/`default_model`
omitted, since only omission inherits later Application Models. Only fixed-depth `.yaml` files
participate; `.yml` and deeper examples remain unmanaged. An empty Application
uses `models: {}` and `agents: {}`.

Optional `tool_endpoint` receives every `server_webhook` Tool call of the
Application's local Agents; the receiver routes by `tool.id` or `tool.name`. Use
exactly one of `url: https://...` (no credentials, query or fragment) or
`from_env: VARIABLE`. A present variable sends its URL; a missing one keeps the
saved URL. Pull never writes an env-backed URL: it keeps a matching local
`from_env`, otherwise it writes `AIMOUNT_APPLICATION_<length>_<NAME>_13_TOOL_ENDPOINT_URL`
(for `paperdesk`: `AIMOUNT_APPLICATION_9_PAPERDESK_13_TOOL_ENDPOINT_URL`).
Signing secrets are managed in the App and never appear in files, plans or pull
output; the CLI has no secret commands.

Plan output shows changed fields, keys and set differences rather than list
positions, plus the effective Model set of local Agents that inherit
`spec.models`. Local Agent changes apply to queued inputs when their Run starts;
removing a key or Plan keeps Sessions, history and accounting, while tokens and
queued inputs bound to a removed Plan are refused.

Model credentials use `{api_key: {from_env: VARIABLE}}` or `{none: {}}`.
A `server_webhook` Contract's optional `webhook` holds only `timeout_seconds`
(1..30, default 10) and `invocation_name`; it has no URL, auth or token.
Present credential values go only into request credential intentions. Missing values retain existing keys;
new keyed routes require a value. Keys never enter pull output or diagnostics.

Unknown/duplicate fields, anchors, aliases, tags, unsupported schemas, missing
instructions, orphan owned files and empty roots fail. Integer fields reject
genuine fractions before conversion; schema numeric tokens remain exact.
`project.yaml` is unsupported for plan/apply and preserved by pull. All discovered
resources are authoritative: omitted saved parents need explicit irreversible
`--prune`, independent of approval. Renames remain Console/API operations;
existing aliases are resolved through fresh server bindings.
Applications have no aliases: changing their name declares a different resource.
Application omission requires `--prune` and authorizes erasure of its Users,
conversations, files and configuration. Agent prune erases only its configuration,
preserving ordinary Application history. Removing a local Agent key needs no
prune; it does not erase conversations. Physical cleanup remains asynchronous.

Stack requests and export require an explicit `applications` array, even when
empty, alongside `models`, `tools` and `agents`. Applications also participate in
the complete identity bindings and resource ETag read-set; runtime keys, Users
and Sessions never enter Stack configuration. Older incomplete Stack graphs are
not partial-management inputs for this CLI/API contract.

Tool Schema `pattern` and every `patternProperties` key use the Go standard
library [`regexp` syntax](https://pkg.go.dev/regexp/syntax), not ECMAScript.
Supported pattern text stays unchanged; lookahead, lookbehind, backreferences
and malformed expressions fail with a file-bound safe explanation that never
prints the expression. This is an intentional authored-format change from the
TypeScript CLI: repair affected patterns manually before plan/apply or pull.
An exported unsupported pattern makes pull fail during staging validation,
leaving the original root intact. Go-only syntax is locally supported, not a
promise that another server accepts it. No translation, dropped constraints or
compatibility engine is provided.

## Local state and recovery

Versionless `auth.json` and `config.json` remain in:

- macOS: `$HOME/Library/Application Support/aimount/`
- Linux: `${XDG_CONFIG_HOME:-$HOME/.config}/aimount/`

JSON publication is atomic; files are mode `0600`. Logout removes only the
selected API's local authorization, not server PATs. Login/logout read fresh
auth state under a short nonblocking native descriptor flock after network waits.
The mode-0600 `auth.json.lock` inode persists; never unlink it during use. Lock
contention exits 2 and requires explicit retry. Link only writes the binding and
its `.gitignore` entry; unlink only removes the binding.

Pull keeps the Stack root directory in place, including when invoked inside it.
The persistent mode-0600 `.aimount/pull.lock` serializes pull and plan/apply across
their complete resource-file use. Each managed file is published by atomic rename;
the complete tree is not an atomic snapshot for external readers. Unmanaged files,
binding, relative symlinks and matching environment locators stay in place. Managed
symlink ancestors are refused; staging validation and detected managed-file edits abort
before publication. Ordinary write errors attempt rollback.

Change detection covers managed contents, permissions and fixed-depth discovery
membership only. Edits to unrelated files do not abort pull; those files are never
read or replaced for publication. The validated `next/` tree and backups share one
prepared journal directory, activated only after validation and conflict checks.

Different managed paths that resolve to the same physical file (case-only aliases
on macOS or hard links) are refused before publication. Rename such files to an
unambiguous temporary spelling before pulling; CLI does not guess a case-only
rename or weaken recovery conflict checks. Resource identity still comes from the
authored technical name, not its filename.

An interrupted publication leaves `.aimount/pull` with before-state backups and a
journal. Ordinary pull/plan/apply refuse that state. Run `aimount pull --recover`
in the same root to restore pre-pull files, or finalize already committed cleanup;
it runs offline without credentials and never starts a new download. Recovery
refuses unknown content or permission changes after interruption rather than overwriting them: keep backups
and inspect the conflict. Never delete the journal to bypass this guard. Temporary
`.aimount/.pull-prepare-*`/`.pull-cleanup-*` leftovers are not active
journals; inspect before manual removal. Arbitrary editors do not join CLI locks.

JSON mode emits one version-1 automation envelope and never prompts. Exits:
success `0`; validation/auth/network `1`; concurrency `2`;
confirmation/cancellation `3`. `CI=true`, a nonterminal stdin, JSON mode or
`--non-interactive` disable prompts.

Text mode shows context summaries, a Project table, managed file names and apply
results. Errors/warnings are plain text on stderr, not JSON string literals;
unsafe terminal control characters in data are escaped. Explicit `--output json`
keeps the machine contract, including help/completion as success-envelope strings.
The shell-internal `__complete` protocol stays raw regardless of `--output`; this
exception is for generated completion scripts, not ordinary command automation.

Apply retains one route/body/read-set/server-Date-derived `s1` key in process
memory. Unknown outcomes offer only explicit identical retry after Retry-After
interactively; there is no automatic resend, replan, rekey or receipt journal.
Leaving loses this memory and does not cancel or roll back server effects.
Noninteractive unknown outcomes fail without another mutation.

API-owned real PostgreSQL/HTTP tests use `AIMOUNT_TEST_APP_CLI` as the absolute
built executable (`TestStackCLIRealBackend`, `TestDeviceRFCClientBackendFlow`).
CLI process tests accept the same variable to qualify a prebuilt candidate without
a Go compiler; without it they build the local source. For example:

```sh
AIMOUNT_TEST_APP_CLI="$PWD/dist/aimount-darwin-arm64" go test ./cmd/aimount -count=1
```

CLI fixtures do not prove server authority, transaction atomicity or single
issuance. See [ARCHITECTURE.md](ARCHITECTURE.md), [TESTS.md](TESTS.md) and
[REVIEW.md](REVIEW.md) for owning standards.

## Dependencies and license

MIT, copyright 2026 Aimount; see [LICENSE](LICENSE). Maintained YAML Node parsing
uses `go.yaml.in/yaml/v3` (MIT/Apache-2.0); JSON Schema grammar uses
`github.com/santhosh-tekuri/jsonschema/v6` (Apache-2.0), and native OS calls use
`golang.org/x/sys` (BSD-3-Clause). API hostname canonicalization uses
`golang.org/x/net/idna` (BSD-3-Clause), rather than a custom Punycode implementation.
Schema compilation uses a detached numeric
grammar projection, never replacing original data tokens. Dependencies are
pinned in `go.mod`/`go.sum`. Cobra (Apache-2.0) and pflag (BSD-3-Clause) own the
command/help/flag shell; no Viper, generator or API module is imported.
