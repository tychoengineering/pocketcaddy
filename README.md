<p align="center">
  <img src="logo.png" alt="pocketcaddy" width="200">
</p>

<h1 align="center">PocketCaddy</h1>

A web server that turns SQLite databases into read-only JSON APIs. Drop
`.sqlite` files in a folder, write `.sql` files with a few comment directives,
and each one becomes an HTTP endpoint that streams JSONL.

Ships as a single binary with no dependencies, and as a
[Caddy](https://caddyserver.com) handler you can add to an existing build.

- **Read-only.** The module opens each database `mode=ro` with `query_only(1)`.
  A query that attempts a write fails at startup, not at request time.
- **Streaming.** Rows go straight from SQLite to the socket. Memory stays flat
  regardless of result size. A 400,000-row response of 130 MB used 13 MB of
  memory.
- **No SQL injection surface.** The module binds every value. It checks each
  identifier against the query's output columns, then quotes it.
- **Pure Go.** Uses `modernc.org/sqlite`, so the binary is static and builds
  need no cgo.

## Installing

Install the `pocketcaddy` binary. It needs no runtime, no Go toolchain, and no
separate Caddy — the server is compiled in.

```sh
curl -fsSL https://raw.githubusercontent.com/tychoengineering/pocketcaddy/main/install.sh | sh
```

Or with Homebrew:

```sh
brew install tychoengineering/tap/pocketcaddy
```

Both cover macOS arm64 and Linux amd64. The script picks the matching build,
checks it against the release checksums, and installs to `/usr/local/bin`. Set
`INSTALL_DIR` to install elsewhere and `POCKETCADDY_VERSION` to pin a release.

### Running it

Point a Caddyfile at a directory of databases and queries, then run it:

```sh
pocketcaddy run --config Caddyfile
```

The binary is Caddy with this handler registered, so every Caddy command works
— `run`, `start`, `stop`, `reload`, `validate`, `fmt` — and a Caddyfile may mix
`pocketcaddy` with any other directive. [Caddyfile](#caddyfile) covers the
configuration, and [Writing a query](#writing-a-query) covers the `.sql` files.

The macOS binary is unsigned. The install script and Homebrew both clear the
quarantine attribute, so Gatekeeper stops only an archive you downloaded
through a browser. Clear it with `xattr -dr com.apple.quarantine pocketcaddy`.

## Building from source

Building needs Go 1.25.5 or later. The build is pure Go, so it needs no C
toolchain and runs with `CGO_ENABLED=0`.

Install the binary from source, on any platform Go targets:

```sh
go install github.com/tychoengineering/pocketcaddy/cmd/pocketcaddy@latest
```

Add the handler to an existing Caddy build instead, alongside other plugins:

```sh
xcaddy build --with github.com/tychoengineering/pocketcaddy
```

Or work on the module itself:

```sh
git clone https://github.com/tychoengineering/pocketcaddy
cd pocketcaddy
go test ./...
go build ./cmd/pocketcaddy
```

## Layout

```
data/
  app.sqlite          -> database name "app"
  reports.sqlite      -> database name "reports"
  sql/
    posts.sql         -> query name "posts"
    admin/stats.sql   -> query name "admin/stats"
```

The database name is the filename without its extension. The module recognizes
`.sqlite`, `.sqlite3`, and `.db`. The query name is the path under `sql/`
without the extension, unless `@name` overrides it.

## Caddyfile

```caddyfile
:8080 {
	handle_path /api/* {
		pocketcaddy {
			root           data
			sql_dir        data/sql
			max_rows       100000   # -1 for unlimited
			max_conns      4
			timeout        30       # seconds
			max_body_bytes 1048576
		}
	}
}
```

All subdirectives are optional; `sql_dir` defaults to `<root>/sql`.

## Writing a query

Directives are `--` line comments. Everything else is the SQL body.

```sql
-- @db app
-- @description List posts by status
-- @param status text = active        Status to match
-- @param min_score float = 0         Minimum score

SELECT id, title, status, score
FROM posts
WHERE status = :status
  AND score >= :min_score
ORDER BY id;
```

| Directive                                                   | Meaning                                            |
| ----------------------------------------------------------- | -------------------------------------------------- |
| `@db <name>`                                                | **Required.** Which database to run against.       |
| `@name <name>`                                              | Override the query name derived from the filename. |
| `@description <text>`                                       | Shown in the catalog.                              |
| `@param <name> [type] [required] [= default] [description]` | Declare a bind parameter.                          |

Types: `text`, `int`, `float`, `bool`, and the list types `text[]`, `int[]`,
`float[]`. A parameter is optional and binds NULL by default. Mark it
`required` or give it a default to change that.

Double-quote a default that contains spaces or commas. Write a list default
either bare or in parentheses:

```sql
-- @param greeting text = "hello, world"
-- @param ids int[] = (1, 2, 3)
```

Reference a parameter in SQL as `:name`. The module parses each query to a
syntax tree and substitutes placeholders on that tree, so it never mistakes a
colon inside a string literal, quoted identifier, comment, or `::` cast for a
parameter. A query that does not parse fails at startup.

## List parameters

Write a single placeholder inside parentheses; it expands to one bind per
element at request time.

```sql
-- @db app
-- @param ids int[] required
-- @param authors text[] = user0,user1

SELECT id, title FROM posts
WHERE id IN (:ids) AND author IN (:authors);
```

Three accepted input forms:

```sh
curl 'localhost:8080/api/posts_by_ids?ids=1&ids=2&ids=7'   # repeated
curl 'localhost:8080/api/posts_by_ids?ids=1,2,7'           # comma-separated
curl -X POST localhost:8080/api/posts_by_ids \
     -H 'Content-Type: application/json' -d '{"ids":[1,2,7]}'
```

An empty list matches zero rows. The module expands it to `IN (NULL)`, which
sidesteps the SQLite syntax error on `IN ()` and stays empty rather than
widening to match everything.

## Calling a query

**GET** — parameters and PostgREST-style options as query string:

```sh
curl 'localhost:8080/api/posts?status=draft'
```

**POST** — parameters as a flat JSON object, one key per declared `@param`:

```sh
curl -X POST localhost:8080/api/post \
     -H 'Content-Type: application/json' -d '{"id":7}'
```

Any key that is not a declared parameter is an error; there is no envelope.

**GET /** on the handler's mount point returns the query catalog as JSONL.

## PostgREST-style filtering

The module applies any query-string key that is not a declared `@param` as a
filter on top of the query's result set. A declared parameter takes precedence
over a filter with the same name.

```sh
curl 'localhost:8080/api/posts?score=gte.40&order=score.desc&select=id,score&limit=3'
```

| Form                                 | SQL                      |
| ------------------------------------ | ------------------------ |
| `col=eq.v` or `col=v`                | `col = v`                |
| `col=neq.v`                          | `col <> v`               |
| `col=gt.v`, `gte`, `lt`, `lte`       | `col > v`, and so on     |
| `col=like.foo%` or `col=ilike.foo%`  | `col LIKE 'foo%'`        |
| `col=in.(a,b,c)`                     | `col IN (a, b, c)`       |
| `col=is.null`, `is.true`, `is.false` | `col IS NULL`, and so on |
| `col=not.<op>.v`                     | the negated form         |

Repeating a key joins the conditions with `AND`. Two options shape the result:
`select=a,b`, which takes an optional `alias:col` renaming, and `order=col.desc`,
which takes an optional `.nullslast`. The next section covers paging.

The module checks filter, order, and select column names against the query's
output columns on every request. An unknown name returns 400 instead of a
silently empty result.

## Pagination

`limit=N` and `offset=N` page through a result set. Both must be non-negative
integers; anything else returns 400. Either works without the other — a bare
`offset` skips rows and returns the rest.

```sh
curl 'localhost:8080/api/posts?limit=100'             # first page
curl 'localhost:8080/api/posts?limit=100&offset=100'  # second page
```

Every response carries a `Content-Range` trailer describing the rows it
actually covers, so a client can page without tracking offsets itself:

```
Content-Range: 100-199/*     # offset 100, 100 rows
Content-Range: */*           # no rows — past the end
```

The total is always `*`. Computing an exact count would require a second
`COUNT(*)` per request, which the streaming design deliberately avoids, so
detect the last page by reading fewer rows than you asked for, or by `*/*`.

Pair `limit` with a deterministic `order`, or with an `ORDER BY` in the query
itself. SQLite guarantees a stable row order across requests only when you do,
so pages can otherwise overlap or skip rows.

The `max_rows` cap still applies and overrides a larger `limit`: with
`max_rows 3`, a `limit=5` returns 3 rows and sets `X-Truncated: true`. Keep
`limit` at or below the cap so paging is not silently short.

## Response format

Every response is JSONL (`application/x-ndjson`) — one JSON object per row,
nothing else:

```
{"id":1,"title":"Post 1","score":1.5}
{"id":4,"title":"Post 4","score":6}
```

The module reports row counts and truncation as HTTP trailers, because it knows
neither one until it writes the last row:

| Trailer             | Meaning                                                                          |
| ------------------- | -------------------------------------------------------------------------------- |
| `Content-Range`     | Rows covered, as `offset`-first..last out of `*`. See [Pagination](#pagination). |
| `X-Truncated: true` | The response hit the row cap. Absent otherwise.                                  |
| `X-Max-Rows: N`     | The cap that applied, sent with `X-Truncated`.                                   |

```sh
curl --raw -D - 'localhost:8080/api/posts?offset=10&limit=2'
```

Trailers arrive after the body. A client must therefore keep chunked transfer
encoding on, and must read the response to completion before it reads the
trailers. Many HTTP clients discard trailers silently — if yours does, a
truncated response looks exactly like a complete one.

## Errors

An error is a single JSONL line holding one `error` object, in the same
`application/x-ndjson` stream as the rows. The fields follow [JSON:API's error
vocabulary](https://jsonapi.org/format/#error-objects) — `status`, `code`,
`title`, `detail`, `source`, `meta` — but drop its document envelope. A
streamed response cannot use that envelope: the module has already flushed the
rows by the time it detects a mid-stream failure, so the error becomes one more
line instead of a wrapper around the lines before it.

```json
{
  "error": {
    "status": "400",
    "code": "unknown_operator",
    "title": "Unknown filter operator",
    "detail": "unknown filter operator \"gtt\"",
    "source": { "parameter": "score" },
    "meta": { "operators": ["eq", "gt", "gte", "ilike", "in", "is", "like", "lt", "lte", "neq"] }
  }
}
```

Branch on `code`, never on `detail`: codes are stable, details name the
offending value and may change.

| Code                     | Status | Raised when                                                          |
| ------------------------ | ------ | -------------------------------------------------------------------- |
| `method_not_allowed`     | 405    | The route does not accept that verb.                                 |
| `unsupported_media_type` | 415    | A POST body is not `application/json`.                               |
| `invalid_body`           | 400    | The body is not a JSON object, or a value will not decode.           |
| `body_too_large`         | 413    | The body exceeds `max_body_bytes`.                                   |
| `unknown_parameter`      | 400    | A key is not a declared `@param`.                                    |
| `missing_parameter`      | 400    | A `required` parameter was omitted.                                  |
| `invalid_type`           | 400    | A value does not fit its declared type.                              |
| `unknown_column`         | 400    | A filter, order, or select names a column the query does not return. |
| `invalid_column`         | 400    | A column name is not a valid identifier.                             |
| `unknown_operator`       | 400    | A filter uses an unrecognized operator.                              |
| `invalid_filter`         | 400    | A filter spec is malformed.                                          |
| `invalid_order`          | 400    | An `order` modifier is unrecognized.                                 |
| `invalid_select`         | 400    | A `select` term is malformed or empty.                               |
| `invalid_range`          | 400    | `limit` or `offset` is not a non-negative integer.                   |
| `query_failed`           | 500    | The database rejected or failed the query.                           |
| `timeout`                | 504    | Execution exceeded `timeout`.                                        |

`source` locates the offending input: `parameter` for a query-string key,
`pointer` for a JSON Pointer into a POST body. It names the key the client
actually sent. A bad column in `?order=scores.desc` therefore reports `order`,
because `order` is the key, and puts `scores` in `meta.column`. Where a fix is
enumerable, `meta` carries the alternatives: `operators`, `columns`, and
`declared`, which lists the query's parameter names.

`query_failed` includes the SQLite message in `detail`. You write the queries
yourself, and the module opens every database read-only, so this message
diagnoses your own SQL rather than disclosing an untrusted schema.

An error after the first row is different: the status code has already gone out,
so the module appends the same `error` object as a trailing record, omitting
`status` and setting `meta.partial` with the row count, then logs the failure.
One client-side check therefore covers both cases.

```
{"id":1,"title":"first"}
{"id":2,"title":"second"}
{"error":{"code":"query_failed","title":"Query execution failed","detail":"disk I/O error","meta":{"partial":true,"rows":2}}}
```

A partial response still carries a `Content-Range` trailer covering the rows
that made it out.

Values keep the JSON types SQLite reports. The module base64-encodes `BLOB`
columns and any other bytes that are not valid UTF-8.
