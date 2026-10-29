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
# Run pull outside the directory being replaced:
aimount pull --root ./stack --force
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
aliases, references, lifecycle, Policies and omission/prune against current state.
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
agents/*/policies/*.yaml
applications/*.yaml
```

Every YAML envelope contains `version: 1`, lowercase `kind`, technical `name`,
optional parent `archived`, and `spec`. Agent Models use ordered names; Tools use
names or `{name, contract}`. Tool `current_contract` selects a discovered
`tool_contract`. Policy Models contain `default` and optionally `allowed` or
`denied`; Tool filters also allow one of these arrays.

Applications use `kind: application`, an immutable technical name and only
`display_name` plus a required, possibly empty `agents: [{name: main}]` set in
`spec`. Connections refer to Agents defined in the complete local graph; they
have no default responder or ordering. Pull sorts them by current primary name.
Only fixed-depth `.yaml` files participate; `.yml` and deeper examples remain
unmanaged. For example, an Application can exist before any Agent is connected:

```yaml
version: 1
kind: application
name: product
spec:
  display_name: Product assistant
  agents: []
```

Model credentials use `{api_key: {from_env: VARIABLE}}` or `{none: {}}`.
Webhook URL uses `{value: https://...}` or `{from_env: VARIABLE}`; auth uses
`{bearer_token: {from_env: VARIABLE}}` or `{none: {}}`. Present credential values
go only into request credential intentions. Missing values retain existing keys;
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
conversations, files and configuration. Agent prune erases only its owned private
tests/files and configuration, preserving ordinary Application history. Detaching
an Agent connection needs no prune; it does not erase conversations or alone
cancel accepted work. Physical cleanup remains asynchronous.

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

Pull holds a sibling `.ROOT.aimount-pull.lock` throughout inspection, network
reads, staging and one native whole-directory exchange. It preserves unmanaged
files, relative symlinks, permissions and matching environment locators. Managed
symlink ancestors are refused. Staging validation or detected local edits abort
before publication; unavailable native exchange leaves the root intact, without
a per-file fallback. A hard kill may leave the sibling lock/staging: inspect
them and recover explicitly. Arbitrary editors do not participate in CLI locks.

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
