# open-warden

Guarded, budgeted agent execution in a single container.

`open-warden` is an abstraction layer that runs coding agents on top of an
embedded inference engine. It adds what raw agent CLIs lack: **proactive**
rate-limit control, hard per-run cost ceilings, automatic model fallback,
subagent policy, parallel agents in one container, and a detailed usage report
that includes everything subagents consumed.

The engine is an implementation detail: its name, flags and output never reach
the user.

```
┌──────────────────────────────────────────────────────────┐
│ open-warden (CLI)                                        │
│                                                          │
│  prompts ──► admission ──► run ──► fallback chain         │
│  (global +   (RPM/TPM/     │        (rate_limit/quota/    │
│   per agent)  RPD/TPD)     │         unavailable)         │
│                            ▼                              │
│                     budget poller ──► stop at $ limit     │
│                            │                              │
│                            ▼                              │
│                  usage collector (session tree)           │
│                  main + subagents, per model              │
└──────────────────────────────────────────────────────────┘
```

## Features

1. Runs via `docker run` (no compose required).
2. Proactive RPM, TPM, RPD and TPD limits, defined by the application per
   model.
3. Per-run cost ceiling in USD; the run is interrupted when reached.
4. Automatic fallback on rate limit, insufficient balance, or provider
   unavailability. Rate limits are handled **before** the request: if the
   primary would exceed its window, the next model is used immediately.
5. Multiple agents running in parallel inside the same container, sharing one
   rate-limit ledger.
6. Global prompts plus per-agent prompts.
7. Detailed report: input, output, reasoning, cached tokens, subagent usage,
   request counts, cost, fallback events.
8. Fixed subagent delegation policy (not user configurable) plus a
   configurable subagent model and fallback.
9. Subagent fallback: when the main model falls back (e.g. free tier
   exhausted), subagents move to their fallback model too.
10. The embedded engine is never exposed to users: output and errors are
    sanitized and reported in warden vocabulary.
11. A loopback request gateway puts warden in the path of every provider call
    (per-request RPM/TPM/RPD/TPD) for OpenAI-compatible providers.
12. A built-in model/provider registry works without configuration and exposes
    aliases, labels, context and prices — never raw engine model ids or the
    fallback chain.
13. One-off runs against any OpenAI-compatible endpoint with `--base-url` and
    `--api-key`, still routed through the gateway.

## Quick start with Docker

```bash
# 1. Build or pull
docker pull esdrasschonevald/open-warden:latest

# 2. Run one agent against your repository
docker run --rm -it \
  -e SILICONFLOW_API_KEY -e DEEPSEEK_API_KEY \
  -v "$PWD:/workspace" \
  -v warden-data:/data \
  esdrasschonevald/open-warden:latest \
  run -c /workspace/warden.yaml "Fix the failing tests"
```

`/workspace` is the mounted repository. `/data` is the single persistent
volume: engine credentials/config, rate-limit windows and run history all live
there.

Run several agents in parallel (from `prompt.agents` in the config):

```bash
docker run --rm -it \
  -e SILICONFLOW_API_KEY -e DEEPSEEK_API_KEY \
  -v "$PWD:/workspace" -v warden-data:/data \
  esdrasschonevald/open-warden:latest \
  agents -c /workspace/warden.yaml "Implement the feature described in TASK.md"
```

No compose file is involved. Each invocation is a plain `docker run`.

Discover what is available without any config:

```bash
docker run --rm -e SILICONFLOW_API_KEY esdrasschonevald/open-warden:latest models
docker run --rm esdrasschonevald/open-warden:latest providers
```

> API keys are read from the environment (`SILICONFLOW_API_KEY`,
> `DEEPSEEK_API_KEY`, `ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY`). Never bake
> them into the image.

## Local build

```bash
make build          # dist/open-warden
make test           # unit + integration tests
./dist/open-warden run -c warden.yaml "your task"
```

## CLI

```
open-warden run [prompt...]     Run one agent
open-warden agents [task...]    Run every configured agent in parallel
open-warden models              List available models (alias, context, price)
open-warden providers           Show provider credential status
open-warden doctor              Check engine, credentials and state
open-warden report              Show usage history
open-warden validate            Validate config and print effective policy
open-warden help [command]      Show help
open-warden version
```

Every command supports `--help` (for example `open-warden run --help`).

Common flags for `run` / `agents`:

