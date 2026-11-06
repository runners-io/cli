# runners

The command line for Scratch machines. A Scratch machine is a CI-class machine
that you sync your working directory to, run commands on, and stop.

**Status: early. Scratch machines are not on sale yet, and this tool does not yet
connect to a service.** Every command that has to reach the service stops with
exit code 125 ("not available yet"). There is no release to download and no
Homebrew formula yet. What exists is the command set, its exit codes, its
JSON event stream and the sync manifest, tested against an in-memory fake.

## Commands

The binary is `runners` and its one verb is `scratch`:

```
runners scratch login  [--json]
runners scratch logout [--json]
runners scratch warmup [--size 2|4|8] [--image ubuntu-2404]
                       [--idle-timeout 10m] [--max-duration 4h]
                       [--setup SCRIPT [--env NAME]...] [--json]
runners scratch sync   --id ID [--dir DIR] [--allow PATH]... [--json]
runners scratch run    --id ID [--dir DIR] [--no-sync] [--detach] [--allow PATH]...
                       [--env NAME]... [--json] -- <command> [args]
runners scratch ssh    --id ID [-- <args>]
runners scratch pull   --id ID [--dir DIR] [--json] <path>...
runners scratch list   [--json]
runners scratch extend --id ID [--by 10m] [--json]
runners scratch stop   --id ID [--json]
runners scratch status --id ID [--json]
```

`runners help` and `runners version` are the only commands outside `scratch`.

## What it does with your files and credentials

- In a git checkout, `run` and `sync` send the files `git ls-files -co
  --exclude-standard` lists. Files with secret-shaped names (`.env*`, `*.pem`,
  `*.key`, `.ssh/`, `.aws/`, and similar) are skipped by name, never read, and
  counted in the output. `--allow PATH` sends one of them on purpose. This is a
  guard against accidents, not a scanner: a secret inside an ordinary file is
  not found.
- `--env NAME` forwards one variable from your environment by name. A value is
  never printed.
- A credential is never a command-line argument and is never printed.
- `--json` writes one JSON event per line on stdout and nothing else.

## Exit codes

| code | meaning |
|---|---|
| 0 | success |
| 120 | bad command line, or an internal error in this tool |
| 121 | the workspace is too large to sync |
| 122 | quota, cap or billing refused the request |
| 123 | not signed in, or the credential was refused |
| 124 | the machine is gone: stopped, expired or never existed |
| 125 | the machine, or the service, is not ready |
| 130 | interrupted |
| other | `run`: the remote command's own exit code, passed through |

The range 120-125 may still change before the first release.

## Building

This repository is a generated copy of the tool's source tree, kept in step by
the maintainers. It imports only the Go standard library.

```
go build -o runners .
go test ./...
```

## Licence

Copyright 2026 GOO HOLDINGS LTD. Licensed under the Apache License, Version 2.0;
see `LICENSE`.
