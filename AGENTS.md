# Repository Guidelines

The guide for coding agents and contributors. `CLAUDE.md` imports this file, so edit
here.

## What this is

`blogctl`, the command-line client for the rahulbalajee.com Blog API. It drafts posts
for an OAuth machine client: list, read, create and update drafts, and upload images.
It cannot publish, unpublish or delete; the owner does that in the site's studio.
Usage for people and agents is in [docs/agent-workflow.md](docs/agent-workflow.md).

The site (`github.com/rahulbalajee/rahulbalajee.com`) owns the API. Its
`docs/api/blog-api.md`, `docs/api/oauth.md` and `docs/api/blog-api.openapi.json` are
the contract; this repository only consumes it and never changes server behaviour.

## Commands

```sh
make build         # build/blogctl, stamped with the version
make install       # go install into GOBIN
make test          # go vet + go test ./...
make quality       # CI gate: fmt-check, vet, staticcheck, race tests, vulncheck
make sync-contract # refresh the site's contract copies (SITE=../rahulbalajee.com)
```

Go 1.26.9 is the minimum; CI reads it from `go.mod`. Quality tools are pinned in the
Makefile, not `go.mod`. `fmt-check` only checks tracked files, so stage new Go files
first.

## Layout

- `main.go`: process entry only (signals, exit code).
- `internal/cli`: Cobra commands, text and JSON output, exit codes, credential loading.
- `internal/client`: a standard-library HTTP client for the API. No Cobra here.
- `internal/rules`: the API's documented limits (slug, article, image), for local
  checks. The site stays authoritative.
- `internal/blogtest`: a fake API, token endpoint and storage endpoint for tests.

## Invariants

Change these only deliberately, with tests.

- **Credentials:** from `--credentials-fd`, `BLOGCTL_CLIENT_ID`/`BLOGCTL_CLIENT_SECRET`,
  or a credentials file without group/other permissions. Never from arguments. Never
  print a secret, client ID, token or upload form field, including in errors;
  `TestNoCredentialsOrTokensInOutput` scans every command.
- **Tokens:** in process memory only, renewed before expiry, at most one replacement
  after a 401, none after a 403. No disk cache, no refresh grant.
- **Transport:** the base URL is the production origin or a loopback test server, never
  another host, so a misled caller cannot send the credentials elsewhere. HTTPS (plain
  HTTP only for loopback), IPv4 unless `--ipv6`, redirects refused,
  `x-amz-content-sha256` on every body (JSON without HTML escaping), timeouts, bounded
  response sizes. Basic credentials only to `/oauth/token`, the bearer token only to
  `/api/v1`, and nothing from the site to the storage upload URL. A failed token
  exchange is not retried again by the request that needed it.
- **Writes are never replayed.** Unclear outcomes are settled by reading the post back
  (exact content match), else exit 10. A 409 alone is never success: only a create
  whose read-back finds this client's revision-1 post with exactly the same content is.
- **No publish, unpublish or delete commands.** Machine clients are draft-only by design.
- **Contract copies:** `internal/rules/testdata/blog-api.openapi.json` and
  `internal/client/testdata/server/*.json` are copies of the site's files
  (`testdata/SOURCE` names the site commit). Refresh them with `make sync-contract`
  when the site changes the API, and keep `internal/rules` matching; the tests fail
  otherwise.
- **Exit codes** are a contract for agents: 0 ok, 1 error, 2 usage or invalid input,
  3 credentials missing or rejected, 4 forbidden, 5 not found, 6 conflict, 7 rate
  limited, 8 unavailable, 9 blocked at the edge, 10 unclear write outcome.

## Coding style and pull requests

- `gofmt` every file; standard Go conventions; document packages and exported
  declarations; comments explain why. Group imports: standard library, third party,
  then this module.
- Prefer the standard library; Cobra is the one CLI dependency, used only in
  `internal/cli`.
- Every change goes through a PR from a fresh branch off `main`; never push to `main`.
  The owner merges. Never put secrets, tokens or operator IP addresses in commits, PRs,
  logs or chat.
