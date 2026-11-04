# ECC-Server

The chat-safety decision maker for Module-ECC. It is one static Go binary beside the game server. Module-ECC
forwards every message players send and carries out the actions this host gives it; every decision is made here:
the flood and duplicate rule, the per-player queues, the fast check (a classifier bundle written by the Module-ECC
training pipeline's `uv run bundle`, run in a `llama-server` (llama.cpp) child process), the trigger, the pending
state, the slow check (Kimi on Amazon Bedrock, verified by code), who is muted, unmuted and banned and when a banned
player is kicked, and the daily record. Thresholds and prompts come only from the bundle. With a W&B API key, the
host fetches the bundle the pipeline promoted to `production` in Weights & Biases and switches to each new one; the
Bedrock key and the policy text in the bundle never reach Module-ECC.

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
sudo ecc-server status          # the W&B check, the bundle download, Bedrock and chat safety
sudo ecc-server token           # the token to paste into Module-ECC's config (see Connecting Module-ECC)
```

The host starts without a bundle and reports not ready until it has one. With the key set, it downloads the
production bundle on its first check (the GGUF is about 3 GB) and serves it. Without W&B, install a bundle folder
copied onto the host instead: `sudo -u ecc-server ecc-server use /path/to/bundle` (the folder must be readable by
`ecc-server`).

Upgrade with `sudo apt update && sudo apt upgrade`. The service restarts, so conversation windows start anew; the
installed bundle, the token, the W&B and Bedrock keys, your edited `config.toml`, the pending players, the unacknowledged
actions and the records stay. Remove with `sudo apt remove ecc-server` (stops and disables the service, keeps
`/etc/ecc-server`, the keys and the data folder); `sudo apt purge ecc-server` also deletes `/var/lib/ecc-server`,
including the bundles, the downloaded llama.cpp, the chat-safety state, the records and the token, the stored W&B and
Bedrock keys, and the `ecc-server` user.

On first start with a bundle, the host downloads the llama.cpp release the bundle names (its Linux CPU build), checks
the asset's SHA-256 against the bundle, and unpacks it under `<data>/llama/<release>/`.

## Updates from W&B

The pipeline uploads each bundle to W&B as a version of the `classifier-bundle` artifact (files `model.gguf`,
`bundle.json`, `manifest.json`). Promoting a bundle is moving the artifact's `production` alias to it in W&B; the
host picks it up within `check_interval` (10 minutes by default), or at once on `sudo ecc-server update`.

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
production moves, an operator runs `sudo ecc-server update`, or the service restarts. A bundle version is installed once:
a changed bundle needs a new version.

**Offline.** When W&B is unreachable or answers with an error, the host keeps serving the bundle on disk and tries
again at the next check. It logs the first failed check of an outage and the first check that succeeds after it.
Before any bundle is installed, the host reports not ready.

**Rollback.** `ecc-server rollback` needs no W&B: the store keeps the previous bundle. When the bundle rolled back
from came from W&B, the host does not install that production version again until W&B's `production` moves to
another version, or until `sudo ecc-server update`, which installs whatever production is. Moving `production` back to the
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

**The call.** Each slow-check call is one Converse request, `POST
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
| `bedrock_error` | any other answer than 200 (throttling, 5xx, validation), or a 200 that is not a Converse response, with Bedrock's status and message |
| `bedrock_unreachable` | a network error or the timeout |

The system prompt, the user text and the answer hold policy text and player chat, so the host never logs them, nor
the key: a successful call's log line has its stop reason, sizes, token counts and duration, a failed call's has its
result, sizes and duration, and a refused key is logged once per outage. `sudo ecc-server status` shows whether a key is
set, the region and model, and the last call's time and result (`ok`, or the reason with Bedrock's status and error
type).

## Chat safety

**The path of a message.** A message from Module-ECC (`POST /messages`) is one of three kinds:

- `shadowed`: only its author saw it (a muted player, or NUSH quarantine). It is recorded and never scored, and enters
  no window and no shared transcript. A pending player's shadowed lines are kept as later evidence for their slow
  check, which runs again on each one: later lines can add evidence, never clear.
- `bypass` (staff): it enters its conversation's window, with its token cost, and the transcripts, and is recorded
  unscored. Staff are never judged.
- every other message goes through the duplicate rule. A player's exact repeat within 60 seconds of their previous
  copy reuses that copy's verdict; when three or more players send the same normalized text (case, colour codes,
  lookalike letters, repeated letters and random-looking tokens ignored) within 60 seconds of its first copy, later
  copies reuse the first copy's verdict. A reused copy still enters its conversation's window, unscored. Every other
  message waits in its player's queue (`queue_size`, oldest dropped from scoring when full; a dropped line still enters
  the window) and is scored. Players are served in turn, and each conversation's lines reach its window in the order
  they were said. While no bundle is served or llama-server fails, scoring pauses and the queues wait; the log warns
  once when scoring pauses and once when it resumes.

