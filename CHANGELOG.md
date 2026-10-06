# Changelog

What core ships today, by area — the honest inventory after ADR-022 (2026-09-05/07) contracted core to
the CLI-only inference runtime. For the per-release diff see
[Releases](https://github.com/opod-io/opod-core/releases). For what moved to the control plane and why, see
the last section.

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
- **Tool calling works: `tools` goes down to the model and `tool_calls` comes back.** Both the declaration and
  the choice are carried verbatim, streaming and non-streaming. A streaming caller gets the model's fragments as
  the engine sent them, which is what the OpenAI wire defines; a caller who asked for one answer gets them
  merged by call index, with `arguments` left a string for them to parse. **Opod never executes a tool** — it
  carries the declaration down and the call back up, and the caller runs the function, exactly as with any
  OpenAI-compatible server.
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
