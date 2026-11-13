# Contributing to Kiri

Thanks for helping. Issues and pull requests are welcome.

## Before you start

For anything larger than a small fix, open an issue first so we can agree on
the approach. Kiri aims to be small and fast: changes that add a dependency, a
background process or startup time need a good reason.

## Building and testing

```sh
make check            # vet and unit tests for engine and desktop
make app-dev          # target/dev/Kiri Dev.app, beside any installed Kiri
tests/e2e.sh          # the Docker compatibility suite, against a running engine
```

The guest needs `make guest`, `make kernel` and `make disk` once; see
[docs/development.md](docs/development.md). Run the suite against the Dev
app's context (`DOCKER_CONTEXT=kiri-dev`), not the Kiri you use every day.

`tests/e2e.sh` only removes containers, networks and volumes it created.

## Every feature ships on macOS, Windows and Linux together

A feature comes in one pull request, for all three systems. Words may differ
by system ("Show in Finder"); what the window offers may not. Windows has no
host in this tree yet, so until it does this covers macOS and Linux.

## Pull requests

- Keep each pull request to one change, with a message that says what it does
  and why.
- Run `gofmt` and `make check`. After changing dependencies or bundled tool
  versions, update NOTICE.
- Sign your commits and the [Contributor License Agreement](CLA.md) when CLA
  Assistant asks for it on your first pull request.

By submitting a contribution you agree it is licensed under Apache-2.0, as
described in the README and the Contributor License Agreement.
