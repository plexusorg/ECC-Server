# ECC-Server

The model host for Module-ECC's chat-safety classifier. It is one static Go binary. It serves a classifier bundle
written by the Module-ECC training pipeline (`uv run bundle`): it runs the bundle's GGUF in a `llama-server`
(llama.cpp) child process, keeps each conversation's prompt window, and returns each message's P(ban) to Module-ECC
over HTTP on the loopback interface. Module-ECC talks only to this host, never to llama-server. With a W&B API key,
the host fetches the bundle the pipeline promoted to `production` in Weights & Biases and switches to each new one.
With an Amazon Bedrock API key, it makes the slow check's call to Kimi on Bedrock, with the system prompt from the
served bundle, so neither the key nor the policy text is in Module-ECC.

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
sudo apt install ecc-server     # enables and starts the service
sudo ecc-server wandb login     # paste the W&B API key at the hidden prompt
sudo ecc-server bedrock login   # paste the Amazon Bedrock API key at the hidden prompt
ecc-server status               # the W&B check, the download of the production bundle, and Bedrock
```

The host starts without a bundle and reports not ready until it has one. With the key set, it downloads the
production bundle on its first check (the GGUF is about 3 GB) and serves it. Without W&B, install a bundle folder
copied onto the host instead: `sudo -u ecc-server ecc-server use /path/to/bundle` (the folder must be readable by
`ecc-server`).

Upgrade with `sudo apt update && sudo apt upgrade`. The service restarts, so conversation windows start anew; the
installed bundle, the W&B and Bedrock keys and your edited `config.toml` stay. Remove with `sudo apt remove
ecc-server` (stops and disables the service, keeps `/etc/ecc-server`, the keys and the data folder); `sudo apt purge
ecc-server` also deletes `/var/lib/ecc-server`, including the bundles and the downloaded llama.cpp, the stored W&B and
Bedrock keys and the `ecc-server` user.

On first start with a bundle, the host downloads the llama.cpp release the bundle names (its Linux CPU build), checks
the asset's SHA-256 against the bundle, and unpacks it under `<data>/llama/<release>/`.

## Updates from W&B

The pipeline uploads each bundle to W&B as a version of the `classifier-bundle` artifact (files `model.gguf`,
`bundle.json`, `manifest.json`). Promoting a bundle is moving the artifact's `production` alias to it in W&B; the
host picks it up within `check_interval` (10 minutes by default), or at once on `ecc-server update`.

**The key.** `sudo ecc-server wandb login` reads the API key from a hidden prompt (or from standard input when
piped), checks it with W&B, encrypts it with `systemd-creds encrypt` into
`/etc/credstore.encrypted/ecc-server.wandb-api-key`, and restarts the service if it is running. The key is
encrypted with the host's credential key, and also bound to the TPM2 chip when the machine has one (with no PCR
policy, so firmware and Secure Boot updates do not lock it), so it can be decrypted only on this machine, by root.
The service unit's `LoadCredentialEncrypted=ecc-server.wandb-api-key` has systemd decrypt it into the service's
private credential directory at each start; without a stored key the service starts and serves the bundle on disk.
`sudo ecc-server wandb logout` deletes the key and restarts the service. The key is never written in plaintext,
logged, or set in `config.toml`.

A stored key that no longer decrypts (the TPM2 was cleared or replaced, or the OS reinstalled with a new
`/var/lib/systemd/credential.secret`) keeps systemd from starting the service, and `systemctl status ecc-server`
shows `status=243/CREDENTIALS`. Run `sudo ecc-server wandb login` again, or `sudo ecc-server wandb logout`, then
`sudo systemctl restart ecc-server`.

**Each check** asks W&B which artifact version carries `production`. When its digest is not the one the served
bundle came from, the host:

1. downloads `manifest.json` and `bundle.json`, then the files the manifest lists, into
   `<data>/staging/<digest>/`, checking each file's size and MD5 against W&B. A file is written as `<name>.part`
   and renamed when it is complete and matches; an interrupted download resumes from its `.part` on the next check
   or after a restart. A bundle version already in the store is not downloaded again.
2. downloads the llama.cpp release the bundle names, if it is new; when that download fails, the staged files stay
   and the next check tries again;
3. installs the folder into the store, checking every file against the manifest's SHA-256s, as `use` does;
4. switches to it without a gap in scoring, as `use` does, and records which W&B digest it came from, so a restart
   does not download it again.

A bundle that fails any of these checks, or whose llama-server fails its health check, is discarded and logged with
the reason; the current bundle keeps serving, and the host does not fetch that production version again until
production moves, an operator runs `ecc-server update`, or the service restarts. A bundle version is installed once:
a changed bundle needs a new version.

**Offline.** When W&B is unreachable or answers with an error, the host keeps serving the bundle on disk and tries
again at the next check. It logs the first failed check of an outage and the first check that succeeds after it.
Before any bundle is installed, the host reports not ready.

**Rollback.** `ecc-server rollback` needs no W&B: the store keeps the previous bundle. When the bundle rolled back
from came from W&B, the host does not install that production version again until W&B's `production` moves to
another version, or until `ecc-server update`, which installs whatever production is. Moving `production` back to the
earlier version in W&B also switches the host back, at its next check. With a key set, a bundle installed with `use`
serves until the next check, which switches to production.

## Slow check on Amazon Bedrock

**The key.** `sudo ecc-server bedrock login` reads an Amazon Bedrock API key from a hidden prompt (or from standard
input when piped) and checks it with a small Converse call (16 output tokens) to `bedrock_model` in `bedrock_region`,
which proves both the key and its access to the model. A key Bedrock rejects, or any failed check, is not stored. A
key that passes is stored the way the W&B key is: encrypted with `systemd-creds` into
`/etc/credstore.encrypted/ecc-server.bedrock-api-key`, loaded by the unit's
`LoadCredentialEncrypted=ecc-server.bedrock-api-key`, and the service is restarted if it is running. Without a stored
key the service starts and scores, and slow checks fail with `no_key`. `sudo ecc-server bedrock logout` deletes the
key and restarts the service. A stored key that no longer decrypts stops the service from starting, as with the W&B
key: run `sudo ecc-server bedrock login` again, or `sudo ecc-server bedrock logout`. Bedrock API keys expire; when
Bedrock refuses the key, the host logs it once, with the fix, until a call succeeds again.

**The call.** The slow check sends one Converse request per check, `POST
https://bedrock-runtime.<bedrock_region>.amazonaws.com/model/<bedrock_model>/converse` with
`Authorization: Bearer <key>`:

