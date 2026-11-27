# wslc-compose

Multi-container orchestrator for Windows powered by `wslc`, bringing Compose workflow support to WSL containers.

## Requirements

- Windows or WSL with `wslc` (or `wslc.exe`) available in `PATH`
- Go `1.27+` (when building from source)

## Installation

```powershell
go build -o wslc-compose.exe .
```

## Quick Start

Create a `compose.yaml` file in your project directory:

```yaml
name: myapp

services:
  web:
    image: nginx:alpine
    ports:
      - "8080:80"
    depends_on:
      api:
        condition: service_healthy

  api:
    build: ./api
    environment:
      APP_ENV: production
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:3000/health"]
      interval: 5s
      timeout: 3s
      retries: 3
```

Start the stack:

```powershell
# Foreground mode (streams logs, stops on Ctrl+C)
wslc-compose up

# Detached mode
wslc-compose up -d

# Tear down containers and managed networks
wslc-compose down
```

## Features

- **Compose File Resolution & Multi-File Merging**: Automatically detects `compose.yaml`, `compose.yml`, `docker-compose.yaml`, or `docker-compose.yml`, or loads and deep-merges multiple files in order via repeatable `-f` / `--file` flags or `COMPOSE_FILE` (separated by `;` on Windows or `:` on Unix).
- **Safe AST Variable Interpolation**: Parses YAML first and interpolates scalar nodes in-memory without re-parsing YAML structure, supporting `.env` files, `--env-file`, and shell environment variables (`${VAR}`, `${VAR:-default}`, `${VAR-default}`, `${VAR:?err}`, `${VAR?err}`, `${VAR:+alt}`, `${VAR+alt}`, and `$$` escaping). Comments are never evaluated, quoted `#` values in `.env` are preserved, and explicit YAML `null` (`KEY:`) passthrough is kept distinct from empty string values (`KEY: ""` or `KEY: "${UNSET}"`).
- **Compose Spec Syntax Support**: Supports both map and list forms for `environment`, `labels`, `build.args`, `extra_hosts`, `networks`, and `depends_on`, plus short and long object syntax for `ports` and `volumes`.
- **Dependency Ordering**: Topological floor execution with `depends_on` conditions (`service_started`, `service_healthy`, `service_completed_successfully`), automatic rollback on startup failure, and graceful reverse-order shutdown.
- **Profiles**: Selective service activation via repeatable `--profile` flags or comma-separated `COMPOSE_PROFILES`.
- **Resource Lifecycle**: Automatically creates and attaches `<project>_default` when services omit `networks:`, manages custom networks, named volumes, bind mounts, image builds, pulls, and safe project-scoped orphan cleanup.
- **Windows & WSL Path Handling**: Relative paths in `volumes` and `env_file` resolve against the first Compose file's base directory. Windows drive paths (e.g., `C:\data\app`) and UNC paths are preserved, while POSIX/WSL absolute paths starting with `/` (e.g., `/var/run/docker.sock`, `/mnt/c/...`) are preserved cleanly without prepending the Windows base directory or running Windows host `os.Stat` checks. In `cp`, single-letter Compose service names such as `a:/etc/hosts` or `a:file.txt` are distinguished from Windows drive paths (`C:\temp\file.txt`).

## CLI Reference

### Global Flags

Global flags can be placed before any subcommand (including `-f` before `rm` or `logs`):

```powershell
wslc-compose -f compose.yaml -f compose.override.yaml up -d
wslc-compose -f staging.yaml rm -f web
wslc-compose -f staging.yaml logs -f api
```

| Flag | Env Var | Description |
| :--- | :--- | :--- |
| `-f, --file <path>` | `COMPOSE_FILE` | Compose configuration file path (repeatable; `COMPOSE_FILE` splits on `;` on Windows or `:` on Unix) |
| `-p, --project-name <name>` | `COMPOSE_PROJECT_NAME` | Override project name |
| `--env-file <path>` | — | Load variables from an alternate `.env` file |
| `--profile <name>` | `COMPOSE_PROFILES` | Enable one or more service profiles (repeatable; `COMPOSE_PROFILES` accepts comma-separated values) |

### Commands

| Command | Common Flags | Purpose |
| :--- | :--- | :--- |
| `up [service...]` | `-d`, `--build`, `--no-build`, `--force-recreate`, `--no-recreate`, `--no-deps`, `--remove-orphans`, `--wait` | Create and start project resources and containers |
| `down` | `-v`, `--remove-orphans`, `-t <sec>`, `--rmi <local\|all>` | Gracefully stop and remove containers, networks, volumes, and images |
| `ps` | `-a`, `-q`, `--services`, `--status <state>`, `--format <table\|json>` | List project containers and states |
| `logs [service...]` | `-f`, `--tail <n\|all>`, `-t` | View or stream container logs |
| `exec <service> <cmd...>` | `-T`, `-u <user>`, `-w <dir>`, `-e <KEY=VAL>` | Run a command inside a running service container |
| `run <service> [cmd...]` | `--rm`, `-d`, `--no-deps`, `--entrypoint`, `-u`, `-w`, `-e` | Launch a one-off container for a service |
| `start [service...]` | — | Start existing stopped service containers in dependency order |
| `stop [service...]` | `-t <sec>` | Gracefully stop running service containers |
| `restart [service...]` | `-t <sec>` | Restart service containers in dependency order |
| `build [service...]` | `--no-cache`, `--pull`, `--build-arg <KEY=VAL>` | Build or rebuild service images |
| `pull [service...]` | `-q`, `--ignore-pull-failures` | Pull images defined by services |
| `config` | `-q`, `--services`, `--volumes` | Validate and render the resolved Compose configuration |
| `cp <src> <dest>` | `-a` (`--archive`), `-L` (`--follow-link`) | Copy files or folders between a service container (`SERVICE:PATH`) and the local filesystem |
| `events [service...]` | `--json` | Receive real-time events for project containers |
| `images [service...]` | `-q` (`--quiet`) | List images used by project services |
| `kill [service...]` | `-s` (`--signal <SIG>`) | Force stop service containers by sending a signal (default: `SIGKILL`) |
| `port <service> <private_port>` | `--protocol <tcp\|udp>` | Print the public host binding for a container port (alias: `ports`; matches `tcp` or `udp` automatically when `--protocol` is omitted) |
| `rm [service...]` | `-f` (`--force`), `-s` (`--stop`), `-v` (`--volumes`) | Remove stopped service containers (optionally stopping running ones first) |
| `stats [service...]` | `--no-stream` | Display a live stream or single snapshot of container resource usage statistics |
| `volumes` | — | List volumes declared in the Compose file and their existence status |
| `completion <shell>` | — | Generate shell completion scripts (`bash`, `zsh`, `fish`, `powershell`) |

## License

Released under the [MIT License](./LICENSE).
