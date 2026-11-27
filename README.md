# wslc-compose

Multi-container orchestrator for Windows powered by `wslc.exe`, bringing Compose workflow support to WSL containers.

## Requirements

- Windows with `wslc.exe` available in `PATH`
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

- **Compose File Resolution**: Automatically detects `compose.yaml`, `compose.yml`, `docker-compose.yaml`, or `docker-compose.yml`.
- **Variable Interpolation**: Supports `.env` files, `--env-file`, and shell environment substitution.
- **Dependency Ordering**: Topological floor execution with `depends_on` conditions (`service_started`, `service_healthy`, `service_completed_successfully`) and automatic rollback on startup failure.
- **Profiles**: Selective service activation via `--profile` or `COMPOSE_PROFILES`.
- **Resource Lifecycle**: Manages project-scoped networks, named volumes, bind mounts, image builds, pulls, and orphan cleanup.

## CLI Reference

### Global Flags

| Flag | Env Var | Description |
| :--- | :--- | :--- |
| `-f, --file <path>` | `COMPOSE_FILE` | Explicit path to the Compose configuration file |
| `-p, --project-name <name>` | `COMPOSE_PROJECT_NAME` | Override project name |
| `--env-file <path>` | — | Load variables from an alternate `.env` file |
| `--profile <name>` | `COMPOSE_PROFILES` | Enable one or more service profiles |

### Commands

| Command | Common Flags | Purpose |
| :--- | :--- | :--- |
| `up [service...]` | `-d`, `--build`, `--no-build`, `--force-recreate`, `--no-recreate`, `--no-deps`, `--remove-orphans`, `--wait` | Create and start project resources and containers |
| `down` | `-v`, `--remove-orphans`, `-t <sec>`, `--rmi <local\|all>` | Stop and remove containers, networks, volumes, and images |
| `ps` | `-a`, `-q`, `--services`, `--status <state>`, `--format <table\|json>` | List project containers and states |
| `logs [service...]` | `-f`, `--tail <n\|all>`, `-t` | View or stream container logs |
| `exec <service> <cmd...>` | `-T`, `-u <user>`, `-w <dir>`, `-e <KEY=VAL>` | Run a command inside a running service container |
| `run <service> [cmd...]` | `--rm`, `-d`, `--no-deps`, `--entrypoint`, `-u`, `-w`, `-e` | Launch a one-off container for a service |
| `start [service...]` | — | Start existing stopped service containers |
| `stop [service...]` | `-t <sec>` | Gracefully stop running service containers |
| `restart [service...]` | `-t <sec>` | Restart service containers in dependency order |
| `build [service...]` | `--no-cache`, `--pull`, `--build-arg <KEY=VAL>` | Build or rebuild service images |
| `pull [service...]` | `-q`, `--ignore-pull-failures` | Pull images defined by services |
| `config` | `-q`, `--services`, `--volumes` | Validate and render the resolved Compose configuration |

## License

Released under the [MIT License](./LICENSE).