```json
{"system": [{"text": "<the served bundle's slow_check.system>"}], "messages": [{"role": "user", "content": [{"text": "<user>"}]}], "inferenceConfig": {"maxTokens": 8000}}
```

Each call is one attempt with a 300-second timeout, and calls run alongside scoring without holding a llama-server
slot. When Bedrock answers 200, the answer carries the bundle version whose system prompt was sent, the model,
Bedrock's `stopReason`, the text of every output text block joined, and the token usage (input, output, cache read,
cache write; 0 when Bedrock omits one). A refusal or a token cap is still an answer; the slow check judges the stop
reason and the text. Otherwise the call fails with one of these reasons:

| reason | when |
| --- | --- |
| `no_bundle` | no bundle is served, so there is no system prompt |
| `no_key` | no Bedrock key is set |
| `bedrock_auth` | Bedrock answered 401 or 403: the key is invalid or expired, or has no access to the model |
| `bedrock_error` | any other answer than 200 (throttling, 5xx, validation), with Bedrock's status and message |
| `bedrock_unreachable` | a network error or the timeout |

The system prompt, the user text and the answer hold policy text and player chat, so the host never logs them, nor
the key: each call's log line has its result, sizes, token counts and duration. `ecc-server status` shows whether a
key is set, the region and model, and the last call's time and result (`ok`, or the reason with Bedrock's status and
error type).

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
| `wandb_entity` | `telesphoreo-telesphoreo` | W&B entity of the project |
| `wandb_project` | `ecc` | W&B project holding the bundle artifact |
| `wandb_artifact` | `classifier-bundle` | the artifact whose `production` alias names the bundle to serve |
| `check_interval` | `"10m"` | time between W&B checks, at least `1m` |
| `bedrock_region` | `us-east-1` | AWS region of the Bedrock endpoint the slow check calls |
| `bedrock_model` | `us.moonshotai.kimi-k3` | Bedrock model or inference profile ID the slow check calls |

The API keys are not config keys; see Updates from W&B and Slow check on Amazon Bedrock. On a machine without systemd
(development), the host reads them from the files `ecc-server.wandb-api-key` and `ecc-server.bedrock-api-key` in the
directory `$CREDENTIALS_DIRECTORY` names.

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
- `ecc-server update`: have the running host check W&B now, and wait while it downloads and switches to a new
  production bundle. It also installs a production version a rollback skipped or one that failed before.
- `ecc-server status`: print the bundle the running host serves and its health; with a W&B key the last check's
  time and result, the version W&B's `production` names, and any download in progress; and the Bedrock key, model
  and last call. Exit status 1 when no host is listening or the host is not ready.
- `ecc-server wandb login`, `ecc-server wandb logout`: store or delete the W&B API key (as root; see Updates from
  W&B).
- `ecc-server bedrock login`, `ecc-server bedrock logout`: store or delete the Amazon Bedrock API key (as root; see
  Slow check on Amazon Bedrock).
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

`GET /bundle` returns the served bundle's version and decision parameters, as the bundle states them. The bundle's
slow-check system prompt is not served: the host sends it to Bedrock itself. A bundle without a non-empty
`slow_check.system` does not load.

```json
{"version": "4b-282f3c43561f", "decision": {"window_rule": "single", "trigger": 0.9, "clearing": 0.5, "corroboration": 0.5}}
```

`GET /health` is `200 {"status": "ready", "version": ...}` when a bundle is loaded and its llama-server answers its
own health check, otherwise `503` with the reason. Its `wandb` field is `null` without a key, otherwise the W&B
state `status` prints: `artifact`, `last_check`, `result`, `production`, `production_digest`, and `download`
(`file`, `done` and `total` bytes) while one runs. Its `bedrock` field is `key_set`, `region`, `model`, and after the
first slow-check call `last_call` and `result`.

`POST /switch` with `{"version": "..."}` switches to an installed bundle; `use` and `rollback` call it.

`POST /update` runs a W&B check and answers when it is done: `200 {"result": "..."}`, `502` with the error, or `409`
without a key; `update` calls it.

## Parity check

`go run ./parity -ecc <Module-ECC checkout> -bundle <bundle folder> -attacks <attacks.json>` replays the
evaluation case sets, the attacks set and a sample of Total Freedom conversations through a running host, in
conversation order, and compares each message's margin with the margin `uv run eval` recorded
(`data/eval/classifier/*.jsonl`). The attacks set is built in the pipeline's code; `attacks.json` is
`json.dumps(ecc.attacks.conversations())`.
