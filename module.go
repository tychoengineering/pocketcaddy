// Package pocketcaddy implements a Caddy HTTP handler that serves named,
// read-only SQL queries over SQLite databases.
//
// Databases are discovered as *.sqlite files in a data directory; queries are
// .sql files in data/sql/ annotated with `-- @db` and `-- @param` directives.
// Results always stream as JSONL.
package pocketcaddy

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(Handler{})
	httpcaddyfile.RegisterHandlerDirective("pocketcaddy", parseCaddyfile)
	// Order it alongside other content-producing handlers so it can be used
	// directly inside handle/handle_path without a route block.
	httpcaddyfile.RegisterDirectiveOrder("pocketcaddy", httpcaddyfile.Before, "file_server")
}

// Default limits, overridable from the Caddyfile.
const (
	defaultMaxRows    = 100_000
	defaultMaxConns   = 4
	defaultTimeoutSec = 30
	defaultBodyLimit  = 1 << 20 // 1 MiB of JSON params is already absurd
)

// Handler serves named SQL queries from a directory of SQLite databases.
type Handler struct {
	// Root is the data directory containing *.sqlite files. Default "data".
	Root string `json:"root,omitempty"`

	// SQLDir holds the .sql query files. Defaults to <root>/sql.
	SQLDir string `json:"sql_dir,omitempty"`

	// MaxRows caps rows streamed per request. Unset uses the default; -1
	// disables the cap entirely.
	MaxRows int64 `json:"max_rows,omitempty"`

	// MaxConns is the per-database connection-pool size.
	MaxConns int `json:"max_conns,omitempty"`

	// TimeoutSec bounds a single query's execution.
	TimeoutSec int `json:"timeout,omitempty"`

	// MaxBodyBytes caps the JSON request body for POST.
	MaxBodyBytes int64 `json:"max_body_bytes,omitempty"`

	store *store
	log   *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.pocketcaddy",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision opens the databases and loads every query file.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.log = ctx.Logger(h)

	if h.Root == "" {
		h.Root = "data"
	}
	if h.SQLDir == "" {
		h.SQLDir = filepath.Join(h.Root, "sql")
	}
	if h.MaxRows == 0 {
		h.MaxRows = defaultMaxRows
	}
	if h.MaxRows < 0 {
		h.MaxRows = 0 // streamQuery treats 0 as unlimited
	}
	if h.MaxConns <= 0 {
		h.MaxConns = defaultMaxConns
	}
	if h.TimeoutSec <= 0 {
		h.TimeoutSec = defaultTimeoutSec
	}
	if h.MaxBodyBytes <= 0 {
		h.MaxBodyBytes = defaultBodyLimit
	}

	st, err := openStore(h.Root, h.SQLDir, h.MaxConns)
	if err != nil {
		return err
	}

	// Fail fast on bad SQL rather than at the first request that hits it.
	vctx, cancel := context.WithTimeout(ctx, time.Duration(h.TimeoutSec)*time.Second)
	defer cancel()
	if err := st.validate(vctx); err != nil {
		st.Close()
		return err
	}

	h.store = st
	h.log.Info("pocketcaddy ready",
		zap.Strings("databases", st.dbNames()),
		zap.Int("queries", len(st.queries)),
	)
	return nil
}

// Cleanup closes all open databases.
func (h *Handler) Cleanup() error {
	if h.store != nil {
		return h.store.Close()
	}
	return nil
}

// Validate checks the handler configuration.
func (h *Handler) Validate() error {
	if h.TimeoutSec < 0 {
		return fmt.Errorf("timeout cannot be negative")
	}
	if h.MaxConns < 0 {
		return fmt.Errorf("max_conns cannot be negative")
	}
	return nil
}

// UnmarshalCaddyfile parses the pocketcaddy directive:
//
//	pocketcaddy {
//	    root           data
//	    sql_dir        data/sql
//	    max_rows       100000
//	    max_conns      4
//	    timeout        30
//	    max_body_bytes 1048576
//	}
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		if d.NextArg() {
			h.Root = d.Val()
		}
		if d.NextArg() {
			return d.ArgErr()
		}

		for d.NextBlock(0) {
			switch d.Val() {
			case "root":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.Root = d.Val()
			case "sql_dir":
				if !d.NextArg() {
					return d.ArgErr()
				}
				h.SQLDir = d.Val()
			case "max_rows":
				if !d.NextArg() {
					return d.ArgErr()
				}
				n, err := strconv.ParseInt(d.Val(), 10, 64)
				if err != nil {
					return d.Errf("invalid max_rows %q: %v", d.Val(), err)
				}
				if n < 0 {
					n = -1 // any negative value means unlimited
				}
				h.MaxRows = n
			case "max_conns":
				n, err := nextInt(d)
				if err != nil {
					return err
				}
				h.MaxConns = int(n)
			case "timeout":
				n, err := nextInt(d)
				if err != nil {
					return err
				}
				h.TimeoutSec = int(n)
			case "max_body_bytes":
				if !d.NextArg() {
					return d.ArgErr()
				}
				n, err := strconv.ParseInt(d.Val(), 10, 64)
				if err != nil {
					return d.Errf("invalid max_body_bytes: %v", err)
				}
				h.MaxBodyBytes = n
			default:
				return d.Errf("unrecognized subdirective %q", d.Val())
			}
		}
	}
	return nil
}

// nextInt consumes the next token as a non-negative integer.
func nextInt(d *caddyfile.Dispenser) (int64, error) {
	if !d.NextArg() {
		return 0, d.ArgErr()
	}
	n, err := strconv.ParseInt(d.Val(), 10, 64)
	if err != nil {
		return 0, d.Errf("invalid integer %q: %v", d.Val(), err)
	}
	if n < 0 {
		return 0, d.Errf("value must not be negative: %d", n)
	}
	return n, nil
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var handler Handler
	if err := handler.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return &handler, nil
}

// Interface guards.
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.Validator             = (*Handler)(nil)
	_ caddy.CleanerUpper          = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)
)
