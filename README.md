# ECC-Server

The chat-safety decision maker for Module-ECC. It is one Go binary beside the game server, with the OpenVINO runtime its models
run on. Module-ECC asks it
for a verdict on every line a player writes before the line is delivered, and carries out the actions it gives; every
decision is made here: the verdict on each line (`deliver` or `hold`), each player's sitting and its held lines, the
checkpoints at which a judge (a model on Amazon Bedrock) reads the sitting and rules `nothing`, `hold` or `ban`, the
actions that carry out each ruling, the judge's spend budget, the review queue and the daily record. The judge's
question, the owner's ruled cases it is shown, the checkpoint settings and the fast model's thresholds come from the
bundle the Module-ECC training pipeline writes; with a W&B API key, the host fetches the bundle the pipeline promoted to
`production` in Weights & Biases. The Bedrock key and the bundle's texts never reach Module-ECC.

The decisions are made by the package `core/`, which has no clock, network or file of its own: the HTTP server drives
it on the host's clock, and `ecc-server replay` drives it on a virtual clock over recorded lines.

**The fast model.** Two models score each line on the host's CPU: SH, a small model, on every line, and C, a large
model, on the lines SH passes; C's score decides `hold` or `deliver`. They run in the server's process through
OpenVINO's C API (no cgo: the binary loads `/usr/lib/ecc-server/openvino` at start), on Linux only. They take about
4 GB of memory, 2.7 GB of it the graphs' weights mapped from the bundle's files. On another system `ecc-server run`
starts only with the test rig's scorer (`[test] hold_words_file`, see Configuration), which holds a line that contains
one of a file's words and logs a warning at every start that the server filters nothing; it exists for the end-to-end
rig, never for a game server with players.

## Install (Debian, Ubuntu)

ECC-Server is packaged as an amd64 `.deb` that depends on `ca-certificates` (the HTTPS calls to W&B and Bedrock) and
`libstdc++6` (OpenVINO). The package installs:

- `/usr/bin/ecc-server`, the binary
- `/usr/lib/ecc-server/openvino/`, the five OpenVINO 2026.4.1 libraries the models run on (77 MB, from OpenVINO's
  release archive), the release the bundles are exported with
- `/lib/systemd/system/ecc-server.service`, which runs `ecc-server run` as the `ecc-server` user with
  `Restart=always`
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
sudo ecc-server status          # the bundle, W&B, the judge, its spend and the sittings
sudo ecc-server token           # the token to paste into Module-ECC's config (see Connecting Module-ECC)
```

The host starts without a bundle and answers `503` to lines until it has one. With the W&B key set, it downloads the
production bundle on its first check and serves it. Without W&B, install a bundle folder copied onto the host instead:
`sudo -u ecc-server ecc-server use /path/to/bundle` (the folder must be readable by `ecc-server`).

Upgrade with `sudo apt update && sudo apt upgrade`. The service restarts: open sittings end (see Restart); the
installed bundle, the token, the W&B and Bedrock keys, your edited `config.toml`, the unacknowledged actions, the
judge's spend, the review queue and the records stay. Remove with `sudo apt remove ecc-server` (stops and disables the
service, keeps `/etc/ecc-server`, the keys and the data folder); `sudo apt purge ecc-server` also deletes
`/var/lib/ecc-server` (the bundles, the actions, the spend, the review queue, the records and the token), the stored
W&B and Bedrock keys, and the `ecc-server` user.

## The bundle

A bundle is a folder: `bundle.json`, `manifest.json` (every file's SHA-256 and size, by its path in the folder, such
as `models/c.bin`) and the files `bundle.json` names: the fast model's graphs and tokenizers, the judge's
`question.md` and `examples.json`. A manifest name that would leave the folder is refused. From `bundle.json` the host
reads:

- `graphs`, `tokenizers`, `special_tokens`, `outputs`, `kinds`, `openvino` (the release and the compile
  settings), `context` (the lists of earlier lines a line is scored with), `sh` and `c` (the models' inputs): how a
  line is scored;
- `channels`: the channels a line may have;
- `text`: how a line's raw text and names are prepared (see A line), with its limits, marks, whitespace and patterns;
- `rules`: the paths of every rule the host implements in code, from `context.rule` to `links.rule`. The host hashes
  the values at those paths as `rules.how` states; a bundle whose rules hash differently from the ones this host
  implements is refused, and so is one whose text limits, marks, whitespace or link pattern differ;
- `thresholds`: `sh_pass`, `hold` (by name) and `default`, the hold threshold to use, and, when set, `kinds`
  (a threshold per kind on C's logits; without them no line is marked with a kind);
- `judge.question`: `question.md`, whose `json` block holds the prompt's fixed strings (`QUESTION`, `ADDED`, `FORMAT`
  and the separators, which must be the ones the host lays the prompt out with, or the bundle is refused);
- `judge.examples`: `examples.json`, the owner's ruled cases (`examples`: `id`, `ruling`, `date`, `text`) in the
  order the prompt lists them, and its `header`, which must be `## C<n>: ruling: <ruling>`;
