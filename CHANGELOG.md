# Changelog

What core ships today, by area — the honest inventory after ADR-022 (2026-09-05/07) contracted core to
the CLI-only inference runtime. For the per-release diff see
[Releases](https://github.com/opod-io/opod-core/releases). For what moved to the control plane and why, see
the last section.

## Unreleased

- **The leader's own node row has its real address (T24.105).** `opod up` wrote `"127.0.0.1" + listen`, so a `listen`
  with a host read `127.0.0.1127.0.0.1:8080` in `opod node ls`; it is now the listen host (127.0.0.1 for an empty or
  wildcard one) and its port.

- **`opod node drain` and `undrain` on a managed leader's host go through the leader (T24.102).** Their "already
  draining" check read the store before the call, and on a managed host that read stopped at the new store guard; it now
  reads the running leader's row first.

- **`opod doctor` on a managed leader's host reads it right (T24.103).** The admin key row takes the leader's admin
  token (`OPOD_ADMIN_TOKEN` or `auth.admin_token`), and the engine row is a note on a managed leader, which runs no engine
  itself. With the pinned certificate (T24.102) the leader and port rows find the running leader. On such a host doctor
  used to report three false problems: no leader answering, the leader's own port "in use by another program" and no
  admin key.

- **The CLI on a managed leader's host answers from the running leader (T24.102).** On a host whose leader runs with
  `OPOD_MANAGED=1` and no key file, `opod node ls` printed "no nodes registered yet" beside a serving worker and
  `node show`, `model ls` and `token ls` read an empty store of their own, while `shard ls` and `model ps` said "no admin
  key". Now an admin call also uses the admin token the leader was started with (`OPOD_ADMIN_TOKEN` or
  `auth.admin_token`, after `OPOD_TOKEN`); over TLS the CLI accepts exactly the certificate in the leader's `tls_cert`
  file, whatever names it carries, and any other certificate is an answer, never "no leader answering"; `node ls` and
  `node show` ask the running leader first, as `drain` and `remove` do; and a command that would open the store on a
  managed host with no `OPOD_STORAGE_DSN` refuses and says what reaches the leader instead.

- **`opod fetch` names the Hub, shows progress and says when a file is cached.** A Hub that does not answer names the
  address and whether it came from `HF_ENDPOINT`; a long download logs its progress every 30 s (a new
  `fetch.Options.Progress`); a cached file says `already cached`; the logs on stderr are one human line per event
  (`--log-format json` for structured lines); a help screen with examples; a failed result-file write names the file and
  where the weights are. The path stays the only stdout line, and the Hub refusal reasons (`hub-token-missing`, …) are
  unchanged.

- **`opod cache *`: a missing directory is said, empty says empty, one unit.** `kv-prune` on a directory that does not
  exist warns (a mistyped `--dir` read as an empty tier and guarded nothing); with `--every` it prints a start line,
  waits for the directory and says so every 10 minutes, and shows an idle pass once a minute. `ls` says when the cache is
  empty; sizes are labelled GiB; a failed sweep of crashed pulls is a warning and the dry run says it does not sweep;
  kv-prune names the missing flag; extra arguments are refused. JSON stays one object per line.

- **`opod image` and `opod catalog`: the same answer in both views, a line on success.** A `--vendor` or `--engine`
  no image is built for is a usage error in the table and under `--json` alike (JSON answered `[]`, exit 0); a filter with
  no match exits 1 in both; `--engine` lists only engines with an image; `image show` takes part of a name; `catalog
  export` says what it wrote; `catalog ls --json`; extra arguments are refused.

- **`opod token *`: an unknown flag never becomes a key, and the key is alone on stdout.** `token create --admn` minted a
  user key named `--admn`; an unknown flag is now a usage error with a did-you-mean. The banner goes to stderr so
  `key=$(opod token create …)` captures the key alone; a past expiry is refused; a `--node` key prints the `opod join` line;
  under `OPOD_MANAGED=1` with no store DSN `create` refuses (the key would be lost). `ls` has a STATE column
  (active/expired/revoked) and `--json`; `revoke` names the key and says "already revoked"; `renew` of a revoked key refuses;
  an unknown id points at `opod token ls`.

- **`opod node *`: a list for scripts, a hint on not-found, "already" on a repeat.** `ls --json`; `show` prints a short
  human summary and `--json` the record; `no node X (see opod node ls)`; a repeat drain or an undrain of a node that is
  not drained says nothing changed, exit 0; extra arguments are usage errors.

- **`opod shard *` names the gang, warns on an interrupt and checks flags first.** `create` ends with the gang, its
  nodes and its head, read from the leader's rows (it read `model_id` from the answer); Ctrl-C prints the cleanup command
  for the partial gang it may leave, and exits 130; unknown `--nodes` ids, a flag with no value (a bare `--gang` became the
  count), `--node` (now the same as `--nodes`) and unknown flags are refused up front. `ls` gets `--json`, prints `-` for a
  missing field and refuses a body that is not a shard list; `remove` with nothing there says so and exits 0.

