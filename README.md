# ECC-Server

The model host for Module-ECC's chat-safety classifier. It is one static Go binary. It serves a classifier bundle
written by the Module-ECC training pipeline (`uv run bundle`): it runs the bundle's GGUF in a `llama-server`
(llama.cpp) child process, keeps each conversation's prompt window, and returns each message's P(ban) to Module-ECC
over HTTP on the loopback interface. Module-ECC talks only to this host, never to llama-server.

Each message is scored as the last line of its conversation's window. The prompt is built byte for byte as the
pipeline builds it (Module-ECC `src/ecc/chat.py`), with token counts from llama-server's
`/tokenize`. Because a window grows one line at a time, llama-server reuses the cached prefix and decodes only the new
line.

## Install (Debian, Ubuntu)

ECC-Server is packaged as an amd64 `.deb` that depends on `libgomp1` (the llama.cpp Linux build links OpenMP's
runtime) and `ca-certificates` (the llama.cpp release download). The package installs:

- `/usr/bin/ecc-server`, the static binary
- `/lib/systemd/system/ecc-server.service`, which runs `ecc-server run` as the `ecc-server` user with
  `Restart=always`; if llama-server exits on its own, the host exits too and systemd restarts both
- `/etc/ecc-server/config.toml`, a config file dpkg keeps across upgrades (see Configuration)
- the `ecc-server` system user and `/var/lib/ecc-server`, the data folder it owns

Add the apt source from the repository's "Use this repository" tab in the Telesphorepo web UI (it gives the apt
line and the signing-key setup). The repository address is `https://telesphorepo.com/debian/ecc-server`;
pre-releases (versions containing `~`) are in `ecc-server-dev`. Then:

```sh
sudo apt update
sudo apt install ecc-server                        # enables and starts the service
sudo -u ecc-server ecc-server use /path/to/bundle  # the folder must be readable by ecc-server
ecc-server status
```

The host starts without a bundle and reports not ready until `use` installs one.

Upgrade with `sudo apt update && sudo apt upgrade`. The service restarts, so conversation windows start anew; the
installed bundle and your edited `config.toml` stay. Remove with `sudo apt remove ecc-server` (stops and disables
the service, keeps `/etc/ecc-server` and the data folder); `sudo apt purge ecc-server` also deletes
`/var/lib/ecc-server`, including the bundles and the downloaded llama.cpp, and the `ecc-server` user.

On first start with a bundle, the host downloads the llama.cpp release the bundle names (its Linux CPU build), checks
the asset's SHA-256 against the bundle, and unpacks it under `<data>/llama/<release>/`.

## Building the package

`./package.sh [version]` (needs Go; nothing else is installed) builds the static binary and runs nfpm, pinned to a
version, into `dist/ecc-server_<version>_amd64.deb`, then prints the `telesphorepo publish` command for it. The version
is the argument, or else the `VERSION` file; `ecc-server version` prints it. Write a Debian version: `0.1.0` is a
release, `0.1.0~dev.3` a pre-release that sorts before it. `telesphorepo.toml` sends versions containing `~` to
`ecc-server-dev` and the rest to `ecc-server`.

GitHub Actions (`.github/workflows/publish.yml`) builds the package on every push and pull request. Each push to
master publishes `<VERSION>~dev.<run number>` to `ecc-server-dev`. To publish a release, run the workflow on master
from the Actions tab with "release" checked: it publishes `VERSION` as it is to `ecc-server`. Bump `VERSION` after a
release. Telesphorepo's CI pipeline trusts this workflow file on master, so no secret is stored.

## Configuration

A TOML file, `/etc/ecc-server/config.toml` unless `-config` names another. Unknown keys are rejected.

| key | default | meaning |
| --- | --- | --- |
| `listen` | `127.0.0.1:8790` | HTTP address; must be a loopback address |
| `data` | `/var/lib/ecc-server` | bundle store, state and the downloaded llama.cpp release |
| `threads` | `6` | llama-server's `-t` |
| `llama_server` | (none) | run this llama-server binary instead of downloading the bundle's release; required on Windows |

## Commands

Every command except `version` takes `-config FILE` before its arguments (default `/etc/ecc-server/config.toml`).

- `ecc-server run`: serve the current bundle. With none installed it reports not ready until `use` installs one.
- `ecc-server use FOLDER`: install a bundle folder (`model.gguf`, `bundle.json`, `manifest.json`) into the store and
  switch to it. Every file is checked against the manifest's SHA-256 and size; files are hardlinked when the folder
  is on the same volume, otherwise copied. With the host running, the switch has no gap in scoring: the host starts a
  second llama-server on the new bundle, health-checks it, switches scoring and the decision parameters together, and
  stops the old one once its in-flight messages finish. If the new bundle fails to start, the current one keeps
  serving and the new one is discarded. Without a running host, the bundle is served from the next start.
- `ecc-server rollback`: switch back to the previous bundle, the same way. The store keeps the current and the
  previous bundle and deletes older ones.
- `ecc-server status`: print the bundle the running host serves and its health, from `/health`; exit status 1 when
  no host is listening or the host is not ready.
- `ecc-server version`: print the package version.

`use` and `rollback` refuse to run as root: run them as the service user (`sudo -u ecc-server ecc-server use ...`),
which owns the data folder the running host also writes.

## HTTP interface

All bodies are JSON.

`POST /score` with `{"conversation": "...", "speaker": "...", "text": "..."}` appends the message to its
conversation's window and scores it as the window's last line:

```json
{"p_ban": 0.013, "margin": -2.98, "version": "4b-282f3c43561f", "earlier": 17, "tokens": 233, "evaluated": 12}
```

- `p_ban` is `sigmoid(a * margin + b)` with the bundle's Platt parameters; `margin` is
  `log P(" ban") - log P(" ok")` at the answer position.
- `version` is the bundle that produced the score. When it changes, read `/bundle` again.
- `earlier` is the number of earlier lines in the prompt, `tokens` the prompt's length, `evaluated` how many of them
  llama-server decoded (the rest came from its cache).
- A conversation's messages are scored one at a time, in the order they arrive; send a conversation's next message
  after the previous answer. Different conversations are scored in parallel up to llama-server's slot count.
- A conversation idle for 30 minutes drops its window; its next message starts a new one. A bundle switch also
  starts new windows.
- `503`: no bundle is served yet; retry later. `502`: llama-server failed; the message was not added to the window,
  so retry it. `422`: neither answer token is among the top logprobs; the message was added to the window but has
  no score, so do not retry it. `400`: malformed request.

`GET /bundle` returns the served bundle's version, decision parameters and slow-check prompt, as the bundle states them.
A bundle without a non-empty `slow_check.system` does not load.

```json
{"version": "4b-282f3c43561f", "decision": {"window_rule": "single", "trigger": 0.9, "clearing": 0.5, "corroboration": 0.5}, "slow_check": {"system": "..."}}
```

`GET /health` is `200 {"status": "ready", "version": ...}` when a bundle is loaded and its llama-server answers its
own health check, otherwise `503` with the reason.

`POST /switch` with `{"version": "..."}` switches to an installed bundle; `use` and `rollback` call it.

## Parity check

`go run ./parity -ecc <Module-ECC checkout> -bundle <bundle folder> -attacks <attacks.json>` replays the
evaluation case sets, the attacks set and a sample of Total Freedom conversations through a running host, in
conversation order, and compares each message's margin with the margin `uv run eval` recorded
(`data/eval/classifier/*.jsonl`). The attacks set is built in the pipeline's code; `attacks.json` is
`json.dumps(ecc.attacks.conversations())`.
