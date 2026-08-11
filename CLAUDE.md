# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
go test ./...                              # full suite
go test -run TestBindArgsExpandsList ./...  # one test
go test -race ./...                        # trailer tests spin up real HTTP servers
go vet ./...
go build ./cmd/pocketcaddy                 # the shipped binary: Caddy + this handler
```

Run the server against the local `Caddyfile`, which listens on port 8899 and mounts the handler at `/api/*`:

```sh
go run ./cmd/pocketcaddy run --config Caddyfile
```

`.gitignore` excludes `data/`. The Caddyfile expects `data/*.sqlite` and `data/sql/*.sql`. The module refuses to provision when either one is missing. Tests never read that directory. Each test builds its own database under `t.TempDir()`.

Pushing a `v*` tag triggers `.github/workflows/release.yml`, which runs the tests and then GoReleaser. The builds set `CGO_ENABLED=0` because `modernc.org/sqlite` is pure Go. Keep it that way so `xcaddy build` works without a C toolchain.

## Architecture

This module is a Caddy HTTP handler (`http.handlers.pocketcaddy`). It turns a directory of SQLite databases and `.sql` files into read-only JSONL endpoints. Two ideas drive most of the design.

**Fail at startup, not per request.** `Provision` (module.go) opens every database and calls `store.validate`. That method prepares each query, then probes its output columns by wrapping the query in `SELECT * FROM (...) WHERE 0`. The probe fills in `Query.Columns`, and every request-time column check reads it.

A test that builds a `Handler` by hand must call `store.validate` as well. Skip it and `Columns` stays empty, so every column check passes silently. `newTrailerTestHandler` in trailer_test.go shows the correct setup.

**Stream, never accumulate.** `streamQuery` (exec.go) writes one JSON object per row into a 32 KiB buffer and flushes every 256 rows. The row count and the truncation state stay unknown until the last row, so both ride as HTTP trailers set through `http.TrailerPrefix`. The same constraint explains why no `Trailer` header announces those names: Go emits announced trailers only for values set before the first write.

### Request path

`ServeHTTP` (handler.go) → `collectInput` → `bindArgs` → optional `options.buildWrapper` → `streamQuery`.

`collectInput` sorts each query-string key into one of three groups, in this order:

1. A declared `@param` claims the key first.
2. The reserved options `select`, `order`, `limit`, and `offset` come next.
3. Every remaining key becomes a PostgREST filter. Repeated keys join with `AND`.

`bindArgs` (exec.go) binds scalar parameters by name. One list parameter switches the whole statement to positional binds, because mixing named and positional binds is not portable across drivers.

List expansion walks the parsed syntax tree from `github.com/rqlite/sql` and replaces `*sql.BindExpr` nodes. It never rewrites the SQL text. A colon inside a string literal, a comment, a quoted identifier, or a `::` cast is therefore never mistaken for a placeholder. An empty list becomes `NULL`, because `IN ()` is a SQLite syntax error and an empty list must match nothing.

PostgREST shaping wraps the query as a subselect (`options.buildWrapper` in values.go). That wrapper is why column validation matters. Inside a subselect, SQLite reads an unknown bare identifier as a string literal rather than raising an error, so an unchecked typo returns zero rows instead of a 400.

### SQL injection boundary

Values always bind. Identifiers cannot bind, so every column name a client supplies passes two checks and then a quote. `checkColumn` calls `validIdent`, which accepts letters, digits, and underscores with no leading digit. `checkColumn` then confirms the name is a member of `Query.Columns`. Only after both checks does `quoteIdent` render it. Any new code that interpolates an identifier must repeat all three steps.

`openReadOnly` (store.go) enforces read-only access twice per connection. The `mode=ro` flag in the connection string blocks writes at the VFS layer. The `query_only(1)` pragma blocks any statement the connection could otherwise use to change state.

### Errors

Two failure paths produce one shape (error.go). Before the first row, `Handler.fail` writes a status and a single JSONL error line. After the first row the status has already gone out, so `errAfterFirstRow` routes to `failMidStream`. That function appends the same object as a trailing record, drops `status`, and sets `meta.partial`.

The wire shape borrows the JSON:API error vocabulary — `status`, `code`, `title`, `detail`, `source`, and `meta` — but not its document envelope. A stream cannot wrap rows it already flushed.

A `Code` value is the stable contract, and clients branch on it. The `detail` field is prose and may change. A new `Code` needs a matching case in `Code.title()`, which `TestEveryCodeHasATitle` enforces, and a row in the README table.

Startup errors stay plain `error` values rather than `*Error`. They surface through Caddy's config validation and never reach HTTP.

### Parsing `.sql` files

`parseSQLFile` (parse.go) scans the file line by line. A `-- @directive` comment configures the query, and every other line joins the SQL body.

A small lexer (directive.go) tokenizes each `@param` body instead of splitting it on whitespace. Tokens carry a column number, so a quoted default keeps its spaces and commas and an error can point at the offending position.

Filter specs use their own scanner (filter.go). A `.` separates segments only at the top level, never inside `in.(1,2)` or a quoted item.

## Commits

Commit as `tychoengr <tychoengr@gmail.com>`. The repository's `.git/config` already sets this identity, so commits need no `-c` override.

Never attribute a commit to Claude. Leave out any `Co-Authored-By: Claude` line, any "generated with Claude Code" footer, and any mention of Claude, Anthropic, or AI assistance. This applies to the commit message, the branch name, and the pull request body. Write as the author would.

## Writing

`writing-guide.md` governs every word you write here: comments, documentation, commit messages, pull request bodies, and this file. Commit e646ec8 applied it repo-wide, so the existing prose is the working example. Read the guide before writing more than a line or two.

The rules that come up most in this codebase:

- Use the active voice and the present tense. "The probe populates `Columns`," not "`Columns` is populated by the probe."
- Write one idea per sentence. Keep descriptive sentences under 25 words.
- Use one word per concept throughout. This code says *query* for a `.sql` file, *parameter* for a declared `@param`, and *filter* for a PostgREST predicate. Don't rotate in synonyms.
- Cut hidden verbs and empty modifiers: "we validate the column," not "we perform validation of the column."
- Write "for example" and "that is" rather than "e.g." and "i.e."
- Avoid slashes. Write "either a filter or an order term," not "filter/order term."

Comments explain *why* — the constraint that forced the code — rather than what the code does. Several existing comments record subtle SQLite or `net/http` behavior that looks like dead weight until you remove it and a test fails. Preserve that reasoning when you edit.