- **`opod model *` names the model, the node and the next step.** `add` takes a catalog YAML as `--file` (`--from` is
  `move`'s source node); `add --node` says what Ctrl-C does, prints progress every 10 s and ends with the next command;
  `load` and `unload` accept ids the leader lists; `ps --json` and `move` refuse an answer that is not the leader's JSON,
  and `move` one with no finished step (a proxy's 200 printed "now served by"); `search` says when nothing matches and
  refuses a bad `--since` or `--sort`; `info` snippets use the real base URL; `remove` says when the engine kept the weights.

- **`connect`, `disconnect`, `config`, `completion` and `batch` say what they need and what they did.** A value flag with
  no value is a usage error (it took the next word as its value); `connect` warns when the snippet carries the admin key;
  `disconnect` takes flags after the client and its list ends with its own verb; `config path|edit|show` say when the file
  does not exist or why the local config is shown; `doctor` flags an engine on the leader's port; completion offers
  `cache kv-prune` and no longer creates `state.db`; `batch run` says how many lines failed, logs a resume, and its ready
  check tells no server from a refused key.

- **`opod update` never downgrades by accident.** Versions are compared as semver, not text (a `0.21.0-dev` build took
  `v0.4.5` as an update and installed it); an older target needs `--force`; a development build says so; `--check` exits
  10 when an update exists; replacing the binary asks first or takes `--yes`; a GitHub refusal is its message, and a rate
  limit says when to retry. A build without release flags reads `0.0.0-dev`.

- **`opod doctor` finishes the report and sets an exit code.** Every check is a row with its fix, the report ends with
  `all checks passed` or `N problem(s) found` (exit 1; it always exited 0), an unknown engine is a failed row instead of
  the end of the report, and it checks the config file, a leader on this host, the admin key and whether the port is
  this host's own leader; install hints follow the OS; `--json`.

- **`opod join` says when it registered, and a stop it chose fails.** `registered with leader … as …` once it is in, and
  one line per kind of failure (`refused the join token — mint one: opod token create --node`, `cannot reach the leader …`)
  beside the JSON log, through a new `agent.Agent.OnRegister` hook; 6 refused re-registers or a taken worker port end with
  `opod: worker stopped: …` and exit 1 (it printed `worker shutdown complete`, exit 0). Flags may come before the URL and
  take `--gpu=1`; `node.yaml` no longer records the join token.

- **`opod down` stops only opod and says so; `opod status` tells healthy from answering.** `down` signals the PID file's
  process only when it is opod (or answers as this host's leader) — it stopped a `sleep` that inherited the number —
  confirms the stop or says the process still runs after 10 s, and a repeat with nothing running exits 0. `status`
  reports healthy only on a 2xx (a 401 or 503 printed ✔), checks the engine either way, reads models and nodes from the
  leader's admin API instead of opening this host's store, and its JSON names the leader `leader` (was `control_plane`).

- **Admin verbs say what failed and what to do, and reach any leader.** `shard *`, `model ps|load|unload|move`,
  `model add --node`, `node drain|undrain|remove` and `config show` call `OPOD_BASE_URL` with `OPOD_TOKEN` when they are
  set, else this host's leader with `~/.opod/admin.key`. A refusal is decoded from the leader's JSON
  (`the leader at … refused the admin key from … — set OPOD_TOKEN …`, `… the leader answered HTTP 500: an empty body`)
  instead of the raw body pasted after a status line; a dead address names where it came from and the fix; a client
  budget that runs out says the leader may still be working. The offline paths (writing the store, driving the engine)
  run only when this host's own leader does not answer: a leader answering 500 with no body read as "no leader", and
  `node remove` then wrote the store under it. The CLI output rules are in CONTRIBUTING.md, pinned by golden
  transcripts (`cmd/opod/testdata/cli`).
- **Warnings go to stderr, and colour follows each stream.** `warn` writes to stderr only (44 warnings in `status`,
  `doctor`, `up`, `join`, `model add|rm|unload` and `down` were on stdout), and the `▶ ✔ ⚠` markers and the `opod:` prefix
  are coloured only when their own stream is a terminal and `NO_COLOR` / `OPOD_NO_COLOR` are unset — they kept their
  escape codes in a pipe.
- **One usage error, exit 2.** A bad flag, a missing argument, an unknown command or subcommand is three lines on
  stderr — `opod: <what is wrong>`, the usage line, `opod <command> --help for every flag` — and exit 2, never Go's
  `Usage of …` and a flag dump; `cache`, `fetch`, `catalog` and `batch` say `opod:` instead of `error:`; an unknown command
  is one line and a did-you-mean (it was ~60 lines of usage); `opod help <command>` shows that command's help; `model`,
  `shard`, `node`, `token` and `join` list their flags; `status` and `doctor` refuse an unknown argument.
- **No terminal, no dead end.** With no terminal to pick or confirm on, `model add|info|remove|load|unload` and
  `connect` say what was wrong and what to type (`no catalog entry "X" (try: opod model search X)`) instead of `no model
  selected`, and `model|node|shard remove` say `run … --yes` instead of `aborted`; `model load` checks its flags first.
- **`opod up` says ready only once the port is its own.** The PID file and the ready block with the one-time admin key
  come after the bind (a new `leader.Server.OnListen` hook); a second leader from the same data directory, a taken port, and
  `--role gateway` without `--leader` or `--role door` without `--routes` are refused before anything starts; a door whose
  routes file is missing warns. `opod up` returns through its cleanup instead of exiting past it.

## v0.4.5 — 2026-11-09

Speculative decoding: a worker renders the plan's n-gram (or draft) setting in its engine's own flags on vLLM, SGLang and
llama.cpp, and `/loadz` reports how many proposed tokens the model kept. Proven on the design-partner cell on all three
engines (2.9–3.5× tokens/s at 1 stream on a copy-heavy load).

- **Speculative decoding, rendered per engine (T29.19, ADR-120, feature `spec_decode`).** A worker's engine flags take
  `spec_method` (`ngram` or `draft`), `spec_tokens` (1–16, default 4) and `spec_draft` (the draft model). The launch writes
  each engine's own spelling: vLLM one `--speculative-config` JSON (method `ngram` or `draft_model`), SGLang
  `--speculative-algorithm NGRAM` or `STANDALONE`, llama.cpp `--spec-type ngram-simple` or `draft-simple`. A draft method
  with no draft renders nothing, and an `extra` that carries the engine's own speculative flag wins. vLLM's draft and
  accepted token counters reach the load sample (`spec_draft_tokens_total`, `spec_accepted_tokens_total`), and `/loadz`
  sums them: accepted ÷ draft is the acceptance rate. llama.cpp's counters of the same names are read too, and SGLang,
  which publishes a rate and no counters, reports `spec_accept_pct` (its `spec_accept_rate`, averaged on `/loadz`).
  llama.cpp's n-gram lookup length is set with the draft length (`--spec-ngram-simple-size-n`, 3 or fewer): with its
  default of 12 a draft of 4 proposed nothing.

## v0.4.4 — 2026-11-08

What the control plane needs to show how routing decides: `/loadz` states the sample each worker is ranked on and what
conversations' pins did, typed in `pkg/adminapi`. Also a usage-count fix and a removed dead knob.

- **`/loadz` lists the sample the picker ranks each worker on (T28.1, feature `load_pull`).** The `load_samples` map
  `/gatewayz` already carried — per worker its `source` (`pull` or `heartbeat`), `age_ms` and whether it is still
  `ranked` — is now on `/loadz` too, so a probe or an operator reads it on the leader and on every door. Omitted before
  any pull answers. It is typed in `pkg/adminapi` (`Load.LoadSamples`, `LoadSample`), so the control plane reads it;
  a heartbeat source is `ranked` only while its sample is inside the 30 s rule.

- **`/loadz` counts the requests that name a conversation and what their pins did (T28.3, feature
  `session_routing`).** A `sessions` block — `requests_1m` (requests with `X-Opod-Session` or a `user` field),
  `pin_hits_1m` (landed on the pinned worker), `pin_moves_1m` (the pinned worker was saturated, full or gone, so the pin
  moved) and `route_tokens_1m` (echoed route tokens that verified) — typed as `adminapi.SessionLoad`. It is absent while
  `routing.session` is off and all zeros when it is on and no caller sends a session. Each process counts its own.

- **The router's unused fallback cap is gone (T9.51).** `SetMaxFallbackAttempts` had no caller, so
  `opod_router_fallback_total{op="chain",reason="cap-exhausted"}` could never be emitted; the setter, the cap and that
  label value are removed. A fallback chain is bounded by its candidate list, as before.

- **A usage record the queue dropped while its batch was in flight is no longer counted as lost (T32.63).** Under
  load the queue's cap could drop the oldest records while an export batch held copies of them; the batch then landed,
  and the records were both delivered and counted in `opod_usage_dropped_total`, so the control plane's `missing`
  figure was too high by the batch. Such a drop is now counted only if its batch fails.

## v0.4.3 — 2026-11-07

Everything since v0.4.2 the same day: ready means answered (a warm-up generation), every router pulls its workers' load,
a conversation keeps its worker (ADR-109), and embeddings on an engine without the route are refused by name.

- **Embeddings on an engine with no embeddings route are refused by name.** Every OpenAI-compatible driver inherited
  `Embed` from the shared client, so a TensorRT-LLM endpoint (whose `trtllm-serve` has no `/v1/embeddings`) answered
  `502` with the engine's `404`. A driver now embeds only when its engine does (vLLM, SGLang, llama.cpp, MLX), and the
  leader answers `501 embeddings_not_supported`, as it does for rerank.

- **A worker reports its models only after its engine has answered one generation (T32.60).** The model list
  answers as soon as an engine's server is up, and TensorRT-LLM listed its model about 70 s before its first
  generation returned: the leader routed the next request to it and that request waited 72 s. The worker now sends a
  one-token warm-up request the first time its engine lists a model; until it returns, the heartbeat reports nothing
  loaded and `/readyz` answers `503 engine warming up`. Any answer from the engine counts (an embeddings-only model
  refuses a chat); no answer is tried again on the next heartbeat. A sleeping engine is not warmed.

- **Every router pulls its workers' load, a front door included (T28.1, ADR-109, feature `load_pull`).** The picker
  ranked a worker on its engine's queue and KV from the heartbeat, up to 5 s old, and a front door got no sample at
  all. The leader and every door now `GET /v1/load` from each worker every `routing.loadRefreshMs` (500 ms by default,
  floor 100 ms; a new field in the policy snapshot); the worker scrapes its engine at most once per interval whoever
  asks. A pulled sample older than three intervals is not ranked on, and requests sent after a sample count toward
  the worker's score. A worker on an older core (404) keeps its heartbeat's sample. `/gatewayz` lists each worker's
  sample and its age under `load_samples`. A gang ranks on its head node's pulled sample, and what was sent to the gang after it.

- **A conversation keeps its worker (T28.3, ADR-109, feature `session_routing`).** A request names its conversation with
  `X-Opod-Session` (or the OpenAI `user` field) and the pin is the conversation's, so two conversations under one key
  each keep their own worker; a request with neither pins per key, as before. With the policy's
  `routing.session.signedRoute`, a 200 answer carries `X-Opod-Session-Route`, a token signed with the key every door
  holds; echoed, any door routes the next turn to that worker, and a token that does not verify is ignored. With
  `routing.session.turnPriority`, a later turn of an active conversation leaves the hold first inside its flow, capped
  (`min(turns, 4) + seconds waited / 2`). A pin never lands a request on a saturated worker or one with no free slot.

## v0.4.2 — 2026-11-07

Fixes to v0.4.1's gang readiness, found on hardware the same day. No contract change.

- **An SGLang gang across machines keeps serving after its first 30 s.** Once the group forms, every SGLang rank but 0
  binds a dummy health server on the engine port. The rank's worker then got an error for its model list and sent no
  engine report, and past the bound the leader took that node, and with it the whole gang, out of rotation: every
  request got `503` "1 part(s) whose engine stopped answering". A node that runs a gang part, and not its coordinator,
  serves nothing of its own, so its missing report now reads as "nothing loaded". Its part is still judged by its
  process state. Seen on a leader built from v0.4.1; v0.4.0 has the same rule and the same worker images.
- **`/v1/models` lists a gang's model only once a coordinator is ready.** With v0.4.1's `starting` coordinator, the list
  still showed the model for the minutes its head loaded, and every request in that time got `503`.
- **A gang's part gets no placement row for the gang's model.** The worker that runs an SGLang gang's rank 0 finds the
  head on its own engine port and reports the model as loaded, so the leader stored a placement for it. With v0.4.1's
  `starting` coordinator, that row was a second way in: in the seconds between the head answering and the coordinator
  row turning `ready`, the router sent a request to the worker (it got `502`) and `/readyz` read 200. The leader now
  skips a reported model that the same node runs a gang part of. The gang's coordinator is the only route to it.

## v0.4.1 — 2026-11-07

Everything since v0.4.0 (2026-11-04). Breaking for a Go importer: the module path is now
`github.com/opod-io/opod-core`, and the wire types are core's own `pkg/` packages (opod-sdk is retired). The binary,
its commands, its routes and the images are unchanged in name.

- **An SGLang or vLLM gang reads `ready` only when its API answers (T32.58).** These gangs start their head without
  waiting for it, because its API comes up only after every part has joined and the weights are loaded. Their
  coordinator row was stored `ready` at launch. So `/admin/v1/shards` and `/readyz` said "serving" while every request
  got 503 — for about 45 s (vLLM, pipeline 2) to 2 min 20 s (SGLang, tensor 2) on a 1.5B model across two machines. The
  coordinator row of an SGLang, vLLM-over-Ray or vLLM expert gang now reads `starting`. Every 5 s the leader asks each
  `starting` coordinator `GET /v1/models`, and stores `ready` when it answers. A gang create still returns as soon as
  every part is launched. llama.cpp gangs are unchanged: their create already waits for the coordinator.
- **llama.cpp's fixed overhead on an AMD Radeon RX 7900 XTX is measured: 365 MiB (T23.18).** `adminapi.CardOverheadMiB`
  gave it the engine's largest figure, 570 MiB. Measured with qwen2.5-0.5b Q4_K_M: 402 MiB beyond weights and KV at 1 slot
  of 8,192 tokens, 430 at 60 slots of 32,768, less the compute buffer and mask the manager adds. Another AMD card keeps 570.
- **The module path is `github.com/opod-io/opod-core`, the repository's own name.** It was `github.com/opod-io/opod`,
  a repository that does not exist, so nothing could fetch core as a Go module. Every import changed with it; the
  `opod` binary, its commands and the images are unchanged.
- **The wire types moved in from opod-sdk (T9.50).** `adminapi`, `nodeapi` and `catalog` are now `pkg/` in this
  repo (`github.com/opod-io/opod-core/pkg/...`), copied from the SDK's final commit `c82d906`; core no longer requires
  `github.com/opod-io/opod-sdk`. A catalog entry is a PR here (`pkg/catalog/<id>.yaml`). `pkg/` is what the leader
  and the worker put on the wire, not a supported public client API; a test holds it to the standard library.
- **A worker states its engine's version.** The heartbeat's `engine` state carries `version`, read once from the
  running engine: vLLM's `/version`, SGLang's `/server_info`, llama-server's `/props` `build_info` (for example
  `b11028-972d2313b`). `GET /admin/v1/nodes` shows it on each node's `engine`.
- **`/loadz` `prefix_hit_pct` is weighted by traffic on vLLM.** It was the plain mean of the workers' rates, and a
  worker that had served nothing counted as 0 %: two workers, all traffic on one at 98 %, read 49 %. A vLLM worker now
  sends its prefix-cache counters (`prefix_hit_tokens_total`, `prefix_query_tokens_total`) and the leader divides
  their sums; `/loadz` carries the two sums too. An engine that publishes only a rate (SGLang, TensorRT-LLM) keeps
  the mean. Needs SDK v0.6.1.
- **An answer's usage carries the engine's cached tokens.** `usage.prompt_tokens_details.cached_tokens` is in the
  chat answer, streamed or not, when the engine reported a prefix-cache count — as OpenAI's answer does. It reached
  the usage record only; the answer had the three counts.
- **Every gang backend stops what a previous leader left.** After a leader restart, a gang's processes still run
  on its parts. Only llama.cpp's create swept them, so a vLLM Ray, SGLang or expert gang refused to re-form with
  `process "s-…" already exists (running)` until its parts restarted. The sweep now runs in the shared create.
- **A vLLM Ray gang takes its plan's engine flags.** The head's `vllm serve` was built from the model, the
  parallelism and the memory fraction only, so a gang's `max_model_len`, `kv_cache_dtype`, `pp_layer_partition`
  and `extra` — and a KV tier's size, which rides `extra` — never reached the engine. They now do, by the rule a
  worker's launch uses.
- **A key with no user id gets a sticky pin.** The pin was keyed by the key's user id only, and every key a mounted
  auth snapshot creates has none, so on a managed leader stickiness never fired, whatever `routing.stickyTtlSeconds`
  said. Such a key now pins as itself (`key:<id>`); a key with a user id pins by it, as before.

## v0.4.0 — 2026-11-04

Everything since v0.3.0 (2026-10-31). Breaking: usage is pushed (feature `usage_push`) and the leader keeps no
usage table, so a manager that reads `usage_stream` must move to the push; requires opod-sdk v0.5.3.

- **Routing by held blocks recovers after a leader restart.** A worker drained its block changes before posting them, so
  a failed post, or a 404 from a restarted leader, lost that batch; the shared block every chain starts with (the system
  prompt) was never stored again, and no worker was credited for anything until it was. After a heartbeat that did not
  land, the next one now restates every block held. The router also logs through the leader's logger (its level and
  JSON), with one debug line per pick naming each candidate's credited blocks.
- **A block in a worker's CPU tier can count for routing** (`routing.prefixBlockTier: cpu-lower`, feature
  `kv_cpu_tier_hits`). A vLLM worker with KV events and a CPU tier now asks vLLM to describe its CPU-tier blocks, reports
  them on its heartbeat, and the leader indexes them; under `cpu-lower` a leading block held there beyond the card's is
  credited at 0.8 of a card block (a CPU-tier hit measured 0.94–0.96 of a card hit's saving). Default `gpu`: as before.
- **The policy sets the sticky TTL live** (`routing.stickyTtlSeconds`, feature `sticky_ttl_policy`). A manager could not
  reach `OPOD_STICKY_SESSION_TTL_SECONDS` on a running leader. Pins placed after the policy lands expire by it, pins
  already placed keep their expiry, and 0 gives back the start value. The TTL is now read atomically, since a policy
  writes it while requests read it.
- **`OPOD_BIND_ADDR`: a worker behind NAT can listen on its own address.** The worker listened on, and its gang
  processes (llama.cpp `rpc-server`, the `llama-server` coordinator) bound, the address it advertises — which behind NAT
  or a port forward is not the host's, so the bind failed. `OPOD_BIND_ADDR=host[:port]` names what the host owns; the
  worker states it on registration (`BindHost` in its capabilities) and the leader binds gang processes there while it
  keeps dialling the advertised address. Unset = as before. `0.0.0.0` is taken only when stated, with a warning.
- **Leftovers removed or wired.** The `opod_callback_*` metrics, which nothing has emitted since callbacks left core,
  are gone. The config view names the response cache's fields in snake case and drops its empty `egress` object. A
  default model is recorded under the engine that serves it, not always `ollama:`. The SGLang NVIDIA and AMD images
  carry `gang: sglang` (SGLang's own launcher), so `opod image recommend` offers them for a model SGLang must split.
- **CLI parsing and help fixes.** `opod token create --node NAME` keeps `NAME` (it became `node-join`). `--tp`, `--pp`
  and `--priority` refuse a value that is not a number (it read as `0`). The admin calls, `status` and the ready banner
  reach the leader at its own `listen` host and over `https` when it serves TLS (they built `http://localhost<listen>`).
  `doctor` reads a free `host:port` as free. A `node drain/undrain/remove` on a host with no `admin.key` no longer writes
  the store while a leader answers. `--help` names `cache kv-prune`; `--auto-pull`'s help states its real default; the
  doctor's catalog hint and the config view's edit hint name the right places.
- **The auth snapshot updates a key it already holds, and refuses an expiry it cannot read.** A known `k_cp_` id kept
  the hash, scope and expiry it was first written with, so a rotation or a moved expiry under the same id did nothing.
  It now follows the file and counts as `updated`. An `expiresAt` that is not RFC 3339 used to mean "never expires";
  the entry is now refused, logged and counted (`refused` in `auth.updated`).
- **`opod up` prints the admin key only on the start that creates it.** Every later start printed it again from
  `admin.key`, so a leader under systemd or Kubernetes wrote its admin key into its log on every restart. A later start
  now prints where the key is saved.
- **`opod update` and `install.sh` never install a binary they could not verify.** With no `checksums.txt` for the
  release, both used to warn and install anyway; `install.sh` did the same with no `sha256sum` or `shasum` on the
  machine. Each now stops with what to do, and the installed binary stays as it was.
- **`opod config show` no longer prints the admin and join tokens when no leader answers.** Its fallback said
  "secrets redacted" and then printed the whole config, both tokens in clear. It now prints the same redacted view the
  leader serves at `/admin/v1/config` (moved to `internal/config`, `config.ViewOf`), and that view now replaces a
  database DSN's password (`postgres://user:redacted@…`, `password=redacted`), which both used to print in clear. **`opod node show`** prints the
  worker token redacted (`sk-orc…` and its last four), not whole.
- **A vLLM worker with a CPU tier reports its prefix-cache blocks again.** vLLM's offloading connector publishes its own
  `BlockStored` and `BlockRemoved` events for the host tier, with no token ids and no block size. One of them failed
  the whole event batch, so the worker reported no blocks, the leader's `prefix_index` read 0 and `prefixBlockWeight`
  scored nothing. An event for any tier other than the GPU is now skipped: the index stays what each worker holds on
  its card. The decode error now names the value it rejected, not only its type.
- **A revoked worker certificate is refused even when the worker connected before the leader had a node CA.** Such a
  worker's TLS handshake was asked for no certificate, and its 5 s heartbeat kept that connection alive, so every call
  stayed a token call and a later revocation changed nothing. The leader now records, per TLS connection, the node-mTLS
  CA and mode it was handshaked under; a register or heartbeat on a connection from an older one is answered with
  `Connection: close`, so the worker re-dials and presents its certificate. Under `nodeMtls: require` that one call is
  refused as having no certificate. Cost: one reconnect per worker per CA or mode change.
- **The vLLM workers run on vLLM's own images, at v0.33.0.** `opod-worker-vllm-nvidia` is built on
  `vllm/vllm-openai:v0.33.0` (was v0.27.1), `opod-worker-vllm-amd` on `vllm/vllm-openai-rocm:v0.33.0` (was AMD's
  `rocm/vllm` 0.23 RDNA build, which vLLM's docs mark deprecated) and `opod-worker-vllm-intel` on
  `vllm/vllm-openai-xpu:v0.33.0` (was `intel/vllm` 0.21). The AMD image is now one build for Instinct (gfx90a, gfx942,
  gfx950) and Radeon RX 7000 / RX 9000 (gfx1100, gfx1101, gfx1150, gfx1151, gfx1200, gfx1201) and is labelled
  `amd:cdna,amd:rdna3,amd:rdna4`; **it no longer covers RDNA2 (RX 6000)**, and the node's amdgpu driver must run ROCm
  10.0 userspace. Removed with the old bases: the build-time backport of vLLM's pinned host-to-device staging
  (the base has it) and the UCX + RIXL compile (the ROCm base ships `nixl_rocm`, which vLLM 0.33 imports). Served on
  one RTX 3060 Ti and one RX 7900 XTX; the Intel image has not served on its new base.
- **CI-built worker images carry their `io.opod.gpu.families` label.** Only the local lane (`images/build.sh`)
  stamped it, so every image the CI lane published — the ones a release and the control-plane chart pin — had none,
  and a control plane could not refuse a card the image has no kernels for at plan time. The workflow now reads the
  families through `images/build.sh families <image>`, from the same `images/images.yaml` declaration.
- **The entrypoint no longer reads `OPOD_BASE_ENV_FILE`.** It sourced a base image's environment script (Intel's
  oneAPI `setvars.sh`) for the old `intel/vllm` base; no image records one now. `images/build.sh refresh-bases` works
  again: it read a `${VAR:-ref}` default as `-ref`, and without `crane` it took the whole manifest list for a digest.
- **Usage is pushed, and the leader keeps no usage table (feature `usage_push`, replaces `usage_stream`).** Each finished
  request becomes one OTLP log record sent to `OPOD_USAGE_OTLP_ENDPOINT` (headers in `OPOD_USAGE_OTLP_HEADERS`); a
  record leaves the leader's memory only on the receiver's 2xx. While the receiver is away records wait in a queue
  bounded by `OPOD_USAGE_QUEUE_MB` (default 64 MiB, about 440 000 records), and past it the oldest are dropped and
  counted (`opod_usage_dropped_total`, `opod_usage_queue_bytes`, `opod_usage_queue_records`, `opod_usage_sent_total`,
  `opod_usage_export_errors_total`). A front door's records join the same queue. **Breaking:** `GET
  /admin/v1/usage/stream` and the SQLite `usage` table are gone (dropped on the first open); a manager that pulled the
  stream reads the push instead, and probes `usage_push`. Models are evicted least-recently-used by the requests since
  the process started.
- **A usage record says what the request did, what the cache saved and how it was ranked.** Each record carries
  `gen_ai.operation.name` (`chat`, `embeddings` or `rerank`; how it was spoken stays `opod.protocol`),
  `gen_ai.usage.cache_read.input_tokens` (prompt tokens the engine served from its prefix cache, as vLLM, SGLang and
  llama-server report them; 0 from an engine that reports none) and `opod.request.class` (the admission class that ranked
  it; absent when the policy has no classes). A front door's rows carry the same three.
- **vLLM and SGLang report the prompt tokens their prefix cache served.** A worker starts vLLM with
  `--enable-prompt-tokens-details` and SGLang with `--enable-cache-report`; without them both answer with no
  `prompt_tokens_details`, so the cached count read 0 on every request. A plan's `extra` that already names the flag is
  not doubled.
- **The installer looks for any engine, not only Ollama, and its next steps print whole.** It finds `llama-server`,
  `vllm`, `trtllm-serve`, `mlx_lm.server` or `ollama` on PATH, and with none it names llama.cpp (Mac), vLLM (Linux with
  `nvidia-smi`) or a llama.cpp build (other Linux), linking docs/ENGINES.md. Its "Joining an existing cluster?" line
  nested double quotes inside a double-quoted string, so the shell read `<leader-host>` as a redirect and printed
  `sh: leader-host: No such file or directory`. The next steps no longer say "Make sure Ollama is running" or point at a
  web UI core does not have.
- **A front door keeps its workers while a restarted leader rebuilds its registry (feature `registry_rebuilding`).** A
  managed leader keeps the worker registry in memory, so after each restart it lists no workers until they heartbeat
  again. A door read that list as final, dropped every worker and answered 503 until its next sync (16 of 60 requests in
  one test). After a boot with an empty registry, `GET /admin/v1/nodes` now carries `X-Opod-Registry: rebuilding` until
  three worker heartbeats (15 s) after the first worker registers again (at most 2 minutes after boot); a door that
  reads it adds and updates workers, placements and gang parts, and removes none. A leader without the feature never
  sends the header, and a door then behaves as before.

## v0.3.0 — 2026-10-31

Everything since v0.2.1 (2026-10-15). Breaking: the pre-request guardrail webhook left the leader (ADR-113) —
checking prompts is the gateway's job in front of it.

- **A first `opod up` with llama.cpp serves instead of timing out.** `opod up` with `OPOD_ENGINE=llamacpp` launches
  `llama-server -hf <repo>`, which downloads the GGUF before it listens. The launch set no readiness bound, so the
  supervisor's 30 s default expired during the first download of a 2 GB model and the leader came up with no engine
  (measured on an Apple M3, 2026-10-31). The probe now asks `/health` and has the caller's whole budget — 30 minutes
  on a first `opod up`, like a cold `model move` target, and 15 minutes for the watchdog's restart. The same first
  run then served in about 3 minutes, tool calls and `/v1/messages` included.
- **Docs lead with llama.cpp and vLLM, and every engine has install steps** (`docs/ENGINES.md`): what each engine is
  for, how a worker launches it, what has served per engine × vendor, the container images, the engine flags.
- Requires `opod-sdk v0.5.1`.

- **An SGLang worker image for Intel: `opod-worker-sglang-intel`** on upstream's `lmsysorg/sglang:v0.5.22-xpu`
  (`images/build.sh sglang-intel`). Upstream targets Arc B-series (Xe2); an Arc A770 served too with SGLang's
  default attention backend, so the image declares both families. One card; no gang has run on the XPU backend.
- **A worker says whether its leader accepts it: `GET /readyz` on the worker (feature `worker_readyz`).** 200 only
  when the leader accepted the worker's last register or heartbeat and its engine answers; 503 with the reason
  otherwise; `?join=only` for a gang part, whose engine the leader starts later. `/healthz` stays the engine alone.
  A worker whose leader refused it used to read healthy, so a manager counted it as capacity.
- **A re-created worker with a certificate rejoins its leader.** The auth snapshot that turns `nodeMtls: allow` on can
  reach a new leader after its first workers have joined by token, which bound each node id to that token's key. A
  worker pod re-created after that presented its certificate, the leader compared "no key" with the bound key, and
  refused every register and heartbeat ("bound to a different key") until the pod gave up. One rule now decides who
  may act as a node: a certificate acts as the node it names whatever bound it, and a node a certificate registered is
  bound to that certificate, so no node token acts as it while certificates are checked — before this such a node had
  no binding at all. With mTLS back to `off` the node's token takes it back.
- **An Intel (SYCL) llama.cpp gang forms.** The SYCL backend's reorder optimisation attaches backend data to every
  Q4_0/Q8_0/Q3_K–Q6_K tensor, and ggml's RPC server refuses such a tensor, so the coordinator aborted loading with
  "Remote RPC server crashed or returned malformed response" and no Intel gang of a k-quant GGUF ever formed. The
  Intel image's `rpc-server` now starts with `GGML_SYCL_ENABLE_OPT=0` (an operator's own value wins); llama-server's
  local backend keeps the optimisation. Proven on two Arc A770 nodes.
- **The leader's own calls join the trace too (T32.42).** A model load or unload, a worker's sleep or resume, the
  pre-route tokenize and the external fallback forward now carry the request's W3C `traceparent`, like the chat
  dispatch does (`engines.TraceTransport`). A door already passed the caller's header through untouched; a test now
  holds that. A gang's `router.*.attempt` span names the worker that heads the gang in `opod.node_id`, not the
  pseudo node `shard:<model>:<gang>`; the usage row still counts the request under that key.
- **`opod fetch` reports what it fetched, and whether it is signed (T32.23).** With `OPOD_FETCH_RESULT` set, the fetch
  writes one JSON line there once the weights are in the cache (`adminapi.FetchResult`): the path, and whether the
  repository publishes a model-transparency signature beside the weights (`model.sig`, OpenSSF model signing) —
  `unsigned`, `unverified` or `unknown` when the Hub could not be asked. The signature is looked for and **not
  verified**: there is no `verified` state yet, and verifying a model signature on the node is still to be built.
- **Per-key limits leave core again (ADR-112): feature `key_limits` is gone.** A key in a manager's auth snapshot no
  longer carries `rpmLimit`, `tpmLimit`, `quotaDailyTokens` or `allowedModels`; the leader and its doors enforce none
  of them, `/v1/models` is no longer filtered per key, the refusals `rate_limit_rpm`, `rate_limit_tpm`, `quota_daily`
  and `model_not_allowed` are gone, and the spend snapshot carries only the door count and the lag bound. Limits,
  quotas and model access per key, user or team are the job of the gateway the owner runs in front of the endpoint
  (LiteLLM, for example). Request classes still shed load.
- **Removed: the pre-request guardrail webhook.** The leader no longer reads `guardrails` from the policy snapshot,
  calls no webhook before a chat, embeddings, rerank or Messages request or before a fallback forward, answers no
  `403 guardrail_blocked`, writes no `guardrail.block|flag` event or audit row, and exports no
  `opod_guardrail_action_total`. The `internal/guardrails` package is gone. Checking a prompt for PII or injection is
  the job of the gateway in front of the leader (LiteLLM's guardrail hooks, for one). A snapshot that still carries the
  key is read and the key ignored. Post-response guardrails were never built.
- **A trace goes on into the engine (T32.7).** Every engine call — leader → worker and worker → engine, for vLLM,
  SGLang, llama.cpp, MLX, TensorRT-LLM and ollama — carries the W3C `traceparent` of the request it serves, set once in
  the drivers' shared transport (`engines.TraceTransport`); a worker reads the leader's header on the way in. Header
  only: no client span and nothing per chunk. An engine that exports its own spans (vLLM `--otlp-traces-endpoint`)
  now lands in the caller's trace instead of one of its own.
- **Trace sampling (feature `trace_sample_ratio`, `OPOD_TRACE_SAMPLE_RATIO`, T32.8).** `0`–`1`, the share of the traces
  the leader starts that it records; unset = `1`, so nothing changes. Parent-based: a request whose caller sampled it
  (LiteLLM in front) is always recorded, one whose caller did not is never. Any other value fails the start and names
  the variable.
- **Attempt and driver spans say what served the request (T32.6).** `router.*.attempt` spans carry `opod.gang.id` when
  the target is a gang's head and `opod.class` (the admission class that ranked the request); `<driver>.Chat` spans
  carry `opod.tokens.cached` when the engine reports a prefix-cache count, which a worker now passes on in its usage
  (`prompt_tokens_details.cached_tokens`). Serving facts only — never a key, user or team — and computed only while the
  span records.
- **A non-streamed answer reaches the client before its usage row is written.** Chat, embeddings, rerank and Messages
  answers that were not streamed used to sit in the server's buffer until the handler returned, so the caller waited
  for the usage row's SQLite insert (up to its 5 s busy timeout) — and embeddings and rerank wrote the row, and the
  embedding cache, before writing the answer at all. Each now hands its answer over first; the row and the cache are
  written after.
- **A stopping worker drains (feature `worker_drain`, `OPOD_DRAIN_SECONDS`).** On SIGTERM it says goodbye at once, so
  no new request reaches it, lets the requests already running on it finish, and stops its engine when nothing is in
  flight or the bound passes, whichever is first. A gang member waits while a peer still holds a connection to it (the
  leader dials a gang's head directly, and the head holds its parts), so parking a whole gang does not cut a stream
  either; a connection from the pod's own address (a head on its own rpc-server) is not a peer. `0` stops at once;
  unset = 10 s, which was the old fixed wait for requests on the worker's own API. A manager
  gives the pod a longer grace period than the bound. The image entrypoint now sends the stop to `opod join` only, and
  stops an engine it launched itself (`OPOD_ENGINE_START`) after the drain instead of with it.
- **`GET /admin/v1/nodes` says which worker's engine is down.** Each row carries `engine` — the worker's last
  statement on its engine process (`state`: serving · starting · restarting · crash-looping · stopped, with `restarts`,
  `since` and `detail`) while it is fresh, and nothing when it is stale or was never sent. `/loadz` already counted the
  unhealthy ones (`engines_unhealthy`); a manager can now act on the one worker and the card it holds. Additive: a
  reader that ignores the key sees the same row.
- **The leader conducts a prefill/decode KV handoff for vLLM (feature `pd_handoff`).** A chat picked onto a decode
  worker is first prefilled on a prefill worker of the same model (one non-streamed call, `max_tokens: 1`,
  `kv_transfer_params: {do_remote_decode: true}`), and the decode call carries the `kv_transfer_params` that engine
  answered with, so the decode engine pulls the prompt's KV instead of computing it. A request is served whole
  instead, never failed, when no prefill worker takes requests, the prefill step fails, or a repetition penalty is in
  force or cannot be read: vLLM's decode answers differently after a remote prefill whenever one is on
  (vllm-project/vllm#63693). The worker reads the served model's default from its snapshot's
  `generation_config.json` (`GET /v1/sampling-defaults`). A decode step that vLLM ends with no token and no finish
  reason is a failed KV load, served whole, and that pair rests for a minute. Outcomes: `opod_pd_handoff_total`.
- **Callers can no longer set `kv_transfer_params` or `bootstrap_host/port/room`** — reserved keys like `cache_salt`, so
  a caller cannot steer a decode engine's KV pull.
- **A TensorRT-LLM worker takes the KV cache's element type from a plan** (`flags.kv_cache_dtype`, e.g. `fp8`), as
  vLLM and SGLang do. `trtllm-serve` has no command-line flag for it, so it is merged into the one options file the
  worker writes, as `kv_cache_config.dtype` inside any `kv_cache_config` block already there; a file that chooses a
  dtype keeps it. Whether the engine has an attention kernel for that mix on a given card is the engine's: TensorRT-LLM
  1.2.1 has none for a 16-bit model with an fp8 KV on sm_120.
- **A vLLM worker can keep a disk KV tier below its host tier (feature `kv_tier_fs`).** The engine flag
  `kv_tier_fs=<absolute dir>`, beside a host tier (`--kv-offloading-size N` in `extra`), adds one
  `--kv-transfer-config` JSON to the launch — vLLM's `OffloadingConnector` with `TieringOffloadingSpec`, the host tier
  as its CPU primary and an `fs` secondary tier in that directory — and exports `PYTHONHASHSEED` (0 unless the
  environment sets one) so every process sharing the directory names one prefix with one file. It is the host tier's
  connector, never a second one: without a host tier, with another backend, or beside a `--kv-transfer-config` of the
  plan's own, the flag is ignored. The engine never deletes from the directory, so **`opod cache kv-prune --dir <dir>
  --max-gb N [--every 10s] --apply`** bounds it: oldest blocks first down to 90 % of the cap, crashed writes after
  10 minutes, only the tier's own files (`*.bin`, `*.tmp`, `config.json`); `--max-gb 0` empties it but for the
  `--keep` entries.
- **`opod cache kv-prune --every` follows a burst instead of a clock.** A pass that trimmed the tier, or found it larger
  than the previous pass left it, is followed after 1 s; `--every` is now the idle period. With a fixed 10 s period a
  burst of ≈120 MB/s took a 4 GiB tier to twice its cap before the next pass; the next pass now comes within a second
  of writes, so the overshoot is about one second of writes plus the pass. Each pass reports how long it walked and
  removed (`walkMs`, `removeMs`, and in the text line), so a pass slower than the period shows in the log.
- **Admission queues at the KV bound when it is the smaller (feature `admission_kv_bound`).** An engine's slots say how
  many requests it runs at once, not how many its KV pool holds (vLLM starts 256 whatever the pool). When the plan's
  `workload` states the endpoint's context and an engine states its pool, that worker or gang counts
  `min(slots, pool ÷ context)`, in the gate and in the picker; `/loadz` `admission_bound` says `slots` or `kv`.
- **The engine's own time to first token (feature `engine_ttft`).** A vLLM or SGLang worker reads its engine's
  `time_to_first_token_seconds` histogram and reports the last minute's median as `engine_ttft_p50_ms` (SDK v0.4.8);
  `/loadz` carries the workers' median, so `ttft_p50_ms` minus it is what the leader and the network add. llama.cpp
  publishes no such histogram and states none.
- **Callers no longer share a prefix cache across teams (feature `cache_salt`).** The policy's `routing.cacheIsolation`
  — `team` (the default: a team's keys share, teams never do; a key with no team is its own team), `key` or `none` —
  says whose requests may reuse each other's cached prefix. The leader sends `cache_salt` to vLLM, SGLang and
  TensorRT-LLM: an HMAC of the caller's team or key id under the endpoint's admin token, never the id, so every front
  of an endpoint salts a caller alike. The prefix pins carry the same salt. `cache_salt` is now a reserved body key: a
  caller's own value is dropped. llama.cpp takes no salt and shares its cache whatever the mode; a request with no key
  carries none. The prefix-cache block index is not salted yet (the engines' cache events carry no salt). **A leader
  whose own engine is a hosted vendor's API** (`OPOD_ENGINE=vllm` pointed at a vendor) must set
  `routing.cacheIsolation: none` in its policy file: the vendor refuses a `cache_salt` field it does not know.
- **A llama.cpp worker that holds a GPU it cannot reach refuses its load instead of serving from the CPU.** llama.cpp
  treats a backend that fails to start as a warning ("no usable GPU found") and loads the model into host RAM. Before
  a whole `llama-server` starts on an accelerated worker, the worker now runs `llama-server --list-devices`; no device
  is a failed load carrying llama.cpp's own error, so the worker reports no model. A plan that pins `ngl` is not
  checked, and a build whose answer cannot be read launches as before.
- **A budgeted vLLM worker takes off the overhead measured for its vendor (feature `engine_overhead`).** The fraction a
  VRAM budget becomes now subtracts `OPOD_ENGINE_OVERHEAD_MIB`, which a manager sets from the SDK's per-vendor table
  (vLLM: 837 MiB on NVIDIA, 1,103 MiB on AMD); unset, the largest measured figure applies. It was 768 MiB for every
  card, and an AMD worker held 279 MiB more than its budget. SGLang keeps its 768 MiB until it is measured.
- **`/loadz` sums the engines' host KV tiers (feature `kv_tier_load`):** hits and queries in tokens, bytes loaded and
  stored, allocation failures, and the fullest tier's fill, from each counted worker's heartbeat; absent while no
  engine keeps a tier.
- **`/loadz` says what a host KV tier would have served (feature `tier_ghost`).** The leader keeps a ghost list of the
  blocks each worker's engine evicted (from its cache events); a block the engine stores again was computed again, and
  is counted at the depth of the evictions in between. `tier_ghost` carries the blocks computed and those returns by
  depth, so a manager can size a tier and state its hit rate from real traffic. Hashes only, never content.
- **A rerank answer must score every document.** An engine that routes `/v1/rerank` for a model that does not rerank
  (seen on a vendor vLLM build serving a chat model) answered 200 with no scores, and the gateway relayed it as an
  answer. A 200 with no scores is now `rerank_not_supported` naming the documents; a partial answer is an engine error.
- **`/admin/v1/nodes` carries a gang coordinator's engine sample on its row.** The coordinator reports through the
  leader's gang driver, not a heartbeat, so a manager reading the rows saw no KV pool for any gang while `/loadz`
  summed it. Its row now carries the same cached sample; an rpc part's row stays without one.
- **A vLLM worker reports its host-memory KV tier.** From the same `/metrics` scrape as its other load signals, the
  worker's heartbeat now carries the tier's hits and queries in tokens, the bytes loaded from and stored into it, its
  fill and its refused stores (`tier_*` in `nodeapi.EngineLoad`, SDK v0.4.5) — each only when the engine publishes it,
  so a worker with no tier states none. `/admin/v1/nodes` passes them through per worker.
- **The AMD vLLM worker no longer answers garbage for Qwen3.** On the vLLM 0.23 RDNA base, Model Runner V2 copied each
  step's `query_start_loc` to the GPU with a non_blocking copy from pageable memory, and on ROCm that copy intermittently
  did not land: Qwen3 1.7B and 8B answered nonsense after the first request (0 of 12 correct on an RX 7900 XTX). The
  worker image now backports upstream vLLM's pinned staging of that copy (vllm-project/vllm#56888; for the RDNA
  release, ROCm/vllm#1403) at build time, and changes nothing on a vLLM that already has it: 12 of 12 correct.
- **TensorRT-LLM serves tools and structured output, and a worker with no tool-call parser says so.** A TensorRT-LLM
  worker answered a `tools` request with the call as plain text and a `response_format` request in prose, both with a
  200. Its launch now adds the catalog's `--tool_parser` for the model (as vLLM and SGLang get theirs) and always names
  one options file that sets `guided_decoding_backend: xgrammar`, merged with any `--config` the plan's `extra` names,
  and the driver sends a `json_schema` format as the engine's own `{"type": "json", "schema": …}` — release 1.2.1
  reads OpenAI's `{name, schema}` wrapper as the schema itself and constrained nothing.
  A vLLM, SGLang or TensorRT-LLM worker started with no parser — the model's catalog entry names none and `extra`
  names none — now answers a request carrying `tools` with a 400 `unsupported_request` that names `tools` and the
  flag to set, instead of a success in the wrong shape.
- **A batch sends lines that share a prompt together.** `opod batch run` sent lines in file order, so a document asked
  many questions across a large batch could be evicted from the engine's prefix cache between them and computed again.
  Lines are now grouped by a hash of their model and the first 2 KiB of their messages or prompt (held in memory only),
  groups in the order they first appear; results, `sent.jsonl` and a resume stay keyed by line, so nothing reads
  differently. `--file-order` keeps file order.
- **A key is counted when keys are optional.** With `require_keys` off the leader never looked at a presented key, so
  every request to such an endpoint was recorded with no `api_key_id` — per-key showback read nothing — and the key's
  model allowlist and request class did not apply. A presented key that resolves (known, not revoked, not expired) is
  now that request's caller: its usage row carries the key, and the key's own settings apply. A request with no key,
  or one that does not resolve, is served as before; nothing is refused in this mode.
- **A gang reports its KV pool.** The leader samples a llama.cpp gang's coordinator for `/loadz`, but could not read its
  pool without the layout it was launched with; the coordinator's row now records it, so `kv_capacity_tokens` covers
  gangs. A slot count left automatic now reads as one shared pool whatever `kv_unified` says, as llama-server builds it.
- **A worker that stops heartbeating is forgotten.** Only an operator's `DELETE /admin/v1/nodes/{id}` ever removed a
  node, so every replaced worker pod left its row, its placements, its in-memory samples and three metric series
  (`opod_node_up`, `opod_model_loaded`, `opod_router_inflight`) on the leader for good. The leader now removes a
  worker 15 minutes after its last heartbeat, exactly as that DELETE does, and records `node.forgotten` (with
  `silent_for`) rather than the operator's `node.removed`; one that was only cut off re-registers on its next
  heartbeat, as it already did on a 404. A front door (`--role gateway`) drops a worker its leader no longer
  lists on its next fresh sync; a stale list still removes nothing.
- **The leader's audit table holds actions, not traffic.** Every `/admin/v1` call wrote a row — reads and the five-second
  worker heartbeat included — and nothing trimmed it, so a leader's audit table (in memory, for a managed leader) grew
  for as long as the leader lived. Reads and the heartbeat now write nothing, the same rule the `admin.call` event
  already followed, and the table is a ring of the newest 50 000 rows like the usage and event tables.
- **The leader image says what it speaks.** `opod version --features` prints the leader's feature keys —
  comma-separated, sorted, the same list `/admin/v1/capabilities` advertises — and both image lanes (`images/build.sh`
  and `images.yml`) stamp it on `opod-leader` as the OCI label `io.opod.leader.features`. A manager that pins a leader
  by digest can then tell from the image whether that leader has, for example, a TLS listener (`tls_listener`),
  instead of guessing from whether the digest matches the one it was configured with. A test holds the command's
  output equal to the advertised map.
- **An SGLang worker reports its load again.** SGLang serves `/metrics` only with `--enable-metrics`, and core never
  passed it, so every load sample failed with a 404: an SGLang worker or gang reported no KV use, queue depth,
  throughput or KV pool (T23.2) at all, and the router and autoscaler saw it as idle. Both the single-worker and the
  gang launch lines now pass the flag.
- **An SGLang pipeline that cannot serve is refused by name** (T8.23, `error.code` `sglang_pp_tied_llama`). On SGLang
  v0.5.21 a pipeline-parallel gang (`pp > 1`) of a Llama-family model with tied word embeddings (`LlamaForCausalLM` and
  the classes SGLang derives from it unchanged: Mistral, Phi3, InternLM3, IQuestCoder) crashes on its last stage —
  `PPMissingLayer` in `_compute_lm_head` — on every re-form. Before such a create claims its guard or touches a part, the
  leader reads the model's `config.json` (a local directory, a complete snapshot, else one bounded Hub GET with
  `HF_TOKEN`) and answers `409` with the code and the remedy: tensor parallelism, one stage, or vLLM. The heal loop's
  `shard.heal_failed` carries the same code. A tied Qwen2 pipeline still forms (it serves), and a config that cannot be
  read never blocks a create.
- **`OPOD_PLAIN_LISTEN`** (feature `plain_listen`, `plain_listen` in `config.yaml`): the whole API — the main
  listener's own handler, so `/v1`, `/admin/v1`, the join path and the probes with the same checks — on a second
  listener that is always plain HTTP, beside a main one that may speak TLS. For a gateway that load-balances several
  leaders and cannot originate TLS to each one's own certificate. The hop to it is **clear text**: bind it only where
  the network decides who may connect. An address that cannot be bound stops the leader from starting. Unset = off,
  and nothing changes.
- **The engines say what KV pool they built** (T23.2, feature `kv_readback`). A heartbeat's `load` carries
  `kv_capacity_tokens`, `kv_used_tokens`, `preemptions_total` and `evictions_total`, each read from the engine's own
  report and absent when it does not say: vLLM's `cache_config_info` (`num_gpu_blocks × block_size`) and
  `num_preemptions_total`; SGLang's `max_total_num_tokens`, `kv_used_tokens` and `num_retracted_requests_total`;
  llama.cpp's `/props` slot context, times the slots when each has its own partition (the worker tells the driver the
  layout it launched). `GET /admin/v1/nodes` gives each worker's sample as `load`; `/loadz` sums them.
  `engines.EngineLoad` is now the SDK's `nodeapi.EngineLoad` itself, so the two cannot drift.
- **`OPOD_CARD_MIB`** (T23.3, feature `card_mib`): the card's size from the manager. A budgeted vLLM or SGLang launch
  divides by it and asks `nvidia-smi` only when it is unset, so a budget now applies on AMD and Intel hosts, where it
  was silently ignored. One translation per engine, and the overhead numbers in one table (`memKnobs`).
  `agent.MemFractionShell` now takes the engine name instead of a default fraction.
- **A sleeper whose card was given away is not resumed by a request** (T11.31, feature `wake_no_resume`). The policy
  snapshot's `wake.noResumeInPlace` names such workers; a request that finds only them is held by the admission hold
  instead, and the change of marks is recorded as a `policy.wake_marks` event.
- **vLLM and SGLang start with the model's tool-call parser** (T19.25, feature `tool_parsers`): the catalog's
  `tool_parser` for the engine adds `--enable-auto-tool-choice --tool-call-parser <name>` (vLLM) or
  `--tool-call-parser <name>` (SGLang) to a worker's launch and to the vLLM Ray, vLLM expert-parallel and SGLang gangs,
  unless the plan's `extra` names a parser already. Before, a `tools` request came back as text.

## 2026-10-19 — Anthropic Messages on every chat model, and images reach the engine

- **`POST /v1/messages` and `/v1/messages/count_tokens` are served again** (ADR-092 §1), translated to the chat path so
  every engine serves them: `system`, text and base64 images, `tool_use` / `tool_result`, `tools`, `tool_choice`,
  `stop_sequences`, `max_tokens`, `temperature`, `top_p` and `top_k`. A stream is Anthropic's events, an error is
  Anthropic's envelope, and the usage row's protocol is `anthropic`. `count_tokens` is the engine's own prompt count,
  taken from a one-token generation. Extended thinking, server tools, documents, image URLs and unknown fields are
  refused 400 naming the field. Feature key `anthropic_messages`; both routes join the leader contract.
- **An image reaches the engine** (T16.2, ADR-092 §3). The gateway parsed `image_url` parts, but the shared OpenAI
  serialiser sent text only and the worker decoded content as a string, so under a worker no engine ever saw an image
  and the answer was about the text alone. Content parts now survive every hop, a data URL whole. An image for a
  catalog model without `vision` is refused 400 `unsupported_request` naming `image_url`, and so is an image sent to
  SGLang, llama.cpp (no `--mmproj` yet) or MLX until each is proven; vLLM and Ollama take them. Feature key `vision`.
- **A Vertex AI custom container** (T19.4, ADR-092 §1). `OPOD_VERTEX=1` adds `POST $AIP_PREDICT_ROUTE` (default
  `/v1/vertex:predict`: `{instances, parameters}` → `{predictions}`, each instance a chat request; a body without
  `instances` is rawPredict, a chat body passed through) and `GET $AIP_HEALTH_ROUTE` (default `/v1/vertex/health`).
  Keyless — Vertex's IAM authenticates the caller — and recorded as protocol `vertex`.
- **Vertex AI's gRPC `PredictionService` on the leader's port** (T19.5, ADR-092 §1). The listener now speaks HTTP/2
  beside HTTP/1.1 (h2 under TLS, h2c in the clear). `ChatCompletions` and `StreamRawPredict` (streams) and `RawPredict`
  carry OpenAI JSON in an `HttpBody` and run through the `/v1` chain — the call's `authorization` metadata is the key,
  usage protocol `grpc-vertex`. Other methods are `Unimplemented`. Feature key `grpc_vertex`.
- **A TensorRT-LLM driver** (`trtllm`, T19.15, ADR-092 §7). A worker launches `trtllm-serve <model>` on the PyTorch
  backend — safetensors from the Hub, no engine build — with `tp`, `max_num_seqs` (`--max_batch_size`, always set),
  `max_model_len` (`--max_seq_len`), `kv_cache_free_gpu_memory_fraction` and `extra`; the driver reads KV
  utilisation and the queue from `/prometheus/metrics`. One node only. A pinned revision is served from its prefetched
  snapshot and refused without one, because `trtllm-serve` takes no `--revision`. The worker's engine launches are one
  table now (vLLM, SGLang, TensorRT-LLM). NVIDIA's container licence forbids a derived copy, so there is no
  `opod-worker-trtllm-*` image: `images.yaml` lists `nvcr-trtllm-nvidia` as an **upstream** row — NVIDIA's own image,
  with opod copied in at start by the manager.
- **Prefix affinity matches a shared lead, not only an identical opening.** With `routing.prefixAffinity` the router
  keyed on one hash of the system prompt plus the first user turn, so the same system prompt with another question
  missed. It now hashes the prompt's leading 4 KiB in 256-byte links, each covering all before it, and pins to the
  worker that served the longest shared link — an approximate scorer for engines that publish no KV events. Reported
  blocks still win, the switch is still off by default, and the pin table is bounded. The pin also yields, as the
  sticky pin does, to a worker whose slots are all in flight, so one long shared system prompt no longer sends every
  request to one worker.
- **SGLang publishes its KV-cache events** (`kv_block_events`). With `OPOD_KV_EVENTS=1` an SGLang worker starts the
  engine with the same publisher config as vLLM and 16-token pages (unless its flags set `--page-size`), and the
  leader tokenizes for its block index through SGLang's `/tokenize`.
- **SGLang on CUDA is `v0.5.21`** (`lmsysorg/sglang:v0.5.21-cu130`, from `v0.5.2-cu126`). The base is CUDA 13, so the
  node needs NVIDIA driver 580 or newer. On `v0.5.2`, `tool_choice: required` killed the engine; on `v0.5.21` it answers
  a tool call.
- **An engine that dies during startup is reported as exited**, with its output, rather than as a port that did not
  open, when the death lands near the readiness deadline.

## 2026-10-18 — a gang whose part is down is formed again

- **The door takes `model: "auto"` — an owner's ordered list of endpoints, with fallback down it** (ADR-092 §8). A
  route that names `candidates` resolves to them by `pick` (`first`, the owner's order; or `least-loaded`, each
  candidate's `/loadz` in-flight plus queued requests per serving worker, read at most every 2 s). The door tries the
  next candidate, `maxHops` times (default 1), on a 429, any 5xx, a context-length refusal or no answer; any other
  answer is the client's. A request that carries `tools` skips a candidate whose model cannot call them. Every answer
  through the door names the endpoint that served in `X-Opod-Endpoint`, and that endpoint's leader records the usage.
  A name route behaves as before: no retry, no fallback.
- **llama.cpp's own context-size refusal (`exceed_context_size_error`) is a context-length error** to the router's
  typed fallback chains, as vLLM's and SGLang's already were.
- **SGLang takes the KV cache's element type from a plan** (`flags.kv_cache_dtype` → `--kv-cache-dtype`), as vLLM does.
- **A worker relays its engine's refusal of the request as the caller's error.** An engine answer that says the request
  itself is wrong — 400, 413, 422 — went back from the worker as 502, so the leader answered `502 upstream_error` to a
  prompt longer than the context. The worker now relays the engine's status and words (`engines.ClientStatus`), and the
  leader's existing passthrough answers 400 with the engine's own error type.
- **llama.cpp's KV layout is the manager's to say** (`flags.kv_unified`: `true` → `--kv-unified`, one pool every slot
  shares; `false` → `--no-kv-unified`, a partition per slot). A build picks one when not told, and the default changed
  between builds. The KV-pressure number stays slot occupancy, which is KV use only with a partition per slot.
- **A worker's stop reaches `opod join`, so it says goodbye.** The image's entrypoint ran `opod join` in the
  background and Kubernetes' SIGTERM went to the shell alone; the join process died with the SIGKILL and never sent
  its goodbye, so after every park and rolling update the leader kept routing to the stopped worker until the
  heartbeat bound (60 s) — each request a 503 after a dial timeout, and a floor-0 endpoint's wake hold never applied.
  The entrypoint now forwards TERM/INT to the worker's processes and waits for them.
- **A gang part whose process is down takes its gang with it.** When a worker's heartbeat newly reports its engine
  `stopped` or `crash-looping`, the leader checks that worker's process list; a gang whose recorded part is not running
  is removed (`shard.stale`, reason "the worker reports its part's process down"), so the heal forms it again and
  gives up after three. Before, a part that could not start again left the gang's rows `ready`: the router refused
  every request for the model and the heal, seeing a gang present, never tried.

## 2026-10-17 — per-key limits, from memory

- **A key from the auth snapshot may carry limits again** (feature `key_limits`). `rpmLimit` and `tpmLimit` are token
  buckets per key; each replica serving keyed traffic — the leader and every gateway — takes `limit ÷ fronts`; the spend snapshot
  carries `fronts` (the doors plus the leader) to the doors.
  `quotaDailyTokens` is a counter per key and UTC day: the leader counts its own answers and its doors' rows on
  top of a base taken once — the larger of its store and the manager's `spentToday` — so a replaced pod keeps the day; the spend snapshot carries the
  count (`day`, `key_tokens_today`) to the doors. `allowedModels` refuses any other model with 403 and filters
  `GET /v1/models`. Over a limit: 429 with Retry-After and `rate_limit_rpm`, `rate_limit_tpm` or `quota_daily`. Each
  refusal is a usage row. Nothing on the request path but memory — the version removed on 2026-09-28 ran a SUM per
  request. Keys made with `opod token create` carry no limits.

## 2026-10-17 — what an operation started, it stops when it fails

- **A part that fails to start is stopped by the start itself.** Every gang backend (llama.cpp RPC, vLLM over Ray,
  SGLang, expert parallel) rolled back the parts that had finished when a create failed, but not the one it was
  starting — a part that failed, never became ready, or whose wait was cancelled kept running and held its card until
  the next create swept it. The shared start call now stops the process whenever a start that may have reached the
  worker does not end ready; a start the worker refused started nothing and is left alone.
- **A stop kills the engine's whole process group.** When a process's context ends (a failed health probe, a stop),
  the kill now goes to its group, as the graceful stop's SIGTERM already did; the default reached only the direct
  child, and an engine's own workers (vLLM's Ray actors) could outlive it on the card.
- **A placement on several nodes that fails part-way unloads what it loaded** (`opod model add <id> --node a,b,c`):
  the nodes before the failing one no longer keep the model.
- **`opod cache prune --json` says how many crashed pulls it swept** (`swept`, only with `--apply`), so a manager can
  record the one cleanup nobody asked for by name.
- **The gang heal gives up after three failed tries of one shape.** The last `shard.heal_failed` carries
  `final: true` (every one now carries `attempt`), and the leader stops trying that gang until it appears, the plan
  changes its shape, or the manager parks it and later un-parks it. A gang the plan marks `parked` is not one the
  leader forms. Before, the heal retried every minute for ever, starting processes on the cards each time.

## 2026-10-16 — an SGLang part with a whole card sizes its own memory

- **A whole-card SGLang part is no longer told to take 85 % of the card.** With no VRAM budget the worker and the
  gang backend now leave `--mem-fraction-static` out, and SGLang reserves room for its activations and CUDA graph
  buffers by card size, as it does when run alone. The fixed 0.85 left an 8 GB card 0.6 GB for that room, and both
  ranks of a two-machine gang died on their first workspace allocation; SGLang's own choice there is about 0.62. On a
  large card the pool is now slightly smaller than before (SGLang's figure, about 0.8 on 48 GB). A part with a
  budget is unchanged: its fraction still comes from the budget. vLLM keeps its fixed default, because it has no
  card-sized rule of its own.

## 2026-10-15 — a pull request that slows the leader fails

- **CI measures what the leader costs on every pull request** (`leader-cost` workflow, `make perfgate`,
  `internal/perfgate`). One runner starts a real `opod up` and `opod join` in front of a stub engine with a fixed
  token cadence and measures the stub two ways, interleaved: called directly and through the leader and the worker.
  Only the difference is judged, so the runner's own speed cancels out. The gate fails when the leader adds more than
  10 ms to time-to-first-token p95, at one stream or at four. A third arm puts a leader that is 20 ms slower in front,
  and the run fails unless the gate fails that arm too — the gate proves on every run that it can see a regression.
  Throughput loss is reported beside it and not yet gated. In five runs on a GitHub `ubuntu-24.04` runner the leader
  added 1.3–2.9 ms at p95 and the slowed arm 21.8–23.3 ms, so the margin has room on both sides.

## 2026-10-15 — an engine that dies at startup keeps its last words

- **The exit a worker records always carries the engine's own last output.** The supervisor read an engine's stdout
  and stderr through pipes beside `exec.Cmd.Wait`, and `Wait` closes those pipes the moment the process exits — so
  an engine that printed its error and died at once (llama.cpp's `GGML_ASSERT`, a missing binary) could be recorded
  as having "produced no output", which is the unactionable failure the tail exists to prevent. Seen once in CI and
  reproduced at about 1 in 300 runs. The engine's output is now the command's own `Stdout`/`Stderr`, so `Wait`
  returns only after every byte is in the log buffer; a last line with no newline is kept, and a process the engine
  left behind holding its output open delays the exit by at most 2 s (`WaitDelay`).

## 2026-10-14 — a worker's join token stays off its command line; an engine between restarts says `restarting`

- **A worker whose engine exited and is about to be relaunched reports `restarting`, not `stopped`.** The supervisor
  marks the process `restart_pending` while it waits out the back-off (1 s, doubling on each consecutive exit), and
  the heartbeat sends the SDK's new `nodeapi.EngineRestarting` (`v0.4.2`). `stopped` means "will not come back
  without a change", so a leader told callers an engine had stopped while it was seconds from serving; the 503 now
  reads "whose engine is restarting", and `/loadz`'s `engines_unhealthy` counts the back-off (a card held, nothing
  served). The process record's `status` still says how it exited (`failed` / `stopped`), because a gang's start reads
  a part's `failed` as its definite answer. A leader older than this reads the new value as no statement.
- **A worker heartbeats the moment its engine serves again, not up to an interval later.** The leader learns a model is
  loaded only from a heartbeat, so after every engine relaunch the model was refused for up to `HeartbeatInterval`
  (5 s) after it could serve — on the design-partner cell, no request between two kills succeeded although each
  relaunch served within about a second. The supervisor now signals a process reaching `running`, starting a restart
  back-off or entering crashloop. A back-off or crashloop is sent at once; a process that is up again is not yet
  loaded (engines start with no readiness probe), so the agent asks the engine twice a second until it lists a model
  (for at most ten minutes) and sends its heartbeat then.
- **A request that finds no worker left by the time it is routed answers 503 with the leader's reason, not 502 "Start
  llama.cpp".** The gate checks capacity before routing; when the heartbeat dropped the last worker's placement in
  between, the router fell back to the leader's own engine — absent on a leader that serves through workers — and the
  caller got `engine_unreachable` naming `127.0.0.1:8089` with a hint to start llama-server (measured on the
  design-partner cell with both engines killed). The leader now asks its gate again and answers `503 unavailable` +
  `Retry-After` with that reason (chat, embeddings, rerank); a standalone engine's own fault keeps its hint.

- **`opod join` reads `OPOD_JOIN_TOKEN` when the URL carries no `?token=`, and the worker image no longer puts the
  token on the command line.** The image's entrypoint ran `opod join "<leader>?token=<token>"`, so anyone who could
  list the pod's processes — or the host's, with host process access — read the join token. It now runs `opod join
  "<leader>"` and the token stays in the environment, where the manager already put it. The URL form still works and
  wins when both are given; `opod join --help` says it is visible in a process list. A URL with no token and no
  `OPOD_JOIN_TOKEN` still joins on the worker's certificate (R9.6), as before.

## 2026-10-12 — what a dying engine left: stranded slots, wrong names, zero-token rows

- **A request in flight on a worker that left the pool no longer holds a slot of the workers that remain.** The
  admission gate counted in-flight requests per model, so when a worker's engine was sent SIGTERM its slots left
  the pool at once while its hung streams (30 s) stayed counted: on the design-partner cell 4 such streams kept every
  slot of an idle sibling and 6 queued requests waited 30–33 s. The gate now reads which worker or gang each granted
  request went to (the router's trace, installed before the gate) and does not count one whose worker is outside
  the pool; a worker that comes back with its requests still running counts them again. `/gatewayz` reports the
  number per model as `admission.models.<m>.stranded`.
- **A `worker_unreachable` answer names the worker whose error it quotes.** With every worker of a model down, the
  router returns the first worker's error while the request's trace names the last worker tried, so the headline
  named one worker and the bracketed error was another's. The router now tags the error with the worker (or gang)
  that gave it, and the message names that one (chat, embeddings, rerank); usage attribution is unchanged.
- **A model whose engines are only restarting no longer answers "no workers are awake … waking".** A worker whose
  engine process is down reports nothing loaded, which drops its placements, so the 503 fell through to the
  scale-from-zero text — "scale-up in progress or floor is 0" — while the floor was 2 and nothing scaled. The leader
  keeps the models each worker last reported, and a live worker that served the model and says its engine is
  `starting` or `crash-looping` is counted as "whose engine is restarting" (`stopped` — which a worker also reports
  while it waits out the back-off before a restart — as "whose engine has stopped").
- **A stream that breaks after tokens reached the caller is metered at what was delivered.** Its usage row wrote
  `completion_tokens 0` (measured on the design-partner cell: ~100 tokens delivered), so usage export and anything
  rating it downstream saw nothing. A streamed answer that ends without the engine's own usage — broken, errored or
  cancelled — now records the content chunks the caller received as `completion_tokens`: one token per chunk on
  llama.cpp and vLLM's default stream, an estimate where an engine batches tokens. `prompt_tokens` stays 0 there;
  only the engine counts it.
- **A successful request no longer takes the router's write lock.** Since the 5 s set-aside, every success cleared
  the worker's penalty-box entry under the write lock; it now checks under the read lock and writes only when there
  is an entry or a pending strike to clear.

## 2026-10-12 — a worker whose engine breaks mid-stream answers with an error, not an empty 200

- **A worker that gave a request nothing is set aside for new requests too.** After a worker's engine died, the
  request at hand went to a sibling, but the next one picked the dead worker again — it answered in milliseconds and
  so looked the least loaded — because the per-worker penalty box was only written by the opt-in breaker
  (`placement_allowed_fails` + `placement_cooldown_seconds`, off by default). A worker that could not be dialled,
  whose engine was down, or whose stream broke before its first event now goes into the box for one heartbeat
  interval (5 s) regardless; a worker that answers again leaves it at once. The box is a preference: when every
  otherwise-live worker of a model is in it they are still asked — before, the configured breaker parking the last
  worker sent the request to the leader's own engine, which does not hold the model.
- **A worker whose own engine is down is "nothing answered", and a sibling serves the request.** When a worker's
  engine process was dead or restarting, the worker's dial to it was refused and it answered a plain-text `502`;
  the leader read every non-2xx from a worker as the engine's own error, so no sibling was asked, and the caller
  was told to start the LEADER's llama-server. The worker now answers that case `502`
  `{"error":{"type":"engine_unreachable_on_worker",…}}` (chat and embeddings); the leader's client reads it back as
  `engines.ErrUnreachable`, so the router asks a sibling under the existing budget and, when none can take it, the
  caller gets `503` + `Retry-After` naming the worker's engine. Any non-2xx answer from a server that was reached
  (`engines.ErrUpstream` — a worker's, or the leader's own engine's) is now `upstream_error` without the
  leader-engine start hint, whatever its text says about a refused connection. Wire: additive both ways — an older leader still reads a
  `502`; an older worker's plain-text `502` is read as before (shown, not replayed).
- **A part lost while its start call is in flight names the process.** A gang create whose part's worker was
  replaced while the leader was still reading the start answer ended with the bare reason ("the worker behind node
  … was replaced") and no process; one lost while the create polled the process named it. Both now read the same:
  the process being started, on which node, and that no status had been polled yet.
- **A worker whose own engine fails mid-stream is an error, not an empty `200`.** A worker relays its engine's
  failure as an in-band chunk, `data: {"error":{…}}`, and closes cleanly; the leader's stream reader skipped the
  chunk and synthesized a normal end, so the caller got `200` with an empty or cut-short answer and the usage row
  said `ok`. The reader now ends the stream on that chunk as an error (vLLM's and llama-server's own in-band error
  events too). The worker marks its engine's transport break `"type":"stream_broken"`: before the first event a
  sibling serves the request (the T8.17 path, same budget; `503` + `Retry-After` when none can); after it the
  caller gets an SSE `error` event (a non-streamed request a `502`) naming the worker, and the usage row says
  `error`. Any other in-band error, or one from a worker older than the field, is shown and never replayed. A
  stream that fails after it started is `upstream_error`, no longer `engine_unreachable` with a hint to start the
  leader's own engine. Wire: additive — an older leader ignores the chunk as before.

## 2026-10-12 — gang creates and teardowns touch only their own incarnation

- **A failed gang create no longer cleans up after its guard is released.** `CreateShards` used to tear down the
  gang's CURRENT rows once the create had failed and let go of the gang's guard: a gang the heal loop had just
  formed again under the same id could go with them, and when a sweep had already removed the rows it logged
  `shards/create cleanup failed … has no gang` at ERROR. The create's own rollback — which already stopped the parts
  it started, under the guard — is now the whole cleanup, and it deletes the rows the create wrote as written
  (`ShardStore.DeleteRows`), never by id.
- **A late stop in a gang teardown reaches only the incarnation the teardown read.** The stops run one after
  another and one can take 30 s (a dial to a pod address that died with its worker); a part's process id is its
  gang's, so the same ids come back the moment the gang is formed again, and a stop still queued behind the slow
  one reached a sibling node at its current address and stopped the NEW gang's part. Right before each stop the
  node must still be the worker process it was when the stops began (same boot id and address) and the row must
  still be the one read; otherwise the stop is skipped with an INFO line saying why. Whole-model teardown and a
  failed create's rollback stop parts through the same path.
- **A part that gives up while its gang forms is a definite answer with its own `error.code`**: `gang_part_failed`
  (feature `shard_create_error_codes`, additive). A coordinator, rpc-server or rank whose process reports `failed`,
  or `crashloop` once its restarts run out, ends the create with the process, its status and the engine's own last
  lines, without the `upstream` prefix; the heal loop's `shard.heal_failed` carries the same code. Before, the 502
  had no code, so a manager could not tell it from a create still forming.
- **A stream that breaks before its first event is served by a sibling gang or worker.** A gang head being
  replaced answered `200` and then dropped the connection before any event; the stream ended `stream: unexpected
  EOF` and the caller got a `502` although a sibling gang was serving. The router now reads a stream's first event
  before it hands the stream on, so nothing has reached the caller yet, and a transport break at that point
  (`engines.ErrStreamBroken` → `engines.ErrBrokeBeforeFirstEvent`) re-picks the model with that worker or gang set
  aside — the path an unreachable worker already took, within the same budget. When every one breaks, the answer is
  `503` + `Retry-After` (`gang_unreachable` / `worker_unreachable`), not a fault of the leader's own engine. A stream
  that breaks after an event is never replayed. Response headers now go out with the first token rather than when
  the engine answers. A gang head is dialled directly, so its break is caught; a plain worker's own engine break
  is caught by the entry above.

## 2026-10-12 — a failing heal backs off and says why on the event stream

- **The heal-within-plan loop backs a gang off after a heal that failed** — 15 s, then 30 s, then 60 s at most;
  reset when a heal succeeds, the gang is present again, or the plan changes that gang's shape. The loop's create
  runs inside its ticker, so the next tick was already queued when a create failed and the next heal began a
  millisecond later: the gang's create guard was held almost without a gap, and a manager's own creates were all
  refused `gang_create_in_flight`. A manager's create now gets the guard between attempts.
- **A failed heal records `shard.heal_failed`** on `/admin/v1/events/stream` (feature `shard_heal_failed_event`,
  additive): subject the model, data `gang`, `parts`, `by: "plan"`, `revision` (the plan revision the heal read),
  `code` (the create's `error.code`, empty when its cause has none), `reason` and `retry_in_s`. Before, only a
  success (`shard.healed`) reached the stream and a failure was only in the leader's log.

## 2026-10-11 — a slow gang teardown no longer deletes the gang formed after it

- **A gang teardown removes the gang it read, never one formed again under the same id while it ran.** A teardown
  (a worker's new boot id, a stale part, `DELETE` of one gang) stopped the parts and then deleted the rows by gang.
  A stop can take 30 s when it dials a pod address that died with its worker, and in that time another teardown
  could remove the gang and a create form it again — new rows under the same ids — which the late delete then took
  with it, leaving the new gang running with no row to route it by. Rows are now deleted only while they are still
  the rows read (same id, node, address, process and creation time; `ShardStore.DeleteRows`), and a teardown that
  found its rows already gone no longer reports the gang removed.

## 2026-10-11 — a failed gang create says why in a code

- **A failed `POST /admin/v1/shards/create` names its cause in `error.code`** (feature `shard_create_error_codes`),
  beside the unchanged status (502), `type` and message. `gang_part_lost`: the gang did not form because a part's
  worker went away while it was forming; the create's parts were stopped and it will not finish on its own, and the
  message is now the reason itself (gang, node, last output polled) without the `upstream` prefix.
  `gang_create_in_flight`: refused because another create of this gang (or of the whole model) is running; nothing
  was touched. A manager can now tell a definite answer from a collision to retry without reading the English.

## 2026-10-11 — a gang create ends when a part's worker goes away

- **A gang create stops the moment the leader knows one of its parts' workers is gone**, instead of polling the dead
  process until the 15-minute formation budget runs out while holding the gang's create guard (so every heal attempt
  was refused with `a create is already forming this gang`). The signals are ones the leader already acts on: the
  worker registered or heartbeated with a new boot id (a recreated pod or restarted container), the stale-part check
  found a recorded part's process gone, or the node was removed. A failed status poll also reads the node's row
  again, so a new boot id ends the wait even without the notification; a poll error from the same worker process is
  still waited out as a blip. The create rolls back what it started, releases the guard, and returns an error that
  wraps `ErrPartLost` and names the gang, the node, what happened to it and the last status and engine output the
  leader polled for the process it was waiting on (or says plainly that the worker reported none). Rollback and
  worker stop calls now run detached from the create's cancellation, so a cancelled create still stops its parts.
  Not covered: a coordinator whose worker stays up and that neither opens its port nor exits still waits for its
  ready timeout.

## 2026-10-10 — a worker says which accelerators it was given

- **A worker states the devices its process was given** (feature `worker_devices`, `hardware_json.GivenDevices`,
  opod-sdk `v0.4.1`). Under a device plugin a worker asks for N devices and is handed N, and nothing upstream is
  told which. The worker reads it from inside its own sandbox — NVIDIA through `nvidia-smi` (index, UUID, PCI
  address), AMD and Intel from the DRM nodes in its `/dev/dri`, Tenstorrent from `/dev/tenstorrent/N`, each resolved
  to a PCI address through sysfs — and registers it with the rest of its capabilities. The device nodes scope the
  answer; sysfs, which lists every card on the host, only turns a node into an address. `GET /admin/v1/nodes`
  serves it back as stored, so a manager can say which card a whole-card worker holds instead of inferring it.
  An empty list is a worker given none; a worker that predates the field states nothing.

## 2026-10-07 — no 502 from a connection the engine closed while idle

- **The engine client retires an idle keep-alive connection after 2 s**, below the 5 s after which llama-server
  (cpp-httplib), vLLM and SGLang (uvicorn) close one. With Go's default 90 s, a request could take a pooled
  connection in the instant the engine closed it and fail with `server closed idle connection` — a 502 for an
  engine that was fine (about one request in a few hundred under a batch). The race is removed rather than
  retried: a request the engine may have received is never sent a second time.
- **A worker's error names the engine it runs.** The leader reaches every worker through the OpenAI-wire client, and
  a llama.cpp worker's failure read `vllm chat: 502 …`; it reads `llamacpp chat: 502 …` now (the engine the worker
  registered, or `worker` when it registered none). Span names for the leader→worker hop change the same way.

## 2026-10-07 — a busy endpoint ranks callers: admission is governed by worker slots

- **The leader dispatches to a worker only while it has a free slot** (feature `slot_admission`). Request classes
  used to act only while NO worker could serve; on a busy endpoint every request went straight into the engine's
  own first-come queue, so classes ranked nothing — with a batch at 8 in flight against a 4-slot llama.cpp worker,
  critical p95 time to first token went from 22 ms to 535 ms. Now each model has a pool of worker slots; a request
  that finds none free for its class waits in the leader, ranked, and every completion grants exactly one waiter,
  the highest class first, fair by flow inside it.
- **A worker states its slots on every heartbeat** (`slots`): llama.cpp's `/props` `total_slots`, SGLang's effective
  running-request limit (`/server_info`), or the number on the launch line — vLLM always starts with
  `--max-num-seqs` now: the plan's `max_num_seqs`, else **256**, so the worker and the engine agree on it. (vLLM picks
  1024 by itself on cards of 70 GiB and more; a plan that wants that sets `max_num_seqs: 1024`.) A gang's slots are
  its head's. A worker that states none is **ungoverned**: its model is admitted without a bound, the old rule, and
  `/gatewayz` `admission.ungoverned_workers` plus `opod_admission_ungoverned_workers` say so.
- **Each class may hold at most its share of the slots** (policy `admission.classes[].maxShare`, default critical 1,
  standard 1, **sheddable 0.25**; the cap is `max(1, floor(share × slots))`). A class at its share waits even while
  slots are free, so a critical request finds one at once instead of waiting for a whole batch answer. A quarter,
  not half: on a 4-slot llama.cpp worker, critical p95 time to first token beside a batch measured +114 % at 0.5 and
  −4 % at 0.25.
- **Time to first token includes the wait for a slot.** It is measured from the request's arrival at the leader,
  before the gate, in the usage row (`ttft_ms`, and `latency_ms` likewise), `opod_time_to_first_token_seconds` and
  `/loadz` `ttft_p50_ms` / `ttft_p95_ms`. Measured from after the gate, all three hid the queueing the gate moved
  out of the engine. The wait itself is `opod_admission_wait_seconds{class}`.
- **One path.** With no classes in the policy every request is `standard`, share 1, on the same gate; the old
  "hold only when nothing can serve" path is gone.
- **Behaviour changes on every endpoint, including ones with no policy.** `holdMs` 0 still answers `503` +
  `Retry-After` at once when no worker can serve; a request that finds every slot BUSY now waits in the leader with
  no deadline of its own — the queue the engine used to keep — and `maxHeld` (64 when unset) now bounds that queue
  too: past slots + 64 waiting, a request is shed with `503` + `Retry-After` instead of queueing in the engine.
  Raise `maxHeld` for a deeper queue.
- **The picker lands each request on a free slot:** a worker whose slots are all in flight ranks behind every worker
  with one free, and a sticky or prefix pin yields the same way.
- **Doors** (`--role gateway`) mirror each worker's slots from the leader's registry (`GET /admin/v1/nodes` now
  carries `slots` and the leader's own `in_flight` per worker) and count only their own dispatches; with N doors a
  worker can be sent up to N times its slots. `/gatewayz` states `slot_scope: per_door` and
  `worker_slots_overcommit_max`.
- **`/loadz` `queue_depth` includes the requests waiting in the leader** for a slot (and so does the vLLM-named
  `vllm:num_requests_waiting` alias): past a governed worker's slots the queue is the leader's now, not the engine's,
  and an autoscaler that scales on queue depth must keep seeing it.
- **Not governed:** `/v1/embeddings` and `/v1/rerank` (they never passed admission); a hedged request takes one pool
  slot but one engine slot per replica.
- **Metrics:** `opod_admission_in_flight{model,class}`, `opod_admission_slots{model}`,
  `opod_admission_ungoverned_workers{model}`, `opod_worker_slots{node}`, `opod_admission_slot_waits_total{class}`,
  `opod_admission_wait_seconds{class}`, and `opod_admission_shed_total` gains the reason `unavailable`.

## 2026-10-07 — expert-parallel gangs on vLLM's own launcher

- **`POST /admin/v1/shards/create` accepts `expert: true`** (feature `shard_expert`): a mixture-of-experts model's
  experts are spread over every device of the gang and attention runs data-parallel beside them, on vLLM's own
  launcher — no Ray. One `vllm serve` per part: the head runs `--data-parallel-size <parts × devices ÷ tp>
  --data-parallel-size-local <devices ÷ tp> --data-parallel-address <head> --data-parallel-rpc-port <port>
  --enable-expert-parallel` and serves the API; every other part runs the same line with `--headless
  --data-parallel-start-rank <part × local>`. The router dials the head only. One part holding every device is the
  in-machine shape, one process with nothing to dial.
- **Refused before anything is torn down:** a catalog entry whose `sharding.engine` is not `vllm`, `pp > 1`, and a
  `tp` that does not divide a part's devices (409, like any shape the fleet cannot hold).
- **The gang's engine flags reach every rank** (`max_model_len`, `gpu_memory_utilization`, `extra`, …), with the
  part's VRAM budget deciding the memory fraction as on every other start path.
- **Heal within the plan carries it:** a plan-declared gang with `parallelism.expert` is re-formed as an expert gang.
- A leader without the feature ignores the field, so a manager reads `shard_expert` before it asks.

## 2026-10-07 — a prefix a sibling holds counts as a peer hit

- **A worker states its KV cache tier** (feature `kv_peer_hits`, ADR-089): `OPOD_KV_TIER=cpu|peer` on `opod join`
  is registered as `hardware_json.KVTier`; any other value refuses to start. Unset = VRAM only, as before.
- **The prefix score has three kinds of hit: local > peer > none.** With `routing.prefixBlockWeight` on, a worker
  that states `peer` is credited, beyond its own leading blocks, for the further leading blocks the best OTHER
  worker that also states `peer` holds — at half a local block's weight, because those blocks still cross the
  network. So an idle worker that can fetch a prefix outranks a busy one that holds it, while at equal load the
  holder still wins. Saturation is judged first, as before.
- **No peer tier = today's score, block for block.** A worker can fetch only from a sibling that shares its tier,
  so a peer tier on one side changes nothing. The engine's peer connector is started by the manager's engine flags;
  core only scores.

## 2026-10-07 — `opod batch run`: a generic OpenAI batch client

- **`opod batch run --input <file|url> --base-url <url> --dir <work dir>`** reads an input in OpenAI's batch line
  format (`custom_id`, `method`, `url`, `body`), sends each line to any OpenAI-compatible server at bounded
  concurrency (`--concurrency`, default 8), and writes OpenAI's batch output shape: `output.jsonl` for the 2xx
  answers, `errors.jsonl` for the rest — a non-2xx answer keeps its status and body (a model the server does not
  serve is its `404`), a line that does not parse or a transport failure carries an error code.
- **The work directory is the progress record, and a line is never sent twice.** Each line is marked in
  `sent.jsonl` and fsynced BEFORE its request leaves; each result carries `batch_req_<line>`. A run started again on
  the same directory skips what is finished, and a line marked sent with no result — in flight when the previous run
  was killed — is written to the error file as `interrupted` rather than sent again: at-most-once, because a resend
  could run (and bill) an inference that already ran. A partial line left by a kill is cut on the next open.
- **A cancel is asked for; a SIGTERM alone is not one.** SIGINT, or a SIGTERM while the OpenAI batch object at
  `--status-url` (`GET …/v1/batches/<id>`, read once with the key) is no longer `validating`/`in_progress` — or no
  longer opens with the key — cancels at the next line: nothing new is sent, what is in flight gets `--drain` (20 s)
  and is then written as `cancelled`, and the run exits 0 with `state: cancelled`. Any other SIGTERM (an eviction, a
  node drain, a deleted pod, or no `--status-url`) is an INTERRUPTION: the same stop and drain, lines cut short are
  written as `interrupted`, and when lines remain unsent the run exits **75** with `state: interrupted`, so a Job
  restarts it and the run on the same directory continues, sending no line twice. Found on a cell: a deleted batch
  pod used to exit 0 as "cancelled" and its batch ended `completed` with 285 of 1000 lines never sent.
- `--model-map alias=id` rewrites a line's `model`; `--ca` trusts exactly one certificate for an https `--base-url`;
  `--ready-timeout` waits for `GET /v1/models` to accept the key before the first line. stdout carries
  `{"progress":{…}}` lines and a last `{"batch":{…}}` with the counts, the state and which result files were written.
- **Inputs and results through URLs that carry their own authorization.** `--input-url` reads the input from a
  presigned (signed) GET without sending the key — a store refuses a request with two credentials — and
  `--output-url` / `--error-url` PUT `output.jsonl` / `errors.jsonl` when the run ends, cancelled or not, with their
  length stated (a presigned PUT takes no chunked body); an empty file is not written. Each defaults to
  `$OPOD_BATCH_INPUT_URL`, `$OPOD_BATCH_OUTPUT_URL`, `$OPOD_BATCH_ERROR_URL`, so a URL that is a credential need not
  be on the command line, and a URL's signature is cut from any error the runner prints. A resumed run writes again
  and replaces what the interrupted one wrote.
- No store, no server and no control-plane concept: what runs it decides where the files go.

## 2026-10-07 — one URL for many endpoints: the door

- **`opod up --role door --routes <file>`** (feature `door`). A cell of five endpoints used to be five base URLs.
  A door is one: it reads each request's `model`, finds the endpoint in a routes file
  (`{revision, routes[{alias, upstream, model, ca}]}`, re-read every 10 s like the plan, auth and policy files; a
  file that does not parse or repeats an alias keeps the last good routes) and forwards the request there.
- **A name lookup, not a router.** The body and headers go through as sent — `Authorization` included — and a
  stream is flushed chunk by chunk. The door chooses no worker, judges no load, retries nothing and falls back to
  nothing; the endpoint's leader still picks the worker. The one change it makes: when a route's `model` differs from
  its alias, the request's `model` is rewritten to it, because a leader routes by its own model id.
- **No credential of its own.** The endpoint's leader checks the caller's key, so a key for endpoint A sent with B's
  model is refused by B. `GET /v1/models` asks every endpoint with the caller's key and lists the aliases whose
  endpoint answered 200, cached 10 s per key under the key's SHA-256 (never the key).
- **OpenAI's answers.** An unknown model, no model and `auto` are `404` with `code: model_not_found`; an endpoint
  that does not answer is `502 upstream_unavailable` for that model alone. Body caps are the leader's, per route.
- **An https endpoint is trusted through the route's `ca`** (PEM), exactly that certificate, as a worker trusts its
  leader. `/readyz` is ready once a routes file has been read; `/gatewayz` says `role: door`, the routes revision and
  its age. A door opens no store, starts no engine and mints no key.

## 2026-10-07 — a full endpoint can rank callers and share fairly between teams

- **Request classes** (`admission_classes`, policy `admission.classes[]{name, holdMs, maxHeld}`). An API key in the
  auth snapshot may carry a `class` — `critical`, `standard` or `sheddable`, `standard` when absent — and a `team`.
  When the policy lists classes, a request that finds no capacity is held in its class's queue: the turn always goes
  to the highest class waiting, and inside a class to the next flow by deficit round robin, a flow being the key's
  team or else the key. A flow sending a thousand requests and a flow sending one are served one for one.
- **Each class has its own budget and cap.** A class not listed takes `admission.holdMs` and no cap of its own;
  `admission.maxHeld` caps every class together. A request that finds the hold full takes the place of a request of
  a lower class, which is shed; with nothing lower, it is shed itself. Every shed is the same `503` + `Retry-After`.
- **`X-Opod-Class` may lower a request's class, never raise it.** A higher class in the header is ignored.
- **No classes in the policy = the first-come hold, unchanged.** Nothing is metered: the class ranks requests, it
  does not count them, and nothing on the request path reads the store for it.
- **Per door.** With `--role gateway` replicas each door ranks the requests it holds. `/gatewayz` says so under
  `admission`: `scope: per_door`, and `flow_share_skew_max` — across N doors a flow can be served up to N times as
  often as a flow of its class that reaches one door, and class order holds within a door only.
- **Metrics:** `opod_admission_held` now carries a `class` label (`standard` for every request when no classes are
  set, so the sum over classes is the old number), and `opod_admission_shed_total{class,reason}` counts sheds by
  `deadline`, `cap` and `evicted`. `opod_admission_held_total{outcome}` gains the outcome `shed_evicted`.

## 2026-10-06 — an interrupted pull resumes at once, not after six hours

- **`opod fetch` stops cleanly on Ctrl-C and SIGTERM.** It used to die mid-write and leave its download lock
  behind, so the next attempt — the one that should resume — sat behind that lock for `LockWait`, six hours.
  SIGTERM is what Kubernetes sends a prefetch Job's pod it deletes, so this was the ordinary case, not a rare one.
  Now the pull is cancelled, the lock is released and the partial file is kept.
- **A lock whose holder died is recognised at once, however it died.** A holder now also takes `flock(2)` on the
  lock file and marks the file `held-by-flock`; the kernel drops that flock when the process ends, including on
  SIGKILL, an OOM kill or a lost node. A waiter that can take the flock takes the lock over immediately, and
  `opod cache prune` does the same. A lock file without the marker was written by an older opod, which takes no
  flock, so only the age rule applies to it — a mixed fleet never mistakes an older binary's live pull for a dead
  one.
- Proven on a real node: a `kill -9` 4 s into a 3 GB pull left the lock behind; the next run took it over in under
  a second, resumed at 135 MB and finished with the Hub's digest.

## 2026-10-06 — `/loadz` reports time to first token

- **`ttft_p50_ms` and `ttft_p95_ms` on `/loadz` are now filled.** The leader measured time to first token on
  every streamed answer and wrote it only into the usage row, so a scaler with a latency budget had nothing to
  read. The figures are nearest-rank over the streamed answers of the last 60 seconds, held in a fixed ring of
  the last 4096 (memory does not grow with traffic). Zero still means *not measured*; a request carrying
  `X-Opod-Probe` is not counted. Feature key `loadz_ttft`.

## 2026-10-06 — a leader can sit behind an inference gateway that reads vLLM's metric names

- **`/metrics` now also carries `vllm:num_requests_waiting`, `vllm:num_requests_running` and
  `vllm:kv_cache_usage_perc`**, on the probe port and the main listener, beside the `opod_*` names (which do not
  change). A Gateway API inference pool whose endpoint picker reads vLLM's names — llm-d's default mapping —
  can select opod leaders as pool members with no custom mapping. Waiting is the workers' summed queue, running
  is the leader's in-flight count less that queue, and KV use is the mean over the workers as a 0–1 fraction.
  See `docs/SCALING-SIGNALS.md`. Feature key `vllm_metric_aliases`; a test fails the build if an alias goes.

## 2026-10-06 — `/v1/rerank` is back

- **`POST /v1/rerank`** `{model, query, documents, top_n, return_documents}` answers `results` most relevant
  first, each `{index, relevance_score, document: {text}}`, with `usage`. ADR-084 amends ADR-022, which removed
  rerank on 2026-09-07: a retrieval stack asks for it and the engines already serve it.
- **Served by vLLM, SGLang and llama.cpp** (llama-server started with `--reranking`), read from each engine's
  source: all three serve `/v1/rerank`, SGLang answers a bare array and the others an object, and the leader
  reads both. The engine only scores; the leader orders, truncates to `top_n` and attaches the text, so every
  engine answers alike. The request crosses the leader→worker hop like embeddings do.
- **An engine with no rerank route is refused by name** — Ollama and MLX-LM, or llama-server without
  `--reranking` — as `501 rerank_not_supported`, never a 404 that reads like a wrong URL.
- The route takes the embeddings body cap (8 MiB), runs the pre-call guardrails, and records a usage row like
  embeddings. It is on the frozen leader contract, and the feature key is `rerank`.

## 2026-10-06 — every OpenAI chat field reaches the engine, and logprobs come back

- **A chat request's fields that opod does not model are no longer dropped.** The gateway decoded the body into
  a struct and re-serialised only what it named, so `seed`, `logprobs`, `top_logprobs`, `top_k`, `min_p`,
  `presence_penalty`, `frequency_penalty`, `logit_bias`, `max_completion_tokens`, `reasoning_effort` and `user`
  never reached vLLM, SGLang, llama.cpp or MLX. Every top-level key now rides through verbatim — across the
  leader→worker hop too — and is merged last into the engine body, so a field the next OpenAI revision adds
  works without a release. Ollama builds its own body and is unchanged.
- **Four keys stay ours:** `model` (an endpoint is one model), `messages` (what the guardrails inspected),
  `stream` and `stream_options` (it carries `include_usage`, which usage capture depends on). A caller's value
  for one of them is ignored.
- **`n` or `best_of` above 1 is refused with a 400** that says why. The answer carries one choice, so passing
  them on would generate — and bill — several answers and return one.
- **An answer's `logprobs` come back**, on each streamed chunk and merged into a non-streamed answer.
- Embeddings are unchanged: their `encoding_format` and `dimensions` are still not forwarded.
- Feature key `openai_passthrough` on `/admin/v1/capabilities`.

## 2026-10-06 — a request that finds no capacity can wait for some before the 503

- **The admission hold** (`admission_hold`, policy `admission.holdMs` / `admission.maxHeld`). A request that finds
  nothing able to take it — every holder drained, lost or silent, no gang ready, the endpoint waking — used to be
  answered `503` + `Retry-After` at once, so under a burst every client retried at the same moment. With a budget
  in the policy snapshot it now waits up to `holdMs` for capacity, is served the moment there is some, and gets
  exactly the old `503` when the budget runs out. **`holdMs` 0 or absent is the old behaviour, unchanged.**
- **Bounded.** At most `maxHeld` requests wait at once (64 when unset); past it a request is shed at once. The
  budget counts from the moment the request reached dispatch, so a sleeping engine's wake spends from it rather
  than adding to it. A client that disconnects releases its slot at once.
- **First come, first served.** Nothing is ranked: no priority, no per-key share.
- **A signal, not a poll.** The leader's own writes of who can serve — a heartbeat, a registration, an undrain, a
  resume, a model or gang coming up — wake held requests to look again.
- **Metrics:** `opod_admission_held` (waiting now) and `opod_admission_held_total{outcome}` with `served`,
  `shed_deadline`, `shed_cap` and `cancelled`. A shed still counts in `/loadz` `unavailable_1m`, the autoscaler's
  wake signal, as before.

## 2026-10-06 — a revoked key is a tombstone, and an endpoint can refuse keys from a stale auth snapshot

- **A revocation can be appended, not only re-derived** (`auth_tombstones`). The auth snapshot carries
  `revokedKeys`: ids the leader refuses even while a row for them is still in `keys`. Anyone with RBAC on the
  mounted Secret can deliver one without rebuilding the allow list, so a revoke lands within one file sync even
  while the manager is down. A tombstone seen once is never undone: a later snapshot that does not carry it — the
  manager's next routine push — leaves the key revoked, and a tombstone for an id the leader has never seen keeps
  that id from ever being created live. Tombstones reach only snapshot-owned keys, never one minted locally with
  `opod token create`.
- **The snapshot's age is tracked and reported** (`auth_snapshot_age`). `issuedAt` (RFC 3339) is what the age is
  measured from — a file's mtime moves only when its content does. `/loadz` gains `auth_snapshot`: revision,
  `issued_at`, `age_s`, the bound, whether it is `stale`, and the key and tombstone counts. Absent on a leader
  with no snapshot; `age_s` absent when the snapshot does not say when it was written.
- **An owner may bound that age** with the policy snapshot's `auth.maxSnapshotAgeSec`. Past it, `/v1` answers
  `503` + `Retry-After` with a message naming the stale snapshot, until a fresher one arrives — 503 and not 401,
  because the caller's key may be fine; it is the endpoint that can no longer vouch for any key. The refusal is
  not counted as demand (`unavailable_1m` does not move), the admin surface and worker heartbeats are never
  refused, and a keyless endpoint is untouched. **Unset, 0, or a snapshot with no `issuedAt` is exactly the
  behaviour before**: the leader serves from whatever snapshot it holds, for as long as it holds it.

## 2026-10-06 — building opod from source now needs Go 1.26, and the leader derives a client IP itself

- **The minimum Go to build core rises from 1.25 to 1.26.** It is not a choice: the dependency group this
  release takes in (`golang.org/x/*` at the current releases, `grpc-gateway/v2` 2.31, `otlp` 1.11.1) declares
  `go 1.26.0` itself, and `go mod tidy` raises the directive on its own. A binary from a release is unaffected —
  this is about compiling the module.
- **Every workflow now reads its Go version from `go.mod`** instead of a literal, so the repository and its CI
  cannot drift apart the way they just did: the dependency bump failed three jobs with
  `go.mod requires go >= 1.26.0 (running go 1.25.14)` while nothing was wrong with the bump.
- **The leader no longer uses chi's `middleware.RealIP`**, which v5.3 deprecated under three advisories because
  forwarding headers are attacker-controlled. The behaviour is unchanged — `X-Forwarded-For`, `X-Real-IP` and
  `True-Client-IP` still name the caller in the access log behind an Ingress — and the invariant that made it
  safe is now written down and tested: the rewritten address is a LABEL, and every trust decision (the loopback
  gate on the bootstrap admin key, the audit actor) reads the TCP peer stashed one middleware earlier. Honouring
  the header only from a trusted proxy CIDR is the stronger answer and would be an addition to the environment
  contract, so it is not made here.

## 2026-10-05 — an interrupted weight pull resumes instead of starting over

- **A download that dies at 39 GB of 40 asks for the rest.** Pulls were exclusive, atomic and digest-checked,
  but had no HTTP range resume, so one dropped connection cost the whole file again. The partial now lives under
  one deterministic name, the bytes already on disk are hashed back in, and the request carries `Range`. **The
  single pass over the network is kept**: reading the partial back is a local pass at disk speed, against
  minutes or hours of re-downloading.
- **Resume happens only when the caller declared a digest**, which is the case that matters — a snapshot takes
  the sha256 the Hub records for every LFS file, and LFS is what a weight file is. Without a digest the only
  check is the length, which a corrupt tail would pass, so those pulls start fresh and leave nothing behind,
  exactly as before.
- **Three answers a server can give, all handled:** `206` resumes and takes the total size from `Content-Range`
  (on a partial response `Content-Length` is what remains, not the file); `200` means the server ignored the
  range, so the download starts over and the stale bytes are not mixed into the hash; `416` means the partial is
  longer than the file, so it is removed. A partial that fails its digest is removed too — otherwise every later
  attempt would resume from bytes already known to be bad.

## 2026-10-05 — tool calling and structured output work, and what an engine cannot do is refused by name

- **`response_format` works: ask for JSON and the engine is told to produce JSON.** `{"type":"json_object"}`
  and a full `{"type":"json_schema", …}` are carried to the engine — verbatim for every OpenAI-shaped server
  (vLLM, SGLang, llama-server, MLX), and translated into Ollama's own `format` field, whose schema sits one
  level higher. The object is passed as the caller wrote it rather than re-encoded through our own struct, so a
  field the next OpenAI revision adds still arrives whole.
- **Tool calling works, as a LOOP and not one exchange.** `tools` and `tool_choice` go down to the model,
  `tool_calls` comes back, and the next request carries the assistant's own calls plus the `tool_call_id` of
  each tool result — the three fields a standard client sends on turn two. Streaming and non-streaming both:
  a streaming caller gets the model's fragments as the engine sent them, which is what the OpenAI wire defines;
  a caller who asked for one answer gets them merged, with `arguments` left a string for them to parse and an
  `id` on every call for them to echo back. An answer carrying calls reports `finish_reason: "tool_calls"` even
  when the engine named no reason, because that is the field every client branches on. **Opod never executes a
  tool** — it carries the declaration down and the call back up, and the caller runs the function, exactly as
  with any OpenAI-compatible server.
- **An engine that cannot do tools says so, by name.** Ollama's tool protocol is its own shape — no
  `tool_choice`, no call index, no id, and `arguments` as an object where OpenAI has a string — so that driver
  answers `400 unsupported_request` naming `tools` instead of dropping them and returning prose. The new
  `ErrUnsupportedRequest` class is how any driver says "not this field", and a worker returns it as a `400` the
  leader relays unchanged.
- **Go's decoder discards unknown fields, which is what made this a silent failure.** Before this release a
  request carrying `tools` was answered as if it had never carried one: the engine never saw them, the model
  never emitted a call, and nothing in any log said why.
- **Only the current chat schema is read.** The deprecated `functions` / `function_call` pair is not parsed,
  not served and not refused — it is ignored like any other unknown field.
- **A request field set to `null` is not a request.** A client that serialises an unset field rather than
  omitting it sends `"tools": null`; that is now read as "no tools" rather than as a declaration, everywhere the
  three new fields are tested.
- **A field a worker does not name is dropped one hop short of the engine.** The leader re-serialises every
  chat through the shared OpenAI body builder before it reaches a worker, and the worker's own request struct
  named eight fields — so anything new worked against a leader-local engine and silently did nothing on the
  normal path. The worker now carries `response_format`, `tools` and `tool_choice`, and the struct says why the
  next field added has the same hole waiting for it.

## 2026-09-29 — a door trusts its leader, a budget is what the card loses, gangs take turns

- **One gang's stuck create no longer strands its sibling.** The guard against two creates of one model tearing each
  other's parts down was keyed by the model alone; a gang whose coordinator never came up held its create for the
  whole formation budget and the healthy sibling, whose parts had been re-created meanwhile, was refused its own
  create for as long. A named create replaces only its own gang and places only on the nodes it names, so two named
  creates of different gangs run at once; a whole-model create still excludes every other. (A first attempt at this
  was reverted the same night: the creates collided through the shared head above, which is what the fix before
  this one ends.)
- **A gang create's head is the request's own, never the leader's shared state.** The `head` of a create was written
  into the orchestrator's `OPOD_COORDINATOR_NODE` field and never cleared, so a later or concurrent create read
  whichever head had last been written — a sibling gang's — and fell back to the default picker after a warning.
  The head now rides the create call; the leader-wide setting stays the default for a create that names none.
- **One gang's stuck create no longer strands its sibling.** The guard against two creates of one model tearing each
  other's parts down was keyed by the model alone; a gang whose coordinator never came up held its create for the
  whole formation budget and the healthy sibling, whose parts had been re-created meanwhile, was refused its own
  create for as long. A named create replaces only its own gang and places only on the nodes it names, so two named
  creates of different gangs run at once; a whole-model create still excludes every other. (A first attempt at this
  was reverted the same night: the creates collided through the shared head above, which the entry above ends.)
- **A request whose gang cannot be reached is retried on a sibling gang.** A coordinator that could not be dialled
  — its node died, a part went and llama-server with it — ended the request, although another gang of the same model
  was serving; the walk that already moves a request off an unreachable WORKER now sets the gang aside by its key and
  picks again. With every gang set aside the caller hears "unreachable" (503, retry shortly), never the leader's own
  engine, which holds no sharded model.
- **A cached file is checked against a pinned digest.** A file already complete on disk was trusted by size, so a
  `--sha256` was only ever compared on the download that first wrote the file, and a version that pinned the wrong
  digest for a file the node held was served. A pinned fetch now hashes a cached file once, records the verified
  digest in the file's cache marker, and answers later pinned fetches from the marker; a mismatch is refused by name
  and the file stays (the pin is wrong, not the file).
- **A worker that registers again at a new address is dialled there.** The router's client for a node id was built
  from the first registration and cached by id, so a worker pod re-created under a pinned id — every
  certificate-identified worker — was dialled at its previous pod's IP until the leader restarted: heartbeats 200,
  every chat "worker could not be reached". A re-register that changes the address or the token drops the cached
  client; nothing else about the node is forgotten.
- **A worker with a certificate joins with no token.** `opod join <leader-url>` without `?token=` is a certificate join
  when `OPOD_NODE_CERT`/`OPOD_NODE_KEY` are set: the leader's mTLS gate is the credential check, and the secret the
  leader signs its calls back to the worker with is minted per process and handed over in the register. A pod under
  `nodeMtls=require` carries no shared secret at all, which is what `require` was for. Without a certificate the token
  is still required, by name.
- **A gateway trusts its leader's certificate.** With `OPOD_LEADER_CA` set, the registry mirror, the usage push and the
  spend poll of `--role gateway` trust exactly that certificate; before, a leader serving a minted certificate could
  not be reached by any door. A CA file that does not load is a refusal, not a fallback to the system roots.
- **A VRAM budget is what the card loses.** The memory fraction an engine is started with is the budget less 768 MiB
  of process overhead, cut to two decimals and never rounded up. Measured: a 10 GB budget cost its card 11.08 GB
  before, 10.12 GB after.
- **Equally loaded gangs take turns.** A tie went to the first gang by name, so one request at a time was all served
  by one gang and a canary gang was never sent the requests it is judged on.
- **The uneven-split wording follows its proof** (`README.md`, `docs/SCALING-SIGNALS.md`): in force across machines
  for a llama.cpp gang, measured; delivered and not serving on the one vLLM gang it was tried on.

## 2026-09-29 — a Hub that refuses a fetch says why, by name

- **A gated repository with no token fails as `hub-token-missing`**, not as `GET <url> → 401 Unauthorized`.
  `opod fetch` (one file or `--snapshot`) and a llama.cpp worker's own pull name the refusal: `hub-token-missing`
  (no `HF_TOKEN` was sent and the Hub wants one — the repository is gated or private), `hub-token-refused` (a
  token was sent and answered 401: mistyped, expired or revoked) and `hub-access-denied` (a token was sent and
  answered 403: it is valid, and its account was never granted this repository). Three names because there are
  three remedies. The sentence after the name says which one, and quotes the Hub's `X-Error-Code` when there is one.
- Decided on the status and on whether a token was sent, so it holds behind a mirror (`HF_ENDPOINT`) that sends
  no error header. A 404 and a 5xx are not named: neither is about the credential.
- A refused fetch leaves nothing in the cache — no partial file and no lock.

## 2026-09-29 — routing by the prefix cache a worker actually holds

- **Feature `kv_block_events`.** The prefix pin remembers which worker last served a prompt prefix; it cannot
  know whether that worker's cache still holds the blocks, nor see a second worker that has them warm. With
  `OPOD_KV_EVENTS=1` a worker starts vLLM with its cache events on (a ZeroMQ publisher bound to localhost),
  translates every stored and evicted block into a hash chained from the block before it, and reports the hashes
  on its heartbeat (`kv_blocks`) — **hashes only: no token id and no text leaves the worker**, and the hash is the
  SDK's `nodeapi.BlockHash`, never the engine's own. The leader keeps a bounded per-worker set and, when the
  policy gives `routing.prefixBlockWeight`, takes that much off a worker's load score for each LEADING block of
  the request it holds. A saturated worker still goes last; with nothing reported the pin decides as before.
- The leader has no tokenizer, so it asks a reporting worker's engine (`POST /v1/tokenize` on the worker, vLLM's
  own `/tokenize` behind it), once per distinct prefix and inside a 250 ms budget.
- Whatever could leave the index wrong — a gap in the engine's event sequence, a gap in a worker's batches, a new
  worker process, an unreadable payload — empties that worker's set. `/loadz` carries `prefix_index
  { workers_reporting, blocks }`, so an operator who set the weight can see whether anything is being reported.
- Pure Go: `go-zeromq/zmq4` and `vmihailenco/msgpack`; the static cross-compile is unchanged.

## 2026-09-29 — a request wakes a sleeping engine, and is served

- **A request for a model whose engine sleeps resumes it and is held through the wake** (feature
  `request_wake`). The leader used to answer `503 … waking; retry shortly` and leave the resume to whoever
  manages the endpoint, on that manager's clock — measured on a cluster, the caller was refused and the
  engine woke ten seconds later, which is the price of a parked pod charged for a sleep. Now the leader calls
  the worker's `/v1/model/resume` itself, holds the request for up to 3 s, marks the placement routable on
  the worker's own `200` rather than on its next heartbeat, and serves. One resume per worker however many
  callers arrive together; an engine with no sleep mode (`501`) is left alone; past the budget the caller
  gets the same `503` + `Retry-After` as before while the resume finishes for the next one. The audit stream
  records `worker.resume` with `by: request` and the milliseconds it took. A manager still decides when an
  engine sleeps.

## 2026-09-28 (late) — a key is an identity: per-key quotas, rate limits and allowlists leave core

**Breaking, on purpose.** The daily token quota, the RPM/TPM leaky buckets and the model allowlist that a key
could carry are gone from the request path, the token API, the CLI and the auth snapshot's effect. Per-caller
policy belongs to the application layer in front of an endpoint — an API gateway, a billing service — where
accounts, users and plans live; the runtime turns GPUs into tokens and records every request per key in the
usage stream, which is what such a layer meters from.

- `/v1` no longer answers `429` for a key's minute or day, nor `403 model_not_allowed`; `X-RateLimit-*` headers
  are not emitted. `X-Opod-Request-Id` stays on every response.
- `POST /admin/v1/tokens` and `PATCH /admin/v1/tokens/{id}` accept and ignore `rpm_limit`, `tpm_limit`,
  `quota_daily_tokens` and `allowed_models`; `PATCH` edits expiry only. The token list no longer carries them.
- `opod token create` loses `--models`, `--rpm`, `--tpm`; `opod token edit` is gone (`expire` and `renew` stay).
- A manager's auth snapshot may still carry those fields on a key; the leader ignores them.
- The front doors' spend snapshot (`GET /admin/v1/spend`) carries the doors and the lag bound and no per-key
  ceilings; the doors keep pushing usage and heartbeating through it.
- The `api_keys` columns that carried the policy — `quota_daily_tokens`, `rpm_limit`, `tpm_limit`,
  `allowed_models` — are **dropped on the next open** of a store that still has them (2026-09-29, idempotent
  like every column step; a fresh store never creates them). A value left in one would be a limit an operator
  believes is enforced and is not.

## 2026-09-28 (evening) — per-route body caps, a stable worker identity, and KV pressure llama.cpp can actually report

- **`internal/controlplane` is `internal/leader`.** The package is the leader process's HTTP surface — gateway, `/admin/v1`, adapters — and the old name read as if core knew of a control plane. Its comments now call whatever writes the plan, auth and policy files "a manager", the term `contract.go` already used. No behaviour change.

- **`/v1` request bodies are capped per route**: 32 MiB for `/chat/completions` (`max_body_bytes` overrides that
  one, as before), 8 MiB for `/embeddings`, 1 MiB for anything else. A body over its cap answers
  `413 request_too_large` with an OpenAI-shaped body; it was one flat 32 MiB cap answering 400.
- **A worker with no `OPOD_NODE_ID` and no persisted `node.yaml` derives its id from `POD_NAME`** (`n_<pod name>`)
  before minting a random one, so a worker in a pod restarts as the same node instead of registering a new one
  every time.
- **`/loadz` publishes `kv_busy_workers`** (SDK v0.2.6): the mean KV-cache use across reporting workers times
  the worker count — "how many workers' worth of cache is in use" — beside `kv_used_pct`, which is a maximum. A
  horizontal scaler computes replicas as ceil(metric ÷ target), so only a figure that grows with the replica
  count can ask for more than one step; the control plane's external-scaler KV trigger reads this one.
- **llama.cpp KV pressure is slot occupancy**: the shipped llama-server emits no `kv_cache_usage_ratio`, so the
  field read 0 for ever and the autoscaler's KV trigger could never fire. It is now `requests_processing ÷
  total_slots` (from `/props`, read once) — exact once the context is pinned, because each slot owns a fixed
  partition of it. A build that emits the ratio is still read first.

## 2026-09-28 — the request path stops copying itself, and a removed node leaves nothing behind

Every item here was measured before it was changed; none changes the wire, and the one that changes bytes
(the worker's streamed delta chunk) is held byte-identical to the leader's encoding by a fuzzed test.

- **A non-streamed chat answer is assembled linearly.** The leader concatenated every token onto a string
  (quadratic: 91× slower than a builder at 2000 tokens, 16× worse again at 8k); it uses a builder, as the
  worker already did.
- **A `/v1` request body is read once.** Three middlewares and the handler each read it into their own copy;
  the first reader stashes it and the rest share it. Same limits, same errors.
- **`/readyz` on a router-only leader reads the fleet once per probe** — one node list and one placement
  read — instead of up to four node lists and a placement read per node every ~10 s. Same answers, same
  precedence between the branches.
- **A removed node is forgotten everywhere.** `opod node remove` dropped the row and its placements and left
  the leader's load, engine, reconcile and silence samples, the drivers and samples of any gang it
  coordinated, and the router's per-node state behind; all of it goes now, and the evicted engine clients
  release their connection pools at the eviction rather than at the 90 s idle timeout.
- **The pick path parses a node's capabilities once**, not four times per candidate per request.
- **Prefix-affinity keys cost their first KiB**, not a copy of the whole prompt.
- **A verified model pull hashes the bytes as they land.** The digest was checked by a second full read of the
  finished file — ~80 s of pure I/O on a 40 GB safetensors; a mismatched file is now refused before it is ever
  renamed into place.
- **The streamed delta chunk is appended by hand on both sides.** The worker built a nested map per token and
  marshalled it (key-sorted every call); the leader marshalled a struct. Both now append the fixed structure
  and marshal only the strings, byte-identical to the leader's struct encoding (the frozen contract). A
  worker's delta chunk therefore carries its keys in the leader's order rather than alphabetical order — the
  same JSON, and the leader parses it rather than comparing it.
- **The span-recording stream relay is gone**: one loop forwards and records the token counts; no extra
  goroutine, no extra channel hop per token.
- The catalog's engine-source list is built once per leader rather than once per reported model per heartbeat.

## 2026-09-26 — a worker's identity can be a certificate (R9.6)

- **A worker may be identified by a CLIENT CERTIFICATE instead of the join token it holds** (feature `mtls`,
  ADR-005's P1). With `OPOD_NODE_CERT` + `OPOD_NODE_KEY` the agent presents a keypair on its calls to the
  leader, and the leader reads the node id out of the certificate's **SPIFFE URI SAN**
  `spiffe://<trust domain>/opod/node/<id>` — the one SAN shape an identity is read from, because a common
  name is free text an operator's PKI hands out for other reasons and reading an id from it would make every
  certificate that CA ever signed a worker identity. Three properties a token cannot state: a worker joins
  with **no shared secret at all**; the certificate **names** the node, so one that says another id cannot
  register or heartbeat as it; and a **revoked** certificate is refused although the CA signed it.
- The mode (`off` | `allow` | `require`), the CA and the revoked serials come from the **auth snapshot the
  leader already watches** (`nodeMtls`, `nodeCertCa`, `revokedCerts`), not from its environment, so a fleet
  moves one endpoint at a time and back again without restarting a leader that is serving. Anything
  unrecognised is **off**, never `require` — a typo in a policy file must not lock a fleet out of its own
  leaders — and a mode asked for with no usable CA stays off and says so.
- `require` reaches the **two join routes only**. One listener serves the gateway as well, so the handshake
  asks for a certificate and verifies it if given rather than demanding one from every client; a manager's
  admin calls, which carry a token and no certificate, are untouched (a test holds that). The identity is
  read from the **verified chain**, never from the leaf the client offered. HMAC stays the GA path and the
  only path leader → worker, and with the snapshot silent nothing here is reachable.
- Requires `opod-sdk v0.2.5` (the worker-certificate policy fields, ADR-073).

## 2026-09-24 — a managed key's daily quota, and where a model's weights come from

- **A key a control plane mints now arrives with its daily quota.** The leader has enforced a per-key daily
  token ceiling since before the auth snapshot existed and `QuotaMiddleware` was right the whole time — the
  number simply never arrived: `applyAuthSnapshot` built the key record with the rate limits and the
  allowlist and **not** the quota, and had no update branch for it either, so every managed key landed with
  `quota_daily_tokens = 0`, which the middleware reads as unlimited. It is carried on create and followed on
  edit (`UpdateDailyQuota`), with `0` meaning "no quota" so a ceiling can be lifted as well as raised. Why no
  test caught it: every other quota test set the field on a **locally** minted record and asserted the
  refusal — the path a customer's key actually takes was never walked. Field
  `SnapshotKey.QuotaDailyTokens`, `opod-sdk v0.2.4`.
- **`opod model info` prints the page for those exact weights.** A repo string is a name, not somewhere you
  can go and look, and for a GGUF the FILE is what says which quantization is served. `info` now prints the
  upstream URL beside the source: the Hugging Face blob for a repo + file, the repo page without one, the
  ollama library page for an ollama name. A source with no addressable page — a file staged on the node, or
  one this build does not know — prints nothing, because a guessed link is worse than none.

## 2026-09-22 — an SGLang gang, and image families as data

- **SGLang's native multi-node scheme is a third gang backend** (feature `shard_sglang`), beside llama.cpp's
  RPC parts and vLLM's Ray cluster: one `sglang.launch_server` per rank, one rendezvous address
  (`--dist-init-addr` / `--nnodes` / `--node-rank`), `--tp-size` over the WHOLE gang, `--pp-size` for stages,
  and **rank 0 serving the group's API** as the coordinator the router dials. It is the simplest of the three
  because nothing sits between the engine and the machines. Run on the design-partner cell: a one-part gang
  serves real text; the two-machine pipeline forms, answers 200 with correct token accounting and returns
  noise — cause not isolated, not claimed. It exists because ADR-068 dropped the rule that a tensor group
  never crosses a machine: SGLang's native shape *is* that shape, so the product now says what it costs
  (every all-reduce of every token on the wire) instead of having no multi-node path for the engine at all.
- **An image carries the GPU architecture families it was built for, as data** (`families:` in
  `images.yaml`, and the same tokens on the image's `io.opod.gpu.families` label, held together by a drift
  test) — upstream's ROCm vLLM ships an RDNA build and a CDNA build and neither runs the other's kernels,
  SGLang's ROCm build is Instinct-only, and vLLM's XPU build is Xe2/Xe3 while an Arc A-series card is
  Xe-HPG. `(engine, vendor)` cannot say any of that, so a build that cannot run on a node's silicon is
  refused at plan time instead of faulting on the first token.
- **One create per model holds at a time.** Every create begins by tearing down what it replaces, so two
  creators — a control plane retrying, and this binary's own heal-within-plan loop — were stopping each
  other's parts as "orphans of a previous leader": measured on the cell, twelve minutes without once forming
  a two-part gang, every attempt healthy in isolation. A second create now gets `ErrCreateInFlight` and waits
  for the running one.
- Also: a gang part is routed by **its own rank** (a part on a host that holds two of them is no longer the
  other's), a worker says where it stood when a prune removes a file, and the agent reads its memory fraction
  from the engine rather than assuming.

## 2026-09-21 — front doors, a worker's goodbye, several gangs, and a prune that says where it stood

### Front doors: one brain, several gateways (feature `gateway_spend`, ADR-063)

- **`opod up --role gateway --leader <url>`** runs a copy of the gateway for an endpoint that already has a
  leader. A door serves `/v1` and the probes and **nothing of `/admin/v1`**: the worker registry, the join
  tokens, the gang calls and the plan revision belong to exactly one process, and a door that exposed them
  would be a second place a worker could join or a gang be torn down. Its worker list (nodes, placements,
  gang parts) is **mirrored** from the leader every 10 s — a stale list keeps workers but adopts none — and
  it runs no engine of its own.
- **Usage has one writer.** A door pushes its rows to `POST /admin/v1/usage/push`, at least once,
  deduplicated by a row id the **door** mints (only the door knows two pushes are the same request, and a
  retry after a response it never saw must not bill twice). Every push doubles as a heartbeat carrying the
  door's own load, so an idle door still counts as a door.
- **Ceilings come back from the leader.** `GET /admin/v1/spend` serves what each key has spent in the current
  daily window, its limits, **this door's 1/N share** of them and the lag bound. The share matters more than
  it looks: a flat 1/N hands a keep-alive client — pinned to one door — a fraction of the rate it was sold,
  so the snapshot carries the ceiling *and* the share, computed from the doors the leader has actually heard
  from. A per-key daily quota may therefore lag across doors by a **published** bound (10 s, in the snapshot
  and on `/gatewayz`) rather than by an undocumented one.
- **`GET /gatewayz`** says something different in each role, because the two processes know different things:
  on a **door**, how stale its registry is and how big its backlog is; on the **leader**, the doors it has
  heard from and the **total** they are carrying — the only aggregate a scaler for the doors can honestly
  read, since keep-alive makes one door's share uneven.
- Proven on the design-partner cell, and it cost six defects that no unit test could have held, because each
  is a property of a pod, an image or a restart: the image entrypoint knew only leader and worker · core made
  its configured models directory whatever the role · a door refused to start while its leader was still
  coming up (so, every rollout) · the mirror dropped the worker's join token, giving `401` inside and a `502`
  outside on the first dispatch · **a door aged out the workers it had been told about when the LEADER went
  quiet** — the exact failure doors exist to prevent — so a door now turns its own heartbeat-age rule off and
  publishes how stale its list is instead · and the doors rolled with `MaxSurge 1` beside required
  anti-affinity, which cannot converge.

### A worker says goodbye (feature `worker_goodbye`, ADR-065)

- A worker sends **one final heartbeat** on its way out, declaring its engine `stopped`. Every park and every
  rolling update had a window: the pod gets SIGTERM, the engine — or an RPC part — dies with it, and the
  worker goes on heartbeating through the grace period with its last good report. For those seconds the
  leader believed the capacity was there, picked the dead part and answered **502**. Only the worker can know:
  the leader sees a heartbeat, Kubernetes sees a container that is still Running. A fresh `stopped` takes the
  worker out of the pick, out of `modelServable`, out of `routableNodes` (so a gang's servability follows) and
  out of `/loadz`'s worker count, while `engines_unhealthy` still counts it — the card it holds is still held.
  `crash-looping` does **not** take a worker out: the process may be up again by the next request, and
  flapping an endpoint on a report is worse. Best-effort and bounded at 3 s; a lost goodbye costs exactly what
  we had before, the heartbeat bound.

### Several gangs of one model (feature `shard_groups`)

- A **gang** is one complete serving copy — N parts plus the coordinator that fronts them — and a model may
  now have several. They are independent: a second gang is **throughput**, not a bigger model, and a part
  never spans gangs. `opod shard create <model> --gang g1` names one, `opod shard remove <model> --gang g1`
  and `DELETE /admin/v1/shards/{model_id}/{gang_id}` remove one, and the router picks the **least loaded gang
  whose coordinator is ready**. A bare create still replaces every gang of the model — what it has always
  meant. Two rules make it safe, and both were wrong while a model could only have one gang: every judgement
  is **per gang** (asked over the union, one lost part hid a healthy copy and one ready coordinator vouched
  for a broken sibling), and every id and port **carries its gang** (ports are allocated per node for the
  length of a create, because two gangs may share a worker).
- **`/readyz` tells a parked gang from an awake one that cannot form**: with no worker at all the endpoint is
  `sleeping` — parked on purpose, and the pod must stay Ready so its Deployment converges — while with workers
  registered and nothing able to serve it is `waking`. Both are 200. The floor-0 branch used to be asked
  first and answer `sleeping` for both, so a gang that was serving reported itself parked and a gang whose
  parts could never form reported a healthy parked endpoint.
- **A gang's pressure comes from its coordinator** (feature `gang_load`). A gang's parts are rpc-servers:
  they hold weights and multiply matrices, run no engine, and have no queue, KV cache or notion of a request.
  So the leader scrapes the **coordinator** — the process that has all three — through the driver the router
  dials it with, cached for the heartbeat cadence. Before it, a sharded endpoint reported `kv_used_pct 0` and
  `queue_depth 0` however loaded it was.
- **A leader can form the gang its own plan declares** (feature `plan_heal_gangs`): when a gang in the
  mounted plan has no parts at all and free workers have registered, the leader forms it without an admin
  call — so a gang whose pods were restored by something outside the leader comes back by itself.
- **A gang that cannot be reached is not the leader's engine failing.** An unreachable coordinator — formed
  but still loading across the gang, or missing a part — answers `503 gang_unreachable` + `Retry-After`
  naming the gang. It used to fall through to the leader's own engine error ("vllm at 127.0.0.1:8000 is not
  reachable", with a hint to start it), which described neither what failed nor anything the caller could do.
- **The coordinator goes on the biggest card, not the biggest host.** Host RAM is the wrong quantity and was
  silently wrong wherever hosts are alike and cards are not: two hosts both reporting 31 GB of RAM behind a
  46 GB card and an 8 GB card, the tie falling to whichever came first, and llama.cpp dying on startup
  allocating a 4 GiB KV buffer on the small one.
- **A probe is not a customer** (feature `probe_header`): a request carrying `X-Opod-Probe` is served like any
  other and counted in none of `in_flight`, `rpm_1m`, `unavailable_1m` or the idle clock — the control plane's
  own proof probe was waking gangs the autoscaler had just parked.
- **The probes can have their own port** (feature `probe_port`): `OPOD_PROBE_LISTEN` serves `/healthz`,
  `/readyz`, `/loadz` and `/metrics` on a second, **always-plain** listener, whatever TLS the main one speaks,
  so a scraper or an autoscaler reads a number without being handed a per-endpoint CA — which is how a
  scaling decision ends up resting on a disabled certificate check. Nothing authenticated is routed there.
- **An uneven layer split**, so cards of different sizes can hold one model instead of being bounded by the
  smallest, with the coordinator on the biggest card.

### Everything else that day

- **`opod cache prune --json` reports the cache volume it measured** (`totalBytes`, `freeBytes`, or `volumeErr` when
  statfs failed). A manager that had to infer those numbers from a node probe reported 0 for the one node where they
  matter: on a node whose disk is full the probe is the first pod a kubelet stops admitting, so the node that had run
  out of space reported no space at all. The prune is standing on the filesystem, so it answers.

### Auth: nothing on a worker uses a bearer

- **The leader signs its request-path calls to a worker** (`engines.NodeSigned`, implemented by the shared
  OpenAI-wire client). The router dialled a worker with the worker token in an `Authorization: Bearer` header and no
  signature, so a worker configured HMAC-only (`OPOD_REJECT_BEARER=1`) answered
  `401 unauthorized (HMAC required; bearer disabled)` to every completion — while every other leader→worker call
  already signed. The bearer header still travels for one transition release, as it does everywhere else.
- **The model a worker exists to serve is loaded in-process** (`OPOD_LOAD_MODEL`, with `OPOD_LOAD_REPO` /
  `OPOD_LOAD_FILE` overriding the catalog's source). `opod join` waits for the engine to answer and then calls the
  same load the HTTP route calls (`agent.LoadModel`, one path for both), retrying a load the engine is not ready
  for and leaving the worker heartbeating if it never takes — a registered worker with no model is a state a
  manager can act on; a dead container is not.
- **Why it matters beyond tidiness:** the container entrypoint used to do this by POSTing the worker's OWN
  `/v1/model/load` with `Authorization: Bearer $OPOD_JOIN_TOKEN`. That made it the one caller of a worker's API that
  could not sign HMAC — a shell cannot — so `OPOD_REJECT_BEARER=1`, the switch that closes the transition auth path,
  could never be turned on: the worker registered and then refused its own load. Nothing on a worker needs the bearer
  path to start serving now.

## 2026-09-19 — router log

- **"router skipping stale worker" is logged once per stale episode, not once per request.** A worker whose process is
  replaced keeps its node row, and every pick that walked past the row logged a WARN — one line per request per dead row
  for as long as the leader ran. It is now logged when the worker becomes stale, and again only if it heartbeats and goes
  stale a second time. The `router_pick` metric still counts every skip.

## 2026-09-18 — `opod image`

- **The images as data.** `images/images.yaml` lists every image the repository builds — engine, vendor, platforms,
  weight format, gang scheme, proven on hardware or not, node requirements, limits — embedded in the binary and held
  to `images/build.sh` by a drift test. `opod image ls | show <image> | recommend <model>` print it, all with `--json`;
  `recommend` names the image for a catalog model (or an `hf:` / `file:` id) per accelerator vendor and says why every
  other engine was passed over. Offline and read-only: no config, no store, no registry call.

## 2026-09-07 — manager signals, sleep tier, services, SDK

- **Load signals (`load_signals`).** vLLM and llama.cpp workers scrape their own `/metrics` (KV-cache %,
  requests waiting, generated tokens/s, prefix-cache hit rate); the sample rides the heartbeat and
  `GET /loadz` aggregates the model's live workers (`kv_used_pct`, `queue_depth`, `tokens_per_s`,
  `prefix_hit_pct`, `workers`, `reporting`). The worker passes `--metrics` to `llama-server`.
- **Sleep tier (`worker_sleep`).** `POST /v1/model/sleep|resume` on a worker drives vLLM's sleep mode
  (`--enable-sleep-mode`, set when the manager asks for it); llama.cpp answers `501 unsupported` — never a
  fake sleep. The heartbeat carries `sleeping`, the leader keeps those placements as `sleeping` (not
  routable), `/readyz` answers `sleeping-workers`, and `POST /admin/v1/nodes/{id}/sleep|resume` proxies to
  the worker with its own token.
- **Services.** Node register/heartbeat/drain/remove, model add/delete/unload and shard create/remove are
  plain methods with typed errors (`nodeservice.go`, `modelservice.go`); handlers only decode → call → encode.
- **One emit.** `record()` writes a lifecycle event to the durable log and the in-process bus at once.
- **SDK.** The wire types moved to `github.com/opod-io/opod-sdk/adminapi` (Apache-2.0); the leader
  marshals those exact structs, so a manager imports them instead of mirroring.
- **Images.** `opod-worker-llamacpp-cpu` (CPU build; dev clusters, kind CI) and `opod-worker-llamacpp-intel`
  (SYCL). `images/build.sh` takes `ARCH=arm64` for a local kind on Apple silicon.
- **Licence.** Apache-2.0 (was PolyForm Shield); DCO sign-off, no CLA (ADR-019).

## Gateway

- OpenAI-compatible `/v1/chat/completions`, `/v1/embeddings`, `/v1/models`; SSE streaming with
  client-disconnect handling (bounded drain, no goroutine leaks)
- Engine drivers: **Ollama**, **vLLM** (incl. Tenstorrent's tt-metal build through its `tt` aliases),
  **SGLang**, **MLX-LM**, **llama.cpp** (single node and RPC); `llama-server`, `vllm serve` and
  `sglang.launch_server` are launched by the worker for a placement; engine endpoints and keys per engine via
  env; `GET /admin/v1/capabilities` lists the linked drivers as data (feature `engines`)
- Hardware auto-detection (mac + linux + NVIDIA) and a default model auto-pick
- Per-key API keys with rpm / tpm / daily-token quotas, model allowlists, expiry; OpenAI-style
  `X-RateLimit-*` headers; keys as hashes in a watched `auth.json` when a manager mounts one
- Per-request fallback: an engine 5xx or timeout retries the catalog's fallback chain within the endpoint
- Placement cooldown (circuit breaker) per worker; heartbeat-age liveness drives `/readyz`, node and
  shard listings and routing
- Response cache (in-process, opt-in) and the pre/post guardrail **hook interface** (implementations live
  in the manager; a webhook rule in the mounted `policy.json` is applied by the leader)
- Errors follow the OpenAI shape with a `type` that follows the status (`unavailable`, `server_error`, …)

## Catalog and models

- 47 curated entries with `released:` dates and licence metadata enforced by CI; `opod model search|info|ls|ps`
- Non-catalog installs: `opod model add hf:owner/repo`, `ollama:tag`, `file:/path.gguf`, `--from my.yaml`;
  user entries persist in `~/.opod/catalog/`
- Signed catalog files: a minisign `<file>.minisig` beside a directory catalog file is verified against
  `OPOD_CATALOG_PUBKEY` at every catalog load and in `opod model add --from`; a signature that does not
  verify is always a refusal, `OPOD_CATALOG_REQUIRE_SIGNED=1` refuses unsigned files too, and the embedded
  catalog is exempt
- Pre-flight source probe on add (a certain 404 is refused; an unverifiable source proceeds with a warning)
- Memory lifecycle: installed vs resident, admission, evict-and-swap, release (`/admin/v1/memory`)
- Per-worker VRAM budgets (`opod join --gpu <i> --vram-budget <GB>`) → vLLM `--gpu-memory-utilization`
- Worker flags from a plan (`OPOD_ENGINE_FLAGS`): vLLM tp / gpu_memory_utilization / max_model_len /
  max_num_seqs / kv_cache_dtype; llama.cpp ctx / ngl / parallel / kv_cache_type; inert `extra`

## Cluster

- `opod up` (leader) / `opod join "<leader>?token=…"` (worker); register + 5 s heartbeats with loaded
  models, the engine load sample and the sleep state; HMAC-signed leader → worker calls
- Router: same model on N workers → load-balance; different models → route by placement; a model bigger
  than any node → llama.cpp-RPC sharding (`opod shard create <model> [N] [--nodes …]`), the coordinator on a
  worker, orphan-process sweep on create, `/readyz` counts a ready gang
- `opod shard create <model>` with no count picks the smallest number of equal parts that fits the live
  workers' free memory (never more than the workers or the model's layers) and prints why; a shape the
  caller names is sent untouched and `POST /admin/v1/shards/create` never picks
- `opod node remove` goes through the running leader (its router drops the node's cached connection, cooldown
  and placements) and falls back to the store — placements included — only when none answers; drain, undrain
  and remove say which path they took; `opod node ls` shows the live state (`lost`, `draining`)
- The "can anything serve this?" check is per model: a model whose every holder is drained, lost or asleep
  answers `503` + `Retry-After` — with the cause in the message — while other models on the leader keep
  serving; it used to pass a leader-wide check and answer 502/404 from the local engine
- `GET /v1/models` lists what can be answered for now: a model held only by drained or lost workers (or a
  gang with a part on one) leaves the list; a model that is merely asleep — sleeping workers, a plan that
  scales to zero — is listed, since a request for it is how it wakes
- `/loadz` `workers` (and `reporting`) count only workers that can take a new request for the plan's model:
  a drained, lost, sleeping or still-loading worker is neither capacity nor pressure
- One rule for "this worker takes new work" (`store.Node.TakesNewWork`: not drained, a serving state,
  heartbeating) behind the router, `/readyz`, the waking 503, `/loadz`, `/v1/models`, the shard pickers and
  `opod node ls`; the router applies it before revision groups, so a revision whose only worker is lost or
  cooling down gives its share to the others instead of dropping it on the local fallback
- A heartbeat whose engine did not answer (`loaded_models: null`) is "no report", not "nothing loaded": it
  advances the node's liveness and changes no placement row — it used to delete them all, the leader's
  `draining` / `released` marks included, so one slow engine tick put a draining placement or the source of a
  model move back in rotation. `[]` still clears the rows. A worker whose engine stays silent past the
  heartbeat bound leaves rotation through the one live rule (`nodes.engine_silent_since`; state
  `engine-silent` in `opod node ls`, a 503 that says so, events `node.engine_silent` / `node.engine_reporting`)
  with its rows and marks untouched, and the first real report brings it back (feature `heartbeat_no_report`)
- `opod model move <id> --from <node> --to <node>` (`POST /admin/v1/models/{id}/move`, feature `model_move`):
  another worker takes over a whole model by overlap — load on the target while the source serves, flip the
  router only when the target's heartbeat shows it serving, let in-flight requests on the source finish,
  unload the source; a target that never serves is unloaded again and the source is left serving; refused up
  front when it cannot work (target too small beside what it holds, a one-model engine serving something
  else, a node that takes no new work, a source that does not hold it or serves its adapters, a sharded
  model); every step is an event `model.move_*`; no KV cache moves; an Ollama source's placement is marked
  `released` (not routed to, across leader restarts) because its weights stay installed
- A worker registers which engine it runs (`hardware_json.Engine`, feature `worker_engine`), and each engine
  driver says whether a server of it serves one model per process (vLLM, SGLang, llama.cpp) or several
  (Ollama, MLX) — so the leader can refuse a load that would silently stop another model, naming both
  (`scheduler.LoadWouldReplace`; a worker that did not say gets the blunt rule: refused if it serves anything)
- **Worker images: a pinned model version is never satisfied by a file of the same name.** The entrypoint loaded "the
  whole file at the top of the node cache" by path whenever one existed, which skipped the revision directory AND the
  digest check: a node that had once served the model unpinned served those bytes under every `OPOD_MODEL_REVISION` /
  `OPOD_MODEL_SHA256`. A pinned load now always goes through the agent's fetch (`<models>/<repo>@<rev>/`, verified,
  reused when present); the by-path shortcut stays for unpinned models
- Requires `opod-sdk v0.2.1`: the usage stream carries `ttft_ms` (time to the first usable token of a streamed answer;
  omitted when not measured), and the revision weights of a canary split are read from the shared, typed policy
  snapshot instead of re-parsing the raw document
- **A worker whose engine is not running reports "nothing loaded" (`[]`), not "no report" (`null`).** Since the
  heartbeat learned to say "no report", a refused connection was one too — and the leader takes a worker with no report
  for 30 s out of rotation (`engine-silent`). That hit every worker whose engine is started by a load (vLLM, SGLang,
  llama.cpp) once it had idled for 30 s, and every part of a llama.cpp RPC gang for ever: a part runs an rpc-server,
  never an engine, so the gang was never routable and its leader never ready. A slow engine, or one that answers with
  an error, is still "no report"
- When the worker the router picked cannot be reached and no other can take the request, the gateway answers `503
  worker_unreachable` + `Retry-After` — no capacity right now — instead of `502 engine_unreachable` naming the LEADER's
  own engine with a hint to start `llama-server`. A parked or just-removed worker still looks alive for a heartbeat or
  two; its first caller was told to fix an engine that was never there (chat and embeddings)
- A connection to an engine or a worker must be established within 3 s (`openaicompat.ConnectTimeout`); responses still
  stream with no overall deadline. The client had inherited Go's 30 s connect timeout, and a removed pod's address does
  not refuse, it hangs — so the request picked for a just-removed worker stalled 20–30 s before the next worker was
  asked, and an outer deadline sometimes answered it 502 first (one per pod swap, measured)
- **A worker that has just gone away costs no request while another serves the model.** Its placement row stays
  fresh until its heartbeat ages out; a request picked for it could not connect and was answered 502, because the
  walk went on to the next fallback MODEL, never to the next WORKER. An unreachable worker received nothing, so the
  same model is picked again with that node set aside for this request (chat and embeddings); an engine's own
  answer — a refusal included — is never replayed. The re-pick stays in the revision group the request was assigned
  to, so a traffic split is not re-rolled by a retry. Seen as one failed chat per worker move under a manager's rollout
- `opod model add <id> --node …` no longer replaces what a worker serves in silence: every named worker is judged
  first (the rule `model move` already used), a load that would stop another model is refused with 409 naming both
  — before any worker is touched — and `--force` (`"force": true`) is how an operator says the replacement is meant
- A leader-driven model load (`opod model add --node`, `opod model move`) is no longer cut at 60 s: the worker
  answers when the pull and the load are done, so the call rides the weights client (as the GGUF upload does),
  bounded by the caller's context — a cold pull used to fail at the leader while the worker carried on
- **`engine.preferred: sglang` starts.** The SGLang driver was registered, but the function that builds the
  configured engine for `opod up` / `opod join` chose the endpoint in a hand-written list without it, so a leader
  or worker set to SGLang exited with `unknown engine "sglang" (valid: … sglang …)`. The endpoint is now chosen by
  the driver registry's canonical name (aliases are spelled once, by the driver), SGLang has its own setting
  (`engine.sglang_endpoint`, `OPOD_SGLANG_ENDPOINT`, default `http://127.0.0.1:30000`), and a test fails when a
  linked driver has no endpoint
- An Ollama worker's heartbeat says which of its installed models are in memory (`resident_models`, feature
  of the same name) beside `loaded_models`, which stays "what this worker answers for" — the router needs
  that; the leader marks the rest `cold` (routable, holding no memory) and the shard-count picker's memory
  facts stop counting installed-but-idle Ollama models as memory in use
- A worker can be asked to stop holding a model: `POST /v1/model/unload` (feature `worker_unload`, HMAC-signed
  like `/v1/model/load`, the same source fields) — the engine's own unload where it has one (Ollama), a stop
  of the engine process where the worker launched it (`vllm serve`, SGLang, `llama-server`); idempotent
  (`noop` when not resident), `409` for a shard part or a held adapter of the model, `501` when the engine
  cannot and the worker did not start it; the model leaves the next heartbeat and its placement row goes
- A placement the leader marks `draining` stays out of rotation across the worker's heartbeats (feature
  `placement_drain`) — it used to be rewritten to `ready` within 5 s; it ends when it is set back, when the
  model leaves the worker, or at a leader start
- A LoRA adapter's `rank` travels on the live path too (`POST /admin/v1/adapters {name, source, rank?}` →
  the worker's `/v1/adapters/load`); a worker whose vLLM was started without LoRA slots, or for a smaller
  rank, refuses with `409` and both numbers instead of relaying the engine's error
- `opod model add <id> --node <n>` sends the catalog entry's `source.file` to the worker (`/v1/model/load`
  `file`, omitted when empty), so a repository holding several GGUF files loads the one the entry names
- One rule for "which rows can take a shard part" (`scheduler.WorkerFor`) on the CLI and the API path alike:
  never the leader's own `local` row, a draining node or a silent one; a create that cannot find its workers
  answers `409` with the numbers before the gang it would replace is touched
- `opod node drain|undrain <id>` (`POST /admin/v1/nodes/{id}/drain|undrain`, feature `node_drain`): the
  router, the hedged pick and the shard pickers give a draining worker nothing new, a gang with a part on
  it leaves rotation, in-flight finishes, `/readyz` and the waking 503 do not count it; the state survives
  heartbeats and a re-register
- Router-only leaders (no local engine) are the default in a cluster; `/readyz` modes: local engine ·
  `router-only` · `sleeping` · `sleeping-workers` · `shard-coordinator`
- Plan as a watched file (`/etc/opod/plan.json`): one model identity per leader, revision on `/loadz`,
  floor-0 sleep = `503 waking` + `Retry-After` (itself the wake signal)
- Managed mode (`OPOD_MANAGED=1`): in-memory store, no admin-key file, bounded usage/event rings,
  `boot` stamp on every stream batch; the admin group always requires a key

## Manager surface

- Frozen, additive-only `/admin/v1` contract (`contract.go`, `GET /admin/v1/version|capabilities`,
  `TestLeaderContract`): the probes incl. `/metrics`, the two gateway routes, worker register/heartbeat,
  nodes (list, drain, undrain, sleep, resume, delete), models (list, load, move), adapters (load, drop),
  healthcheck, `events/stream` and `usage/stream` (cursor + replay + `boot`), shards (list, create, delete a
  model's gangs, delete one gang) — and **45 feature keys** beside it, which is what a manager should probe
  instead of sniffing behaviour. `GET /gatewayz`, `POST /admin/v1/usage/push` and `GET /admin/v1/spend` are
  shipped but deliberately **not** frozen yet
- Every state-changing `/admin/v1` call is an `admin.call` lifecycle event (actor, status) — audit rides
  the stream; guardrail verdicts too
- Managed mode: `OPOD_MANAGED` (`TestSurfacesOff` walks the contract on a managed leader). The
  `OPOD_UI` · `OPOD_EGRESS` · `OPOD_CALLBACKS` switches are retired — the surfaces they switched left
  with ADR-022 — and are still accepted and ignored, in the environment and in `config.yaml`
- Prometheus `/metrics`; OTLP traces (`observability.otlp_endpoint`)

## CLI

- `opod up|down|join|status|doctor|version|update|completion`, `opod node ls|show|drain|undrain|remove`,
  `opod model add|load|unload|move|rm|ls|ps|search|info`, `opod shard create|ls|remove`,
  `opod catalog ls|export`, `opod fetch [--snapshot]`, `opod cache ls|prune`,
  `opod image ls|show|recommend`, `opod token create|ls|edit|renew|expire|revoke`,
  `opod connect|disconnect <client>` (copy-paste snippets for 15 OpenAI-shape tools), `opod config show`
  (secrets redacted) `|path|edit`
- Interactive pickers, `--json` everywhere, `NO_COLOR`, a "did you mean" on a mistyped verb

## Release and ops

- One-line installer (`curl | sh`) for macOS (Apple silicon) and Linux (x86_64, arm64); launchd / systemd
  units; `opod update` with checksum verification; static single binary
- Images from the official upstream engine images plus a thin opod layer, pushed to `ghcr.io/opod-io/*`,
  digest-pinned by consumers, never built on cluster nodes; the llama.cpp CUDA RPC pair prebuilt once per
  release (`images/README.md`)
- Reference Grafana dashboards in `dashboards/`

## Security and hardening

- SSRF protection for outbound hook clients (`block_private_targets`); forwarded headers gated behind
  `trust_proxy_headers`; `opod update` tar extraction hardened; `SQLITE_BUSY` avoided with `busy_timeout`;
  dev mode never locks out cluster admin; the managed admin group always requires a key

## Removed with ADR-022 (2026-09-05 → 2026-09-07) — now the control plane's

The embedded dashboard and its Connect/Invite tabs, the localhost key bootstrap, the automatic update check,
vendor egress (12 hosted providers, key pools, Bedrock/Vertex signing, the `model="auto"` routing chain),
the Anthropic Messages and audio/rerank adapters, callback sinks (webhooks, Langfuse, S3), dollar budgets
and `$` cost fields, the usage/audit query APIs (`opod usage|audit`), and the guardrail implementations.
Each has one home now: the control plane (`opodcp`) — console, Routing, Integrations, Usage, Audit,
Guardrails pages — driven from core's typed event and usage streams. Core keeps the mechanisms and the
hook interfaces. Rollback of any core release is a re-pin of the previous leader image digest.
