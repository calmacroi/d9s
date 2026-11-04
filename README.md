# d9s — k9s for your local Docker / Compose

`d9s` is a keyboard-driven terminal UI for the local Docker daemon, modelled on
[k9s](https://k9scli.io). It is built for the SymbioCommerce Docker Compose stack:
containers are grouped by compose project, so you can scan the stack at a glance
and act on single services.

It is a single Go binary (tview + the Docker Engine SDK). It reads the daemon
of your current `docker context` (or `DOCKER_HOST`), and calls the `docker` CLI
for `exec` and `compose`.

> Already-existing alternatives: [lazydocker](https://github.com/jesseduffield/lazydocker)
> (installed via Homebrew on this machine) and [ctop](https://github.com/bcicen/ctop).
> d9s is for those who want k9s muscle memory (`:`, `/`, drill-down, `esc` back).

## Install

Requires Go ≥ 1.25 and the Docker CLI with the compose plugin.

```bash
cd d9s
make install        # go install → $(go env GOPATH)/bin/d9s
# or: make build && ./d9s
```

## Usage

```bash
d9s                              # containers of the current docker context
d9s --view compose               # start in the compose projects view
d9s --view "svc symbiocommerce"  # start in the services of a project
d9s --context desktop-linux --refresh 5s
```

## Views (`:` command mode)

| Command | Aliases | Shows |
|---|---|---|
| `:containers [project]` | `:co` `:ps` `:c` | all containers grouped by compose project: state, health, CPU%, memory, ports, uptime, age |
| `:compose` | `:projects` `:cp` `:p` | compose projects with running/total, aggregated CPU and memory, config files |
| `:svc <project>` | `:services` | the project's services, including declared but not created ones |
| `:images` | `:img` `:i` | images (one row per tag), size, containers using them |
| `:volumes` | `:vol` `:v` | volumes grouped by compose project, number of containers using them |
| `:networks` | `:net` `:n` | networks with attached container count |
| `:q` | `:quit` | quit |

Command names are suggested while typing. The header shows docker context, host, engine version and resource counts.

Drill-down (`enter`): project → services → containers; image / volume / network → containers using it.
`esc` clears the filter first, then goes back.

## Key bindings

Navigation: `j`/`k` or arrows, `g`/`G` top/bottom, `ctrl-f`/`ctrl-b` page, `esc` back, `q` back (quits at top level), `ctrl-c` quit.

| Key | Anywhere in a table |
|---|---|
| `/` | live filter over all columns (`!text` inverts) |
| `<` `>` | previous / next sort column (arrow in header) |
| `=` | reverse sort direction |
| `ctrl-g` | toggle grouping by compose project |
| `ctrl-r` | refresh now |
| `?` | help overlay for the current view |

| Key | Containers | Compose projects | Services | Images / Volumes / Networks |
|---|---|---|---|---|
| `enter` | logs | services | containers | containers using it |
| `l` | logs | `compose logs` | `compose logs <svc>` | |
| `d` | describe (YAML/JSON) | | describe container | describe |
| `s` | shell (`bash`, falls back to `sh`) | | shell | |
| `p` | ports: mappings + open in browser | | | |
| `S` | start | `compose up -d` | `compose up -d <svc>` | |
| `T` | stop | `compose stop` | `compose stop <svc>` | |
| `R` | restart | `compose restart` | `compose restart <svc>` | |
| `c` | | containers of project | | |
| `ctrl-d` | delete (confirm) | `compose down` (confirm, keeps volumes) | `compose rm -sf <svc>` (confirm) | delete (confirm) |

Logs view: `f` follow on/off, `t` timestamps, `w` wrap, `/` filter lines (matches highlighted),
`c` clear, `0` full history instead of the last 500 lines, `esc` back.
Describe view: `y` toggles YAML / JSON.

Compose commands reuse the project's original compose files, working directory and env files
(from the container labels), so they work no matter where d9s was started. If those files are
gone, `S` falls back to `compose start` and only label-based commands work.

## Development

```bash
make vet     # gofmt + go vet
make e2e     # end-to-end UI test against the local daemon
```

`make e2e` starts a throwaway compose project (`e2e/compose.yml`, project `d9s-e2e`) and
`d9s-e2e-*` resources, then drives the real UI on a simulated screen: views, filter, sort,
grouping, logs, describe, ports, start/stop/restart, compose up/stop/restart/rm/down and
deletes. It only touches `d9s-e2e*` resources and ends with `compose down` of that project.
Set `D9S_E2E_OUT=<dir>` to keep screen dumps of every step.
