# blogctl

Command-line client for the [rahulbalajee.com](https://rahulbalajee.com) Blog API, for
people and AI agents (Hermes, Claude Code and others). It drafts posts for an OAuth
machine client: list, read, create and update drafts, and upload images for them.

It cannot publish, unpublish or delete. Machine clients are draft-only by design; the
owner reviews drafts in the site's studio and publishes them there.

```sh
blogctl auth check                                    # can this machine reach the API?
blogctl posts create my-notes --file article.json     # new draft
blogctl posts get my-notes --edit-file edit.json      # read it, with an edit file
blogctl posts update my-notes --file edit.json        # save the edited article
blogctl media upload my-notes chart.png --alt "..."   # image for the draft
blogctl --output json posts list                      # JSON for agents
```

## Install

The repository is private, so Go needs git access to it:

```sh
export GOPRIVATE=github.com/rahulbalajee
go install github.com/rahulbalajee/blogctl@latest
```

Or, in a checkout, `make build` writes `build/blogctl`.

Credentials come from a file only you can read (`~/.config/blogctl/credentials.json`,
mode 600), from `BLOGCTL_CLIENT_ID` and `BLOGCTL_CLIENT_SECRET`, or from an inherited
file descriptor; never from arguments. The API only answers approved IPv4 addresses.

**[Drafting with blogctl](docs/agent-workflow.md)** covers setup, the editing loop,
images, JSON output, exit codes, retries and quotas.

## Contract

The site owns the API. Its
[Blog API](https://github.com/rahulbalajee/rahulbalajee.com/blob/main/docs/api/blog-api.md)
and [OAuth](https://github.com/rahulbalajee/rahulbalajee.com/blob/main/docs/api/oauth.md)
documents are authoritative. This repository keeps copies of the OpenAPI document and
of representative server responses, and its tests fail if the CLI drifts from them;
`make sync-contract` refreshes the copies from a site checkout.

## Development

```sh
make test      # vet and tests
make quality   # CI gate: fmt-check, vet, staticcheck, race tests, vulncheck
```

See [AGENTS.md](AGENTS.md) for the layout and the rules the code keeps.