| Flag | Description |
| --- | --- |
| `-c, --config <file>` | Configuration file (default: `./warden.yaml`, then `$WARDEN_CONFIG`). |
| `--agent <name>` | Agent profile from `prompt.agents`. |
| `-m, --model <model>` | Override the primary model. |
| `--fallback <model>` | Prepend a fallback model. |
| `-b, --budget <usd>` | Override the per-run budget. |
| `-w, --workdir <dir>` | Working directory for the agent. |
| `--prompt-file <file>` | Read the task from a file. |
| `--timeout <duration>` | Run timeout (e.g. `20m`). |
| `--json` | Machine-readable output, including the full report. |
| `-q, --quiet` | Do not stream assistant text. |
| `--base-url <url>` | One-off OpenAI-compatible endpoint; requires `-m`. |
| `--api-key <key>` | Override the provider credential (also `WARDEN_API_KEY`). |
| `--api-key-file <file>` | Read the credential from a file. |
| `--economy <mode>` | Token economy: `off`, `balanced` (default), `aggressive`. |
| `--no-economy` | Disable the economy mode. |
| `--subagents-model <alias>` | Model used by subagents. |
| `--subagents-fallback <alias>` | Subagent fallback model. |
| `--report-file <file>` | Write the run report as JSON while still streaming text. |

`models` accepts `--all` (include models whose provider key is not set) and
`--json`. `providers` accepts `--json`. Neither needs a configuration file.

The prompt comes from arguments, `--prompt-file`, or stdin when piped.

Exit codes: `0` success, `1` failure, `2` usage error, `3` budget reached.

## Usage examples

### Discover models

```bash
open-warden models
```

No configuration file is needed. By default this lists models whose provider
has a key configured, plus the managed `free-tier` models. Use `--all` to see
every model known to the application.

```
ALIAS             LABEL               CTX      $IN/M     $OUT/M
qwen-coder-30b    Qwen3 Coder 30B     262k       0.07       0.28
glm-5.3-flash     GLM-5.3 Flash       200k       0.15       0.50
deepseek-flash    DeepSeek Flash      128k       0.15       0.60
```

The list is intentionally small: **alias, label, context, input price, output
price**. Models, providers, prices and limits are built into the application;
they cannot be overridden by configuration. The underlying engine model id and
fallback chain stay internal.

`open-warden models --json` returns the same fields for a UI, and
`open-warden models qwen-coder-30b` shows one entry.

### Run with an alias

```bash
open-warden run -c warden.yaml -m qwen-coder-30b --budget 2.00 \
  "Fix the failing tests"
```

Aliases are resolved internally; `-m` also accepts a raw provider/model id for
administrative use.

### One-off endpoint without config

```bash
open-warden run \
  --base-url https://api.deepseek.com \
  --api-key "$DEEPSEEK_API_KEY" \
  -m deepseek-flash \
  "Fix the failing tests"
```

The custom endpoint still goes through the local gateway. `--api-key` alone
overrides the credential of the provider selected by `-m`/config, without
changing the provider. Prefer `--api-key-file` or `WARDEN_API_KEY` to keep the
key out of shell history.

### Run every configured agent in parallel

```bash
open-warden agents -c warden.yaml "Implement the feature described in TASK.md"
```

### Check providers and credentials

```bash
open-warden providers
```

No configuration file is needed.

```
PROVIDER     UPSTREAM                              ENV                        STATUS
deepseek     https://api.deepseek.com              DEEPSEEK_API_KEY           configured
siliconflow  https://api.siliconflow.com/v1        SILICONFLOW_API_KEY        configured
```

The command never prints key values. Use `open-warden doctor` to check the
engine, state directory, gateway and model catalog as well:

```bash
open-warden doctor -c warden.yaml
```

### Reports

```bash
open-warden report -c warden.yaml --last 20
open-warden report -c warden.yaml --json
```

### Docker with an env file or secret

```bash
# env file (chmod 600, never committed)
docker run --rm -it --env-file ./warden.env \
  -v "$PWD:/workspace" -v warden-data:/data \
  esdrasschonevald/open-warden:latest \
  run -c /workspace/warden.yaml -m qwen-coder-30b "Fix the tests"

# Docker secret: name the variable <ENV>_FILE
docker run --rm -it \
  -e SILICONFLOW_API_KEY_FILE=/run/secrets/siliconflow \
  -v /run/secrets:/run/secrets:ro \
  -v "$PWD:/workspace" -v warden-data:/data \
  esdrasschonevald/open-warden:latest \
  models
```

## How requests flow

The gateway is always on for pay-as-you-go providers:

```
engine -> 127.0.0.1:<random>/<provider> -> warden gateway -> provider
```

Before forwarding, the gateway estimates the request weight and checks the
application-defined RPM, TPM, RPD and TPD windows for the model. If a window is
closed, it waits up to 20 seconds and then returns 429; actual tokens from the
response (including SSE usage frames) reconcile the estimates. This is **per
request**, so tool calls and subagent requests are covered too.