- `judge.checkpoints`: `schedule`, `every`, `wait_seconds`, `sitting_end_seconds`, `review_held` (see Sittings and
  checkpoints);
- `judge.render`: how a sitting is shown to the judge: `before`, `after`, `max_chars`, `cut_before`, `cut_after`, and
  `kinds`, the name the judge reads for each kind the fast model marks;
- `links`: `canonical`, the link the models were trained with in place of an allowed link, and the link `pattern` and
  `rule` (see A line).

The config replaces only the checkpoint schedule, `every` and the wait when it sets them (see Configuration).

## Updates from W&B

The pipeline uploads each bundle to W&B as a version of the `classifier-bundle` artifact. Promoting a bundle is moving
the artifact's `production` alias to it in W&B; the host picks it up within `check_interval` (10 minutes by default),
or at once on `sudo ecc-server update`.

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
2. installs the folder into the store, checking every file against the manifest's SHA-256s and `bundle.json` as
   `use` does;
3. switches to it as `use` does, and records which W&B digest it came from, so a restart does not download it again.

A bundle that fails any of these checks is discarded and logged with the reason; the current bundle keeps serving,
and the host does not fetch that production version again until production moves, an operator runs
`sudo ecc-server update`, or the service restarts. A bundle version is installed once: a changed bundle needs a new
version.

**Offline.** When W&B is unreachable or answers with an error, the host keeps serving the bundle on disk and tries
again at the next check. It logs the first failed check of an outage and the first check that succeeds after it.

**Rollback.** `ecc-server rollback` needs no W&B: the store keeps the previous bundle. When the bundle rolled back
from came from W&B, the host does not install that production version again until W&B's `production` moves to
another version, or until `sudo ecc-server update`, which installs whatever production is. Moving `production` back to
the earlier version in W&B also switches the host back, at its next check. With a key set, a bundle installed with
`use` serves until the next check, which switches to production.

## Chat safety

**A line.** Module-ECC sends each line (`POST /line`) as it was written: the raw text, the account name, on chat the
name the console shows. The host times it when the request arrives and numbers it `L<n>` in the current stream (a
stream runs from one game server start to the next). A line on a channel `bundle.json` does not name is held, never
scored and never context; the host logs it with the channel's name, counts it in `/status` (`unknown_channels`) and
puts the player's sitting on the review queue when it ends. A Discord relay line (channel `discord`, no speaker UUID)
or a line marked `trusted` (Module-ECC delivered it at once: its speaker is in one of the module's trusted groups or
holds the bypass permission) is kept as context for other players' lines only: never scored, held or judged, and in no
sitting. A private message is a `msg` line; its conversation is the pair of the two account names. Every other line is
scored by the fast model with the lines before it that the bundle's `context` names (the public lines, a private
message's earlier messages between the same two players, a placed line's author's earlier placed lines: the last 30
within 30 minutes); a line the scorer cannot score is held. Lines are scored one at a time, in the order they arrive. A
line still waiting for its verdict `[model] max_wait` after it arrived is held, unscored if its models have not run (a
burst of lines is held rather than queued past the time Module-ECC waits), and a model call is cut off at that point or
after `[model] call_timeout`. After a restart or a bundle switch, the first lines are scored with only the lines since
as their context.

