// Command pocketcaddy is a Caddy web server with the pocketcaddy handler
// built in. It accepts every command and Caddyfile directive Caddy does.
package main

import (
	caddycmd "github.com/caddyserver/caddy/v2/cmd"

	// The standard Caddy modules: the HTTP server, TLS automation, the
	// file server, and the rest of the Caddyfile vocabulary.
	_ "github.com/caddyserver/caddy/v2/modules/standard"

	// The pocketcaddy handler itself, which registers on import.
	_ "github.com/tychoengineering/pocketcaddy"
)

func main() {
	caddycmd.Main()
}
