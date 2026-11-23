# annex

[Annex](https://annex.eversonder.com) from your terminal: start a sandbox for an agent on any provider you have connected, run commands in it, move files in and out, and open a shell, without opening the dashboard.

```sh
annex login                       # approve once in your browser
annex start --route cheapest      # a sandbox on your cheapest connected account
annex run -- npm test             # run a command there, streaming its output
annex shell                       # a terminal in it
annex stop
```

## Install

```sh
curl -fsSL https://annex.eversonder.com/cli/install.sh | sh
```

The installer downloads the build for your computer (macOS or Linux, Intel or ARM) and checks it against a hash before installing. On Windows, use it inside WSL.

Or build it yourself from this source, which needs only Go:

```sh
go install github.com/eversonder/annex-cli/cmd/annex@latest
```

## Commands

| | |
|---|---|
| `annex login`, `logout`, `whoami` | Sign in by approving a code in your browser, or `login --key` with a key you have |
| `annex ls`, `start`, `info`, `stop`, `pause`, `resume` | Sandboxes |
| `annex run <id> -- <command>` | Run a command, streaming its output; exits with its exit code; Ctrl+C stops it |
| `annex shell <id>` | A terminal in the sandbox, with history and Tab completion; `cd` and exports carry over |
| `annex files`, `cp`, `logs` | Look around, copy files in or out, read the log |
| `annex template ls`, `apply`, `show`, `from`, `rm` | Templates: what new sandboxes start as |
| `annex providers` | The accounts the workspace has connected |
| `annex connect claude`, `connect cursor` | Give a coding agent Annex's tools, with this computer's key |

`<id>` may be left out when exactly one sandbox is running. `--json` prints the API's own answer. `annex help` lists everything; the full guide is at [annex.eversonder.com/docs/cli](https://annex.eversonder.com/docs/cli).

### Give your coding agent a computer

```sh
annex connect claude     # Claude Code, in every project
annex connect cursor     # Cursor; --project writes a file for the project that holds no key
```

Your agent then has tools to start a sandbox, run commands in it and read and write its files. Ask it: "Start a sandbox, clone this repo there and run the tests." Nothing is pasted anywhere: the command uses the key this computer is signed in with.

### annex.yml

A template is a small file saying what a sandbox starts with. In a folder that has one, `annex start` saves it and starts from it. Ready-made ones are at [annex-templates](https://github.com/eversonder/annex-templates).

```yaml
name: node-dev
vcpu: 2
memory_gib: 4
route: cheapest
setup:
  - npm install -g pnpm
```

## What it keeps, and where

- Signing in saves an API key in `~/.config/annex/config.json`, readable by you only. `annex logout` removes it from this computer; revoke it for good under API keys in the dashboard.
- `ANNEX_API_KEY`, `ANNEX_WORKSPACE` and `ANNEX_URL` win over that file, for scripts and CI.
- Shell history is kept in `~/.config/annex/history`.
- It talks to Annex's public HTTP API and nothing else, and sends no usage data.

## How it's built

- Go's standard library only: there are no dependencies to audit, and `go.mod` has none.
- [`cli/`](cli) is the whole command; [`cmd/annex`](cmd/annex) is a dozen lines that run it.
- Everything it does is a documented call to the [HTTP API](https://annex.eversonder.com/docs), so anything here can be done from your own code. For working projects, see [annex-examples](https://github.com/eversonder/annex-examples).

```sh
go test ./...
go build -o annex ./cmd/annex
```

This repository mirrors the command's source from Annex's own, where the builds the installer serves are made. Issues and pull requests are welcome here.

## Not yet

- No native Windows build (use WSL), no Homebrew formula, no self-update.
- `annex shell` runs each line as a command, so programs that draw the whole screen or prompt for input don't work in it.

## Licence

MIT. See [LICENSE](LICENSE).
