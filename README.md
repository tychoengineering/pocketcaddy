# pocketcaddy

A Caddy HTTP handler that serves named, read-only SQL queries over SQLite
databases. Drop `.sqlite` files in a folder, write `.sql` files with a few
comment directives, and each one becomes an HTTP endpoint that streams JSONL.

- **Read-only.** The module opens each database `mode=ro` with `query_only(1)`.
  A query that attempts a write fails at startup, not at request time.
- **Streaming.** Rows go straight from SQLite to the socket. Memory stays flat
  regardless of result size. A 400,000-row response of 130 MB used 13 MB of
  memory.
- **No SQL injection surface.** The module binds every value. It checks each
  identifier against the query's output columns, then quotes it.
- **Pure Go.** Uses `modernc.org/sqlite`, so `xcaddy` builds need no cgo.

## Building

```sh
xcaddy build --with github.com/tychoengineering/pocketcaddy
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

| Directive | Meaning |
|---|---|
| `@db <name>` | **Required.** Which database to run against. |
| `@name <name>` | Override the query name derived from the filename. |
| `@description <text>` | Shown in the catalog. |
| `@param <name> [type] [required] [= default] [description]` | Declare a bind parameter. |

Types: `text`, `int`, `float`, `bool`, and the list types `text[]`, `int[]`,
`float[]`. A parameter is optional and binds NULL unless you mark it `required`
or give it a default.

Reference a parameter in SQL as `:name`. The parser leaves colons alone inside
string literals, quoted identifiers, comments, and `::` casts.

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

An empty list matches nothing. It does not raise a syntax error, and it does not
match everything.

## Calling a query

**GET** — parameters and PostgREST-style options as query string:

```sh
curl 'localhost:8080/api/posts?status=draft'
```

**POST** — parameters as a JSON body, either bare or wrapped in `params`:

```sh
curl -X POST localhost:8080/api/post \
     -H 'Content-Type: application/json' -d '{"id":7}'

curl -X POST localhost:8080/api/post \
     -H 'Content-Type: application/json' -d '{"params":{"id":7}}'
```

**GET /** on the handler's mount point returns the query catalog as JSONL.

## PostgREST-style filtering

The module applies any query-string key that is not a declared `@param` as a
filter on top of the query's result set. A declared parameter takes precedence
over a filter with the same name.

```sh
curl 'localhost:8080/api/posts?score=gte.40&order=score.desc&select=id,score&limit=3'
```

| Form | SQL |
|---|---|
| `col=eq.v` or `col=v` | `col = v` |
| `col=neq.v` | `col <> v` |
| `col=gt.v`, `gte`, `lt`, `lte` | `col > v`, and so on |
| `col=like.foo%` or `col=ilike.foo%` | `col LIKE 'foo%'` |
| `col=in.(a,b,c)` | `col IN (a, b, c)` |
| `col=is.null`, `is.true`, `is.false` | `col IS NULL`, and so on |
| `col=not.<op>.v` | the negated form |

Repeating a key joins the conditions with `AND`. The shaping options are
`select=a,b` with optional `alias:col` renaming, `order=col.desc` with an
optional `.nullslast`, `limit=N`, and `offset=N`.

The module checks filter, order, and select column names against the query's
output columns on every request. An unknown name returns 400 instead of a
silently empty result.

## Response format

Every response is JSONL (`application/x-ndjson`) — one JSON object per row,
then a metadata record:

```
{"id":1,"title":"Post 1","score":1.5}
{"id":4,"title":"Post 4","score":6}
{"_meta":true,"_rows":2,"_elapsed_ms":0}
```

When a response hits the row cap, the metadata record also carries
`_truncated: true` and `_max_rows`.

An error that happens before the first row returns the matching status code and
a single `{"_error":..., "_status":...}` line. An error after the first row is
different: the status code has already gone out, so the module appends a
trailing `{"_error":...}` record and logs the failure. Check the last line to
confirm a stream finished.

Values keep the JSON types SQLite reports. The module base64-encodes `BLOB`
columns and any other bytes that are not valid UTF-8.