- **OpenAI-compatible providers** (SiliconFlow, DeepSeek, OpenRouter and
  `--base-url`) are proxied transparently.
- **Anthropic** is proxied through its native `/v1/messages` protocol, with
  streaming usage merged from `message_start`/`message_delta`.
- **Managed free tier** is the only direct path: the engine's free models
  require client attestation, which a local proxy would break. Publicly these
  models are always shown as provider `free-tier`; the engine provider id never
  appears in output.
- Other providers are rejected with a clear error instead of silently bypassing
  the gateway.

The engine always runs in standalone mode so that the generated provider
configuration and credentials apply to the run.

## Configuration

Models, providers, prices and limits are **not** configurable: they are part of
the application. The config selects behavior only.

```yaml
model: qwen-coder-30b
fallbacks:
  - glm-5.3-flash
  - deepseek-flash
budget: 2.00

subagents:
  model: free-glm
  fallback_model: deepseek-flash

economy:
  mode: balanced

prompt:
  global_file: prompts/global.md
  agents:
    builder:
      prompt_file: prompts/builder.md
      workdir: /workspace

run:
  timeout: 30m
  poll_interval: 2s
  auto_approve: true
```

See [`warden.example.yaml`](warden.example.yaml) for the complete reference.

Provider credentials come from the environment. `open-warden providers` shows
the expected variables and which ones are configured.

## Built-in registry

The application ships with this provider set:

| Provider | Credential | Protocol |
| --- | --- | --- |
| `siliconflow` | `SILICONFLOW_API_KEY` | OpenAI-compatible, proxied |
| `deepseek` | `DEEPSEEK_API_KEY` | OpenAI-compatible, proxied |
| `openrouter` | `OPENROUTER_API_KEY` | OpenAI-compatible, proxied |
| `anthropic` | `ANTHROPIC_API_KEY` | Anthropic `/v1/messages`, proxied |
| `free-tier` | none | managed free models, direct |

`free-tier` is the public identity for the engine's managed free models
(`free-glm`, `free-deepseek`, `free-minimax`, `free-longcat`). Internally they
map to the engine's own provider; that name never appears in `models`,
`providers`, reports or errors.

Every model has application-defined context, price and rate limits. The
list is deliberately curated; use `open-warden models --all` to see everything
and `open-warden models --json` for machine consumption.

## Token economy

The **delegation policy is fixed** and always injected into the main prompt.
It cannot be viewed or changed by users: it defines when and how to delegate,
what never to delegate, and that the primary agent remains accountable.
Users can only choose the subagent model and fallback.

Economy mode adds prevention rules on top:

| Mode | Prompt rules | Tool output | Compaction | Output cap |
|---|---|---|---|---|
| `off` | none | engine default | engine default | none |
| `balanced` | selective reads, summaries, artifacts | 200 lines / 20 KB | keep 8k / buffer 12k | 32768 |
| `aggressive` | stricter | 100 lines / 10 KB | keep 4k / buffer 8k | 16384 |

The output cap is applied by the gateway to `max_tokens`,
`max_completion_tokens`, `max_output_tokens` or Anthropic `max_tokens`,
never raising a smaller value chosen by the caller. Requests that exceed the
model context are rejected before reaching the provider.

```bash
open-warden run --economy aggressive "Task"
open-warden run --no-economy "Task"
open-warden run --economy balanced --subagents-model qwen3-30b "Task"
```

`off` disables only warden's economy policy. The engine's own defaults and any
global engine config still apply.

## Proactive rate limiting

Limits are application-defined per model and stored as sliding windows
persisted in the state file, so they survive across `docker run` invocations
that share the `/data` volume. Windows are consumed **per request** by the
gateway; direct free-tier runs use a pre-flight estimate reconciled from
session usage:

| Window | Meaning |
| --- | --- |
| RPM | Requests in the last 60 seconds. |
| TPM | Estimated tokens in the last 60 seconds. |
| RPD | Requests in the last 24 hours. |
| TPD | Estimated tokens in the last 24 hours. |

Before each dispatch the warden estimates the request weight
(`prompt_chars / estimate_divisor + output_headroom`) and checks the windows.
If the primary model has no room, the **next model in the fallback chain is
selected immediately** — no 429, no wait. When every model is blocked:

- `wait_when_all_blocked: true` sleeps until the nearest window opens;
- `false` fails fast with a `rate_limit` error.

Actual token usage is reconciled into the windows after the run, so estimates
never drift for long. Subagent tokens are counted in the report and in the
budget; TPM windows track dispatches made by warden, while subagent model
fallback uses its own proactive admission check (`subagents.fallback_model`).