**Text preparation.** On arrival the host prepares the text as the training pipeline prepared its lines
(`bundle.json` `text`): `§` codes and `&` colour codes removed, a CR made a space and a line break ` / `, whitespace
collapsed, a link over 60 characters cut to its first 50 and `[...]`, a text over its channel's limit (book 1000, nick
80, any other 400) cut with ` [...]`, and IPv4 and IPv6 addresses made `[ip]`. Lengths count code points, and the
rules match as the pipeline's Python patterns do (Unicode whitespace and digits, Python's case folding). A text that
preparation empties is delivered and dropped: it is not scored and is no context. A chat name loses its codes, the
client's `[Not Secure]` mark and its icon codes, a Discord name its tags; the judge sees the prepared shown name on chat,
the prepared Discord name on discord and the account name on every other channel. The models, the context and the
judge read the prepared text; the record keeps the raw text and name beside it. In the text the scorer sees, the
line's and its context's, each link the allowlist allows is replaced by the bundle's `links.canonical`, the whole
match of `links.pattern` with its scheme, port and path: an entry `host` or `host/prefix` (Module-ECC sends the list
with `start`) allows a link whose host is the host or one of its subdomains and whose path, any case, starts with the
prefix (punctuation after the link counts as path). The record and the judge see the link. The host keeps the last
allowlist in `<data>/links.json` for its next start; before any `start`, no link is replaced.

**Repeats.** A line is a repeat of an earlier line of the stream with the same name, channel and text within 60
seconds (whole seconds) of that line's last copy, whatever lines came between, as the training pipeline folded
repeats (`bundle.json` `text.repeat`): the name is, on chat, the shown name followed by `=` and the account when it
does not contain the account; on discord the Discord name; elsewhere the account; the channel carries the recipient on
msg and mail and the player named on nick; the text is the prepared text before addresses become `[ip]`. A repeat is
not scored and is no context for later lines, it gets the verdict of the line it repeats (a copy of a held line is
held, and delivered with it if a ruling releases it; a copy of a line a ruling already released is delivered), the
judge sees the line once with "(sent N times)", and it is no new held line for the checkpoints. A release lists the
held lines and their repeats in the order they arrived.
**Kinds.** On a line C scores, the fast model marks each kind whose logit is at or over its `thresholds.kinds` value.
The judge sees the player's marked lines with the kinds' names from `judge.render.kinds`, and a marked line, held or
not, is shown with the lines around it.

**Sittings and checkpoints.** A sitting runs from a player's first scored line after they join until
`sitting_end_seconds` after they leave (with no leave known, after their last line); a game server start ends every
sitting. The judge is called at a sitting's checkpoints: after each held-line count in the schedule, then every
`every`-th held line; when a held line has waited the wait without a ruling; and at the sitting's end while held lines
are unruled. One call per sitting is in flight; a later checkpoint waits for its answer. The case the judge reads is
headed with the UTC date of its first line and shows each line at its UTC time of day. A ruling covers the held lines
up to its checkpoint's last line:

| ruling | action |
| --- | --- |
| `nothing` | `release`: deliver those held lines, and their repeats, in the order they arrived |
| `hold` | the lines stay held; `warn` the player, once a sitting |
| `ban` | `ban`: an indefinite ban and an immediate kick; the player's held lines are dropped and the sitting ends |

