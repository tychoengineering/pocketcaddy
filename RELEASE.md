# Releasing

A release publishes two archives, a checksum file, and a Homebrew cask. Pushing
a `v*` tag is the whole procedure; everything else here explains what that tag
sets in motion and how to check the result.

## Cutting a release

Start from a clean `main` that passes the tests, because nothing runs on push —
only a tag triggers the workflow.

```sh
go test ./...
git switch main && git pull

git tag v0.2.0
git push origin v0.2.0
```

Watch the run, which takes about eight minutes:

```sh
gh run watch --exit-status
```

Then confirm the release carries both archives and the checksums:

```sh
gh release view v0.2.0
```

Versions follow [semantic versioning](https://semver.org) with a leading `v`.
The tag is the only place the version lives: GoReleaser derives the archive
names and the `CustomVersion` stamp from it, so `pocketcaddy version` reports
the tag the binary was built from.

## What the tag triggers

`.github/workflows/release.yml` runs `go test ./...` and then GoReleaser. A
failing test stops the release before it publishes anything, which is the only
gate on a tag — no workflow runs on pushes to `main` or on pull requests.

GoReleaser reads `.goreleaser.yaml` and, for macOS arm64 and Linux amd64:

1. Builds `./cmd/pocketcaddy` with `CGO_ENABLED=0`, `-trimpath`, and `-s -w`.
2. Archives the binary with `README.md` and `LICENSE`.
3. Writes `checksums.txt` over both archives.
4. Creates the GitHub release.
5. Pushes `Casks/pocketcaddy.rb` to `tychoengineering/homebrew-tap`.

The build stamps the tag into `github.com/caddyserver/caddy/v2.CustomVersion`.
That value must hold no spaces: the Go linker splits `-X` on whitespace, so a
version string like `pocketcaddy 0.2.0 (caddy 2.11.4)` makes the linker treat
`0.2.0` as a flag and dump its usage text.

## Targets

Releases cover macOS arm64 and Linux amd64. `.goreleaser.yaml` ignores the
other two combinations its `goos`/`goarch` lists imply.

Adding a target means deleting its `ignore` entry, and — for macOS amd64 —
removing the matching case from `install.sh`, which names that platform
explicitly so an Intel Mac gets a useful message rather than a 404.

Cross-compiling needs no C toolchain. `modernc.org/sqlite` is pure Go, which is
what keeps `CGO_ENABLED=0` viable; a dependency that needs cgo would break both
the cross-compile and `xcaddy build`.

## Prerequisites

Two things live outside this repository.

The tap repository `tychoengineering/homebrew-tap` must exist and be public,
with a non-empty default branch. GoReleaser commits the cask into it; it does
not create it.

The `HOMEBREW_TAP_TOKEN` secret on this repository must hold a token with
`Contents: Read and write` on the tap. The workflow's built-in `GITHUB_TOKEN`
cannot write to another repository, so the Homebrew step fails without it. A
fine-grained token scoped to the tap alone is enough. Fine-grained tokens owned
by an organization can sit in a pending state until an owner approves them
under **Settings → Third-party Access → Personal access tokens**.

Rotate the token before it expires. Nothing warns you first: the release
succeeds and only the Homebrew step fails.

## Testing without publishing

A snapshot build runs the whole pipeline against the working tree and writes to
`dist/` without touching GitHub. Run it after changing `.goreleaser.yaml`.

```sh
goreleaser check
HOMEBREW_TAP_TOKEN=dummy goreleaser release --snapshot --clean --skip=publish
```

Then exercise the binary the archive actually ships:

```sh
tar -xzf dist/pocketcaddy_*_darwin_arm64.tar.gz -C /tmp
/tmp/pocketcaddy version
/tmp/pocketcaddy list-modules | grep pocketcaddy
```

`list-modules` is the check worth keeping. The handler registers through a
blank import in `cmd/pocketcaddy/main.go`, so dropping that import still builds
and still runs — it just yields a Caddy that does not recognize the
`pocketcaddy` directive.

## Repository visibility

`install.sh` and Homebrew both download release assets anonymously, so this
repository has to stay public for either to work. Making it private breaks both
install paths with a 404 while the release itself still looks healthy through
`gh`, which authenticates.

GitHub answers a HEAD request against `/releases/latest` with a 404 whether or
not the repository is public, so the script asks the releases API with a GET
instead. A 404 from that API means the release is genuinely unreachable, and
visibility is the first thing to check.

Verify with an anonymous request rather than `gh`, which authenticates and so
succeeds either way:

```sh
curl -fsSL https://api.github.com/repos/tychoengineering/pocketcaddy/releases/latest |
	grep tag_name
```

## Fixing a bad release

Delete the release and the tag, then tag again once the fix lands.

```sh
gh release delete v0.2.0 --cleanup-tag
git push origin :refs/tags/v0.2.0
git tag -d v0.2.0
```

Anyone who already installed keeps their copy, and the cask in the tap still
points at the deleted assets until the next release overwrites it. Prefer
publishing `v0.2.1` once a version has been public for any length of time.

## Signing

The macOS binary is unsigned. Both install paths clear the quarantine
attribute — the cask through a post-install hook, the script by never setting
it — so Gatekeeper stops only an archive downloaded through a browser.

Signing and notarizing would remove that caveat. It needs an Apple Developer
account, a Developer ID certificate in the workflow, and a `notarize` step in
`.goreleaser.yaml`.