## Fallbacks

The chain is `model` → agent fallback → CLI fallback → `fallbacks`.

- **Proactive** (before the request): rate-limit windows.
- **Reactive** (after the request): `rate_limit`, `quota`
  (balance/credits), `unavailable`, timeouts.

Every decision is recorded in the report with from/to, reason, scope
(`main` or `subagents`) and timestamp.

Subagent fallback is enabled with `fallback.subagents: true`: whenever the
main model moves to a fallback, the subagent policy is regenerated with
`subagents.fallback_model` before the next run.

## Cost control

`budget` is enforced **per run**. While the agent streams, a poller reads the
run's session tree every `run.poll_interval` and sums cost, including
subagents; at the ceiling the run is interrupted and reported as
`budget_exceeded` (exit code 3).

Cost comes from provider-reported pricing when available. The built-in price
table acts as a lower bound, so the ceiling also works for free tiers that
report zero cost. For a custom `--base-url` whose model is not in the registry,
there is no price information and the budget cannot be computed.

## Prompts

Prompts are composed in this order:

1. `prompt.global` / `prompt.global_file` — applied to every agent;
2. `prompt.agents.<name>.prompt` / `prompt_file` — per agent;
3. the task prompt (CLI/stdin).

`open-warden agents` runs every agent in `prompt.agents` concurrently, capped by
`max_parallel`. All agents share one limiter, so the combined traffic respects
the configured windows.

## Reports

Every run ends with a report like:

```
=== warden report | builder | ses_... ===
status: ok_with_fallback (budget $2.00, spent $0.041200)
tokens:
  main         in=15230 out=840 reasoning=1200 cache_read=40960 cache_write=0 requests=6 cost=$0.031000
  subagents    in=4210 out=210 reasoning=90 cache_read=10240 cache_write=0 requests=2 cost=$0.010200
  total        in=19440 out=1050 reasoning=1290 cache_read=51200 cache_write=0 requests=8 cost=$0.041200
by model:
  siliconflow/zai-org/GLM-5.3-Flash  in=15230 out=840 ... cost=$0.031000
  deepseek/deepseek-flash            in=4210  out=210 ... cost=$0.010200
fallback events:
  [main] siliconflow/... → deepseek/deepseek-flash (12:03:11) reason=rate_limit
duration: 84.2s
```

Subagent consumption is collected by walking the session tree, so delegated
work is never invisible. History is persisted in the state file and available
via `open-warden report` (or `report --json`).

`--report-file <path>` writes the same report as JSON while keeping normal text
streaming on stdout. This is the integration path for external orchestration
(claim/run/log/usage scripts): the caller gets `id`, `status`, `model`,
`economy_mode`, `subagents_model`, `fallback_events`, `budget` and the full
`usage` tree without parsing logs.

## Integrating with an orchestrator

Any scheduler that used to spawn a raw agent CLI can shell out to warden and
consume machine-readable results:

```bash
open-warden run \
  -c /workspace/warden.yaml \
  --agent coder \
  --workdir /workspace/.worktrees/coder \
  --budget 2.00 \
  --json \
  --prompt-file /tmp/task.md
```

The JSON document contains `status`, `model`, `fallback_events`, `usage`
(main/subagents/total and per model) and the final `output` — enough to feed an
external ledger without parsing logs. Exit code `3` means the budget ceiling
was reached, `1` means failure, `0` success.

This one-process-per-item model maps directly onto watcher-style
orchestrators: unique sessions, isolated workdirs, proactive limits shared
through the state volume, and no engine details to scrape.

## Credentials

- Keys are read from the environment. `warden.yaml` never contains key values.
- `--api-key`, `--api-key-file` and `WARDEN_API_KEY` override the credential
  for a single run; they are never written to disk.
- `open-warden providers` shows which variables are configured without
  revealing them.
- `<ENV>_FILE` is supported for Docker/Kubernetes secrets (for example
  `SILICONFLOW_API_KEY_FILE=/run/secrets/siliconflow`).
- Logs, reports and JSON output redact key material; the gateway binds to
  `127.0.0.1` and requires a per-process token.

## Security notes

- The container runs with the mounted workspace as its working directory and
  auto-approves tool execution by default (`run.auto_approve`). Mount only the
  repository the agent should touch.
- Do not mount the Docker socket or your home directory into the container.
- Pass provider keys as environment variables, never in the config file.

## Publishing

```bash
IMAGE=esdrasschonevald/open-warden make push VERSION=0.1.0
```

Requires `docker login` (or `DOCKERHUB_USERNAME`/`DOCKERHUB_TOKEN`).

## License

MIT