A checkpoint the judge does not rule (three invalid answers, a refusal by Bedrock, a prompt over `max_prompt_chars`,
the day's calls or a budget used up, no Bedrock key, or a judge it could not reach before the sitting ended) leaves
its lines held.

**The review queue.** `<data>/review/queue.jsonl` gets one JSON line for each ban, each sitting that ends with
`review_held` or more held lines and no ban, each sitting with a checkpoint the judge did not rule, each sitting with a
line on a channel the bundle does not name, and each sitting a restart ended with held lines no ruling covered.

**Restart.** Nothing about a sitting is reloaded. At start the host reads the last two days' record for sittings that
started and did not end, records each one's end, and puts each with held lines no ruling covered on the review queue.
Their held lines stay held: Module-ECC delivers a held line only on a `release`. Module-ECC sends `start` when it
starts, so a host and plugin restarted together begin a clean stream.

**Actions** wait in `<data>/actions.json` (written atomically) until Module-ECC acknowledges each with `ok: true`;
after `ok: false` the action stays and is served again. A restart of either program resends what was not
acknowledged.

**The record.** `<data>/record/YYYY-MM-DD.jsonl` (UTC days), one JSON line per event, each with `type` and `server`
(the config's `server`): every `line` (stream, id, time, channel, conversation, account, name and the raw name,
target, the prepared and the raw text, sitting, verdict, kinds, score), `repeat`, `sitting_start`, `checkpoint` (the
held-line count it covers, why it was made, the prompt's size and SHA-256, whether it was sent), `judge_call` (each
call: attempt, status, the usage's input, cached, cache-write, output and reasoning tokens, cost, duration, or the
error), `ruling` (the judge's answer, the error, or `pending` with the time it is put again), `checkpoint_again`,
`action` (the core's, by line id), `action_sent` (as Module-ECC gets it), `ack`, `presence`, `sitting_end` (outcome, held, released and unruled
lines, the checkpoints) and `review`. Lines are flushed one at a time. Just after midnight UTC the day's file is
compressed to `YYYY-MM-DD.jsonl.zst`; compressed files older than `record_retention_days` are deleted.

**The journal.** Run `journalctl -u ecc-server -f` to watch rulings and actions as they are made, and each judge
call's size, token counts, cost and duration. The journal never holds player text, prompts or the judge's answers; the
record has the text and the rulings.

## The judge on Amazon Bedrock

**The key.** `sudo ecc-server bedrock login` reads an Amazon Bedrock API key from a hidden prompt (or from standard
input when piped) and checks it with a small Responses call (16 output tokens, no reasoning) to `judge.model` in
`judge.region`,
which proves both the key and its access to the model. A key Bedrock rejects, or any failed check, is not stored. A
key that passes is stored the way the W&B key is: encrypted with `systemd-creds` into
`/etc/credstore.encrypted/ecc-server.bedrock-api-key`, loaded by the unit's
`LoadCredentialEncrypted=ecc-server.bedrock-api-key`, and the service is restarted if it is running. Without a stored
key the service starts and gives verdicts, and no checkpoint is judged. `sudo ecc-server bedrock logout` deletes the
key and restarts the service. Bedrock API keys expire; when Bedrock refuses the key, the host logs it once, with the
fix, until a call succeeds again.

**The call.** Each checkpoint is one Responses request, `POST
https://bedrock-runtime.<region>.amazonaws.com/openai/v1/responses` with `Authorization: Bearer <key>`:

```json
{"model": "us.moonshotai.kimi-k3.1", "max_output_tokens": 16000, "reasoning": {"effort": "medium"},
 "service_tier": "flex", "prompt_cache_options": {"mode": "explicit"}, "store": false,
 "input": [{"type": "message", "role": "user", "content": [
   {"type": "input_text", "text": "<the question and the owner's cases>", "prompt_cache_breakpoint": {"mode": "explicit"}},
   {"type": "input_text", "text": "<the new case and the answer format>"}]}]}
```

The two parts, joined, are the prompt: the question, the owner's cases, the sitting so far with the held lines marked
`[held]`, and the answer format. The first part is the same in every call and ends at an explicit cache breakpoint, so
Bedrock serves it from its prompt cache (at least 30 minutes; a hit resets the time, and a request another server takes
writes a new entry). `store` is false: Bedrock keeps no copy of the prompt or the answer. The answer is the output
message's text; an answer whose `status` is `incomplete` (its output cut at `max_output_tokens`) is invalid. A valid
answer is one JSON object (a Markdown code fence around it is accepted) whose `rulings` list holds one entry for the
case `N1`: `ruling` `nothing`, `hold` or `ban` (`not ban`, a ruling of the owner's cases, is taken as `hold`); for
`hold` or `ban`, `by` one of the player's lines the case shows; `like` a list of at most three of the owner's case
ids; `sure` true or false. An invalid answer is asked for again, up to three answers. A call that fails (`429`, a
`5xx`, the `timeout`, the network) is made again after 5 seconds, then 10, 20, ..., while the failures have lasted
less than 5 minutes; then the checkpoint is pending: it is put again 5 minutes later, and so on until the sitting ends
(`/status` counts the pending ones). A call refused by the budget, or by Bedrock for the key (`401`, `403`) or as a
bad request, is not made again.
**The budget.** The host adds up each call's cost from the usage Bedrock returns at the configured prices, halved on
the flex tier: the input tokens neither read from nor written to the cache at the input price, `cached_tokens` at the
cache-read price, `cache_write_tokens` at the cache-write price (Bedrock documents that field for its OpenAI models; when
an answer does not state it, those tokens count at the input price), and the output, reasoning included, into the UTC day's and the UTC month's totals in
`<data>/spend.json`, which a restart keeps. Before each call it reserves the call's worst case: every byte of the
prompt a token at the dearest input price, plus `max_output_tokens` at the output price. The reservation counts as
spent at once and is saved, so a call cut off by a crash or a restart stays charged its worst case; when the call
returns, its cost replaces the reservation (nothing when Bedrock refused the request). A call whose worst case would
take the day's or the month's total past its budget waits while other calls are in flight, and is not made once none
is: its lines stay held, its sitting goes to the review queue, and the first refusal in a day (or month) sends a
`notice` that staff with the alert permission see. `sudo ecc-server status` shows the totals.

## Building the package

`./package.sh [version]` (needs Go, curl, GNU tar and sha256sum; nothing is installed) fetches OpenVINO's pinned
release archive into `dist/` (once; checked against its published SHA-256) and takes the five libraries from it,
builds the binary (`CGO_ENABLED=0`, from any system) and runs nfpm, pinned to a version, into `dist/ecc-server_<version>_amd64.deb`, then prints the `telesphorepo publish` command for it. The version
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
| `data` | `/var/lib/ecc-server` | bundle store, state, actions, spend, review queue (`review/`) and records (`record/`) |
| `server` | (required) | this game server's name in every record line: 1 to 32 of `a-z`, `0-9`, `-` and `_` |
| `record_retention_days` | `180` | days the compressed daily records are kept, at least 3 |
| `wandb_entity` | `telesphoreo-telesphoreo` | W&B entity of the project |
| `wandb_project` | `ecc` | W&B project holding the bundle artifact |
| `wandb_artifact` | `classifier-bundle` | the artifact whose `production` alias names the bundle to serve |
| `check_interval` | `"10m"` | time between W&B checks, at least `1m` |
| `model.threads` | `6` | CPU threads of each model call (one runs at a time) |
| `model.call_timeout` | `"800ms"` | a model call that takes longer is cut off and its line held |
| `model.max_wait` | `"900ms"` | a line still waiting this long after it arrived is held; keep it under Module-ECC's `verdict_timeout_ms` |
| `model.threshold` | the bundle's default | the name of the bundle's hold threshold to use (`thresholds.hold`) |
| `judge.region` | `us-east-1` | AWS region of the Bedrock endpoint |
| `judge.model` | `us.moonshotai.kimi-k3.1` | Bedrock model or inference profile ID |
| `judge.service_tier` | `flex` | Bedrock service tier: `flex`, `default` or `priority` |
| `judge.reasoning_effort` | `medium` | `reasoning.effort`: `none`, `low`, `medium`, `high`, `xhigh` or `max` (the model's own default is `max`) |
| `judge.max_output_tokens` | `16000` | the most output tokens a call may produce, reasoning included |
| `judge.timeout` | `"10m"` | how long a call may take |
| `budget.daily_usd`, `budget.monthly_usd` | `25`, `250` | what the judge may spend in a UTC day and a UTC month |
| `budget.input_per_million`, `output_per_million`, `cache_read_per_million`, `cache_write_per_million` | `4.40`, `22.00`, `0.22`, `5.50` | standard-tier prices in dollars per million tokens; halved on `flex` |
| `budget.max_prompt_chars` | `500000` | a longer prompt is not sent |
| `budget.max_calls_per_day` | `2000` | the most calls in a UTC day |
| `chat.schedule`, `chat.every`, `chat.wait` | the bundle's | set, they replace the bundle's checkpoint schedule, every-n-th held line and wait |
| `test.hold_words_file` | (none) | the test rig's scorer: a file of words, one a line; a line containing one is held |
| `test.bedrock_endpoint` | (none) | replaces the Bedrock runtime endpoint, for the rig's stub |
| `test.judge_backoff`, `test.judge_retry_for`, `test.judge_retry_after` | `5s`, `5m`, `5m` | for the rig: the first wait after a failed call, how long failures are retried, when a pending checkpoint is put again |

The API keys are not config keys; see Updates from W&B and The judge on Amazon Bedrock. On a machine without systemd
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

Every command except `version` and `replay` takes `-config FILE` before its arguments (default
`/etc/ecc-server/config.toml`).

- `ecc-server run`: serve the current bundle. With none installed it answers `503` to lines until `use` installs one.
- `ecc-server use FOLDER`: install a bundle folder into the store and switch to it. Every file is checked against the
  manifest's SHA-256 and size, and `bundle.json` and the judge's files are read as the host reads them; files are
  hardlinked when the folder is on the same volume, otherwise copied. With the host running, it takes the new
  bundle's texts and settings at once and its open sittings carry on; a bundle that fails to load is discarded and the
  current one keeps serving. Without a running host, the bundle is served from the next start.
- `ecc-server rollback`: switch back to the previous bundle, the same way. The store keeps the current and the
  previous bundle and deletes older ones.
- `sudo ecc-server update`: have the running host check W&B now, and wait while it downloads and switches to a new
  production bundle. It also installs a production version a rollback skipped or one that failed before.
- `sudo ecc-server status`: print the bundle the running host serves, its health and scorer; with a W&B key the last
  check's time and result, the version W&B's `production` names, and any download in progress; the judge's model,
  tier, key and last call; the judge's spend today and this month against the budgets; the open sittings, their
  unruled held lines and the judge calls in flight; and the unacknowledged actions. Exit status 1 when no host is
  listening or the host is not ready.
- `sudo ecc-server token`: print the token (see Connecting Module-ECC).
- `ecc-server wandb login`, `ecc-server wandb logout`: store or delete the W&B API key (as root; see Updates from
  W&B).
- `ecc-server bedrock login`, `ecc-server bedrock logout`: store or delete the Amazon Bedrock API key (as root; see
  The judge on Amazon Bedrock).
- `ecc-server replay ...`, `ecc-server score ...`: see Replay.
- `ecc-server version`: print the package version.

`use` and `rollback` refuse to run as root: run them as the service user (`sudo -u ecc-server ecc-server use ...`),
which owns the data folder the running host also writes.

`status`, `update` and the switch done by `use` and `rollback` read `<data>/token` and send it to the running host, so
they run as root or the service user, and fail with a clear error when the token file cannot be read. They connect to
`listen`; when `listen` is a wildcard address they connect to loopback on its port.

## HTTP interface

All bodies are JSON. Module-ECC uses the first four; every endpoint needs the token (see Connecting Module-ECC).

`POST /line` with one line answers its verdict:

```json
{"id": "<uuid>", "channel": "chat", "speaker_uuid": "<uuid>", "speaker": "Name", "shown": "[Mod] ~Nick",
 "target_uuid": null, "target": null, "text": "<the raw text>", "trusted": false}
```

`{"verdict": "deliver"}` or `{"verdict": "hold"}`. `id` is Module-ECC's own id for the line: Module-ECC keeps a held
line by it until a `release` names it or a `ban` drops it, and a line sent again with the same id gets the same verdict
and is not counted twice. `channel` is one of `bundle.json` `channels` (another is held, see A line). `speaker` is the
account name (the models tell players apart by it), and on `discord` the Discord name; `speaker_uuid` is `null` only
on `discord`. `shown` is the chat line's name as the console shows it. `target_uuid` and `target` are set for private
messages and mail (the recipient) and for a nickname set on another player. `text` is the text as written; the host
prepares it. A line with `trusted: true`, or on `discord`, is context only. `400` names what is malformed; `503` while
no bundle is served, or while 10,000 lines wait to be scored (Module-ECC asks again).

`POST /presence` with `{"kind": "start", "allowed_links": ["example.org", "discord.gg/<code>"]}` when Module-ECC starts
(a new stream: every open sitting ends; the allowlist of links, each `host` or `host/prefix`, `400` naming an entry
that is not), or `{"kind": "join" | "leave", "uuid": "<uuid>", "name": "Name"}`: `200 {}`.
`GET /actions` long-polls: it answers as soon as there is an unacknowledged action, or after 30 seconds, with every
unacknowledged action, oldest first:

```json
[{"id": "<uuid>", "type": "release", "uuid": "<player uuid>", "name": "Name", "ids": ["<line id>", "<line id>"]}]
```

| type | fields | what Module-ECC does |
| --- | --- | --- |
| `ban` | `uuid`, `name` | ban the player indefinitely, kick them at once, drop their held lines |
| `warn` | `uuid`, `name` | show the player the warning; their held lines stay held |
| `release` | `uuid`, `name`, `ids` | deliver the held lines with these ids in the order they were written (the host lists the lines and their repeats in the order they arrived) |
| `notice` | `kind`, `text` | show `text` to staff with the alert permission (`kind` `budget`: a judge budget is used up) |

`POST /actions/ack` with `{"id": "...", "ok": true, "detail": "..."}` once an action is carried out: `200 {}`. Only
`ok: true` ends an action; after `ok: false` it stays and is served again. Every acknowledgement is recorded; a
repeated or unknown id changes nothing.

`GET /status` is `200` when a bundle is served, otherwise `503`; the body is the same either way: `status`, `version`
(the bundle), `scorer`, `lines_waiting` (lines queued for the models), `wandb` (`null` without a key, otherwise `artifact`, `last_check`, `result`, `production`,
`production_digest`, and `download` with `file`, `done` and `total` bytes while one runs), `judge` (`key_set`,
`region`, `model`, `service_tier`, `reasoning_effort`, `since_start` (calls, answered calls and their summed usage), and
after the first call `last_call`, `result` and `last_usage`: `input_tokens`, `cached_tokens`, `cache_write_tokens`,
`output_tokens`, `reasoning_tokens`), `spend` (`day`, `day_usd`,
`daily_budget_usd`, `day_calls`, `month`, `month_usd`, `monthly_budget_usd`, `in_flight_reserved_usd`), `sittings`
(`stream`, `open_sittings`, `held_unruled`, `judge_calls_in_flight`, `checkpoints_pending`, `unknown_channels` by
channel) and `unacknowledged_actions`.

`POST /switch` with `{"version": "..."}` switches to an installed bundle; `use` and `rollback` call it.

`POST /update` runs a W&B check and answers when it is done: `200 {"result": "..."}`, `502` with the error, or `409`
without a key; `update` calls it.

## Replay

`ecc-server replay -config FILE -lines FILE -fold DIR -out DIR [-mode parity|live] [-judge-delay 15s]` feeds recorded
lines (prepared already, with presence events) through the decision core on a virtual clock, with the fast model's
recorded verdicts and the judge's saved answers (by the SHA-256 of the prompt), and writes the record, one line per
sitting and a summary to `DIR`. `-config` is a TOML file with the core's settings in a `[core]` table. The core's
timers run at their time, and a ruling arrives `-judge-delay` after its checkpoint, as an event of its own: the lines
before then are handled first. Parity mode takes the sittings from the lines, rules at once, and checks every prompt
and every sitting's outcome against the measured run; live mode makes sittings from presence and silence and uses the
waiting-time checkpoint. With `-bundle DIR` (`-threads`, `-threshold`) the bundle's models score every line in place
of the recorded verdicts (Linux only). `ecc-server replay -h` describes the inputs.

`ecc-server score -bundle DIR -in FILE -out FILE [-ids] [-allow FILE]` takes lines as Module-ECC sends them, handles
each as the host does (prepared, refused, dropped, folded, links replaced, scored) and writes the format of the
training pipeline's reference scorer (`ecc.reference`), the prepared texts, keys, token ids and features included with
`-ids`, to check the host against it (Linux only). `ecc-server score -h` describes the formats.