So a conversation's window holds every line other players saw. A message whose P(ban) is at or above the bundle's
`trigger` threshold triggers its speaker.

**Trigger.** The player is muted (a `mute` action) and becomes pending. The pending state holds the conversation up to
and including the trigger, the player's recent messages, and their lines said after it; it is written to
`<data>/safety/pending/<uuid>.json` (written to a temporary file, fsynced, renamed) on every change.

**Slow check.** Each pending player's check asks Kimi, through the Bedrock call below, for evidence on the whole
transcript and on the transcript cut at each judged player's trigger, and code decides every participant (the port of
the pipeline's `src/ecc/redteam.py`, matched against its cached red-team answers with no mismatch). Kimi's call is
retried as the pipeline retries it (three retries, 2, 4 and 8 seconds apart; an API failure, a refusal, the token cap
and an unparseable answer are retried); a call that still fails is retried with backoff from 30 seconds to 10 minutes
until Bedrock answers, and the player stays muted. The thresholds are those of the bundle that scored the trigger; the
system prompt is the served bundle's. After a restart every pending player's check resumes.

| verdict | action |
| --- | --- |
| offender or at risk | `ban` with `role` and a `kick_at` a random time in `kick_delay_min` to `kick_delay_max` after the verdict; the player stays muted until then, and an `unmute` follows at `kick_at` |
| not involved | `unmute`; the pending state ends |
| not sure yet (held) | stays muted; a `notice` at the pending state's first such verdict; checked again on each new line; after `held_cap` of such verdicts, not counting time while Bedrock fails, `unmute` with no ban |

A participant who never triggered is banned only with corroboration (an offending quote of their own line, and a
P(ban) at or above the bundle's `corroboration` threshold on one of their quoted lines), with the same `ban` action.
A participant who is pending is left to their own check. A player is not banned again for lines said before their
last ban (another check of the same conversation reaches the same verdict on the same lines), and a player awaiting
their kick stays muted until it: their lines do not trigger, and no verdict unmutes them before `kick_at`.

**Actions** wait in `<data>/safety/actions.json` (written atomically) until Module-ECC acknowledges each with
`ok: true`; after `ok: false` the action stays and is served again. A restart of either program resends what was not
acknowledged.

**The record.** `<data>/record/YYYY-MM-DD.jsonl` (UTC days), one JSON line per event, each with `type`, `server` (the
config's `server`) and `ts`: every `message` (its text, channel, conversation, speaker, target, `shadowed` and
`bypass` flags, `decision` (`ok`, `trigger`, `shadowed`, `bypass`, `dropped`, `unscored`, `no_score`), `p_ban`,
`margin`, the bundle `version`, and for a reused verdict `rule` and `reused_from`), every trigger (`decision`), every
Kimi call (`slow_call`: the trigger's bundle, the prompt's bundle, the view, attempts, errors, token usage and the
answer), every verdict (`slow_verdict`: outcome, cause and the validated evidence per view), every `action` (with the
verdict's role and cause) and every `ack`. Lines are flushed one at a time. Just after midnight UTC the day's file is
compressed to `YYYY-MM-DD.jsonl.zst`; compressed files older than `record_retention_days` are deleted.

**The journal.** Run `journalctl -u ecc-server -f` to watch decisions as they are made. Each is one info line: a
`trigger` (player, uuid, channel), a `verdict` (player, uuid, outcome, cause, and `check_of`, the pending player whose
slow check it is), an `action` when one is made (type, player, uuid, id) and an `acknowledged` when Module-ECC answers
(type, player, ok, id, and `known`: false for an id that is not, or no longer, unacknowledged). The journal never
holds player text, prompts or model answers; the record has them.

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
| `listen` | `127.0.0.1:8790` | HTTP address, `host:port`; see Connecting Module-ECC |
| `data` | `/var/lib/ecc-server` | bundle store, state, chat-safety state (`safety/`), records (`record/`) and the downloaded llama.cpp release |
| `threads` | `6` | llama-server's `-t` |
| `llama_server` | (none) | run this llama-server binary instead of downloading the bundle's release; required on Windows |
| `wandb_entity` | `telesphoreo-telesphoreo` | W&B entity of the project |
| `wandb_project` | `ecc` | W&B project holding the bundle artifact |
| `wandb_artifact` | `classifier-bundle` | the artifact whose `production` alias names the bundle to serve |
| `check_interval` | `"10m"` | time between W&B checks, at least `1m` |
| `bedrock_region` | `us-east-1` | AWS region of the Bedrock endpoint the slow check calls |
| `bedrock_model` | `us.moonshotai.kimi-k3` | Bedrock model or inference profile ID the slow check calls |
| `server` | (required) | this game server's name in every record line: 1 to 32 of `a-z`, `0-9`, `-` and `_` |
| `queue_size` | `20` | messages a player can have waiting to be scored; past this their oldest waiting one is not scored |
| `kick_delay_min`, `kick_delay_max` | `"2m"`, `"10m"` | a banned player is kicked at a random time in this range after the verdict |
| `held_cap` | `"10m"` | how long "not sure yet" verdicts hold a player muted, not counting time while Bedrock fails; at least `1m` |
| `record_retention_days` | `180` | days the compressed daily records are kept |

The API keys are not config keys; see Updates from W&B and Slow check on Amazon Bedrock. On a machine without systemd
(development), the host reads them from the files `ecc-server.wandb-api-key` and `ecc-server.bedrock-api-key` in the
directory `$CREDENTIALS_DIRECTORY` names.

## Connecting Module-ECC

Every HTTP request needs the host's token, sent as `Authorization: Bearer <token>`; without it, or with a wrong one,
the answer is `401` with no body. The host creates the token on its first start: 32 random bytes, hex, written
atomically to `<data>/token` (mode 0600, owned by the service user), and never logs it. `sudo ecc-server token` prints
it. To replace it, delete the file and restart the service, then paste the new one into Module-ECC's config.

In Module-ECC's `config.yml`, under `chat_safety`:

- `model_host`: ECC-Server's address as the game server reaches it, `http://host:port`.
- `token`: the output of `sudo ecc-server token`. Chat safety stays off while it is empty.

On the same machine, the default `listen` (`127.0.0.1:8790`) and `http://127.0.0.1:8790` work. When the Minecraft
server runs in a Docker container, the container reaches the host on the Docker network: set `listen` in
`/etc/ecc-server/config.toml` to the host's address on that network, for example the bridge gateway
`172.17.0.1:8790`, set `model_host` to `http://172.17.0.1:8790`, and restart the service (`sudo systemctl restart
ecc-server`). The unit is ordered after `docker.service` so the bridge exists when the host binds. A wildcard address
(`0.0.0.0` or `[::]`) listens on every interface; prefer the one address, and do not expose the port beyond the
networks that need it. The token is sent in plain HTTP, so keep it on a network you trust.

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
- `sudo ecc-server update`: have the running host check W&B now, and wait while it downloads and switches to a new
  production bundle. It also installs a production version a rollback skipped or one that failed before.
- `sudo ecc-server status`: print the bundle the running host serves and its health; with a W&B key the last check's
  time and result, the version W&B's `production` names, and any download in progress; the Bedrock key, model
  and last call; and chat safety: the messages waiting to be scored and why scoring is paused, the pending players
  (those being checked and those held), whether Bedrock is failing, and the unacknowledged actions. Exit status 1
  when no host is listening or the host is not ready.
- `sudo ecc-server token`: print the token (see Connecting Module-ECC).
- `ecc-server wandb login`, `ecc-server wandb logout`: store or delete the W&B API key (as root; see Updates from
  W&B).
- `ecc-server bedrock login`, `ecc-server bedrock logout`: store or delete the Amazon Bedrock API key (as root; see
  Slow check on Amazon Bedrock).
- `ecc-server version`: print the package version.

`use` and `rollback` refuse to run as root: run them as the service user (`sudo -u ecc-server ecc-server use ...`),
which owns the data folder the running host also writes.

`status`, `update` and the switch done by `use` and `rollback` read `<data>/token` and send it to the running host, so
they run as root or the service user, and fail with a clear error when the token file cannot be read. They connect to
`listen`; when `listen` is a wildcard address they connect to loopback on its port.

## HTTP interface

All bodies are JSON. Module-ECC uses the first five; every endpoint needs the token (see Connecting Module-ECC).

`POST /messages` with one message:

```json
{"id": "<uuid>", "time": "2026-11-04T21:15:03.120Z", "channel": "chat", "conversation": "chat", "speaker_uuid": "<uuid>", "speaker": "Name", "target_uuid": null, "target": null, "text": "...", "shadowed": false, "bypass": false}
```

`id` is a UUID Module-ECC makes, `time` RFC 3339; `target_uuid` and `target` are set for private messages and mail.
`202` once the message is queued; an `id` already queued is the same message and changes nothing. `400` names what is
malformed; `503` while the host is stopping (send it again). A message whose text is only whitespace is ignored.

`GET /actions` long-polls: it answers as soon as there is an unacknowledged action, or after 30 seconds, with every
unacknowledged action, oldest first:

```json
[{"id": "<uuid>", "type": "ban", "uuid": "<player uuid>", "name": "Name", "role": "offender", "kick_at": "2026-11-04T21:21:40.512Z", "why": {"cause": "kimi"}}]
```

`type` is `mute`, `unmute`, `ban` or `notice`; `role` (`offender` or `at_risk`) and `kick_at` are on `ban` only. A ban
mutes the player until `kick_at`, and an `unmute` follows at `kick_at`. Every action carries `why`, for staff:

| type | `why` |
| --- | --- |
| `mute` | `{"channel", "text"}`: the triggering line's channel and its text as the player typed it |
| `unmute` | `{"cause"}`: `not_involved` (the slow check found no violation), `held_past_cap` (not sure yet for `held_cap`) or `kicked` (the unmute at a ban's `kick_at`) |
| `ban` | `{"cause"}`: `kimi` (Kimi's evidence) or `corroborated` (a participant who never triggered, with corroboration) |
| `notice` | `{"kind": "held"}`: the slow check is not sure yet; sent once per pending state, at its first such verdict |

A `notice` changes nothing: Module-ECC shows it to staff and acknowledges it like any other action.

`POST /actions/ack` with `{"id": "...", "ok": true, "detail": "..."}` once an action is carried out: `200 {}`. Only
`ok: true` ends an action; after `ok: false` it stays and is served again. Every acknowledgement is recorded; a
repeated or unknown id changes nothing.

`GET /muted` is the mute set, `[{"uuid": "...", "name": "..."}]`: every pending player and every banned player whose
`kick_at` has not passed.

`GET /status` is `200` when a bundle is loaded and its llama-server answers its own health check, otherwise `503`
with the reason in `status`; the body is the same either way: `status`, `version` (the bundle), `wandb` (`null`
without a key, otherwise `artifact`, `last_check`, `result`, `production`, `production_digest`, and `download` with
`file`, `done` and `total` bytes while one runs), `bedrock` (`key_set`, `region`, `model`, and after the first call
`last_call` and `result`), and `safety`: `backlog` (messages waiting to be scored), `paused` (why scoring waits,
absent while it runs), `pending` (names), `slow_check` (`answering`, `failure`, `since`, `checking`, `held`) and
`unacknowledged_actions`.

`POST /score` with `{"conversation": "...", "speaker": "...", "text": "..."}` scores a line outside chat safety (no
record, no trigger) for the parity check below, appending it to that conversation's window:

```json
{"p_ban": 0.013, "margin": -2.98, "version": "4b-282f3c43561f", "earlier": 17, "tokens": 233, "evaluated": 12}
```

`p_ban` is `sigmoid(a * margin + b)` with the bundle's Platt parameters; `margin` is `log P(" ban") - log P(" ok")` at
the answer position; `earlier` is the number of earlier lines in the prompt, `tokens` the prompt's length,
`evaluated` how many of them llama-server decoded. `503`: no bundle is served. `502`: llama-server failed and the line
was not added. `422`: neither answer token is among the top logprobs; the line was added but has no score.

Every conversation's lines reach its window one at a time, in order. Chat safety scores one line at a time across
all conversations; `/score` calls for different conversations run in parallel up to llama-server's slot count. A
conversation idle for 30 minutes drops its window; its next line starts a new one. A bundle switch also starts new
windows.

`POST /switch` with `{"version": "..."}` switches to an installed bundle; `use` and `rollback` call it.

`POST /update` runs a W&B check and answers when it is done: `200 {"result": "..."}`, `502` with the error, or `409`
without a key; `update` calls it.

## Parity check

`go run ./parity -ecc <Module-ECC checkout> -bundle <bundle folder> -attacks <attacks.json> -token-file <file>`
replays the evaluation case sets, the attacks set and a sample of Total Freedom conversations through a running host
(`-host` is its address, `http://127.0.0.1:8790` by default; `-token-file` names a file holding its token, for example
a copy of the output of `sudo ecc-server token`), in conversation order, and compares each message's margin with the
margin `uv run eval` recorded (`<work>/eval/classifier/*.jsonl`; `<work>` is `[paths] work` in the checkout's `data/local.toml`, which the tool
reads). Case sets are the folders of `.txt` case files in `ecc.toml`'s `[eval.sets]`, read in name order. The attacks
set is built by the code in the pipeline's `data/cases/attacks/`; write `attacks.json` from the checkout with
`uv run python -c "import json; from ecc.core import Config; from ecc.evaluate import attack_conversations; print(json.dumps(attack_conversations(Config())))" > attacks.json`.
