# Personal Data Store — Implementation Plan

A personal knowledge capture system: ingest notes via SMS/web/Shortcut, enrich with LLM extraction, store in DynamoDB, serve instant search-as-you-type from an in-RAM Go index, answer questions and fire reminders.

**Working name:** `recall` (rename freely)

---

## Architecture summary

```
Capture (SMS / web / Shortcut)
        │
        ▼
Ingestion API ──────────► DynamoDB (single table, source of truth)
                              │
                              ▼ (DynamoDB Streams)
                    Enrichment Lambda
                    ├─ LLM extraction → canonical text, entities, dates, facts, tags
                    ├─ MiniLM embeddings (per fact)
                    └─ writes enriched attrs + edge items back to Dynamo
                              │
                              ▼ (stream)
                    Go Search Service (Fly.io, persistent)
                    ├─ RAM: vector matrix, trie, inverted index, trigram index,
                    │        prefix-embedding cache, preview table
                    ├─ GET /search?q=   → tiered match + RRF → previews (~1ms)
                    └─ GET /notes/{id}  → Dynamo GetItem → full note

EventBridge cron → due-reminder query → Twilio outbound SMS
```

**Key invariant:** everything in the Go service's RAM is derived from Dynamo and rebuildable on boot. Dynamo is the only stateful component.

---

## Feasibility & cost

| Component | Service | Est. monthly cost |
|---|---|---|
| Notes table + streams | DynamoDB on-demand | ~$0 (free tier covers personal volume) |
| Ingestion + enrichment | Lambda + API Gateway | ~$0 (free tier) |
| LLM extraction | Claude Haiku via Anthropic API | < $1 (1 short call per note) |
| Embeddings | all-MiniLM-L6-v2, local ONNX | $0 |
| Search service | Fly.io shared-cpu-1x, 256MB | ~$2–3 (or free allowance) |
| SMS in/out | Twilio | ~$1.15 number + ~$0.0079/msg |
| Reminders cron | EventBridge | $0 |

**Total: roughly $5/month.** Latency targets: capture ack < 200ms, note searchable < 2s after send, search keystroke < 50ms end-to-end, click-to-full-note < 100ms.

**Risk register (ranked):**
1. *ONNX-in-Go friction* — tokenizer + onnxruntime bindings are the fiddliest dependency. Mitigation: Phase 4 isolates this in `pkg/embed` with a fallback (Python sidecar) if bindings fight you.
2. *LLM extraction quality* — bad date normalization or invented entities pollute edges. Mitigation: golden-file test suite for the prompt before wiring it in.
3. *Stream→service delivery* — Lambda can't push to Fly.io if the service is asleep. Mitigation: service also polls/rebuilds on boot; push is an optimization.
4. *Scale* — non-issue. Flat scan + maps are fine past 50k notes; swappable upgrades (int8, SIMD, HNSW) documented in Phase 9.

---

## Repo layout

```
recall/
├── infra/                  # CDK (TypeScript) — table, lambdas, API GW, EventBridge
├── cmd/
│   ├── searchd/            # the Go search service (main)
│   └── reindex/            # CLI: rebuild index from Dynamo (also used by searchd on boot)
├── pkg/
│   ├── store/              # Dynamo access: PutNote, GetNote, QueryEdges, stream records
│   ├── extract/            # LLM extraction client + prompt + golden tests
│   ├── embed/              # ONNX MiniLM wrapper: Embed(string) []float32
│   ├── index/              # trie, inverted, trigram, vector matrix, RRF merge
│   └── api/                # HTTP handlers for searchd
├── lambdas/
│   ├── ingest/             # POST /entries → Dynamo
│   ├── enrich/             # stream consumer → extract + embed + fan-out
│   └── remind/             # EventBridge → due reminders → Twilio
└── web/                    # capture box + search UI (plain React or vanilla)
```

---

## Phase 0 — Prerequisites (½ day)

**You create:**
- [ ] AWS account (or use existing) + IAM user/role with admin for personal dev; install AWS CLI v2, configure profile `recall`
- [ ] Anthropic API key (console.anthropic.com) → store in AWS Secrets Manager as `recall/anthropic`
- [ ] Fly.io account, install `flyctl`
- [ ] Go 1.22+, Node 20+ (for CDK), Docker

**Defer until needed:** Twilio (Phase 7). Start with web capture so SMS cost/setup never blocks progress.

**Acceptance:** `aws sts get-caller-identity` works under the `recall` profile; `flyctl auth whoami` works.

---

## Phase 1 — Infra skeleton + Dynamo table (½–1 day)

**Goal:** one CDK stack, one table, streams on.

**Implementation details:**
- Single table `recall-main`, on-demand capacity, `PK` (S) / `SK` (S), DynamoDB Streams = `NEW_AND_OLD_IMAGES`.
- GSI1: `GSI1PK` / `GSI1SK`, projection `ALL` — used sparsely for reminders (`USER#joshua#REMINDER` / due ISO timestamp).
- TTL attribute `ttl` enabled (used later for chat-turn items).

**Item model (write it in `pkg/store/model.go` now, even before code uses it):**

```
Note (source of truth)
  PK: USER#joshua            SK: NOTE#<ULID>
  raw_text, canonical, source, type, subtype,
  entities [..], when {..}, tags [..], facts [..],
  embeddings: list of Binary (one per fact, 384 × float32 LE),
  enriched (bool), created_at

Edges (written by enrichment, all small)
  PK: ENTITY#<slug>          SK: NOTE#<ULID>
  PK: DATE#R#<MM-DD>         SK: NOTE#<ULID>      # recurring
  PK: DATE#<YYYY-MM-DD>      SK: NOTE#<ULID>      # one-off
  PK: TYPE#<type>            SK: <sort-useful-key>#NOTE#<ULID>

Reminder projection (sparse GSI)
  GSI1PK: USER#joshua#REMINDER   GSI1SK: <due ISO8601>
```

**Acceptance:** `cdk deploy` succeeds; you can `aws dynamodb put-item` and `get-item` a dummy note from the CLI.

---

## Phase 2 — Ingestion API (1 day)

**Goal:** `POST /entries {text, source}` → raw note in Dynamo in <200ms. No AI anywhere in this path.

**Implementation details:**
- API Gateway HTTP API → `lambdas/ingest` (Go runtime, `provided.al2023`).
- Generate ULID server-side (`github.com/oklog/ulid/v2`) — gives you time-ordered SKs for free.
- Write with `enriched=false`. Return `201 {id}` immediately.
- Auth: a single static bearer token in Secrets Manager for now (it's a one-user system); check it in the Lambda. Don't build users.
- Idempotency: accept optional client `Idempotency-Key`, use conditional put on a derived SK if provided (matters once SMS retries exist).

**Acceptance:** `curl -H "Authorization: Bearer ..." -d '{"text":"jaden bday march 12","source":"web"}'` returns 201 in <300ms cold, <100ms warm; item visible in Dynamo.

---

## Phase 3 — Capture web UI (½ day)

**Goal:** a textbox you'll actually use, deployed.

**Implementation details:**
- `web/`: single page — textarea, submit on Cmd+Enter, optimistic "saved ✓". Host on Fly static / S3+CloudFront / Vercel, whatever's fastest for you.
- iOS Shortcut (15 minutes, no code): Share Sheet → Get Text → `POST /entries` with the bearer token. Gives you Siri capture before Twilio exists.

**Acceptance:** you can capture a note from your phone in under 5 seconds. **Start using it daily now** — real notes are your test corpus for everything after.

---

## Phase 4 — Embeddings package (1–2 days; the fiddly one)

**Goal:** `embed.Embed("jaden's birthday is march 12") → [384]float32`, pure Go process, ~2–5ms.

**Implementation details:**
- Model: `all-MiniLM-L6-v2` ONNX export (HuggingFace `optimum/all-MiniLM-L6-v2`). Vendor the `.onnx` + `tokenizer.json` into the repo or pull at build.
- Runtime: `github.com/yalue/onnxruntime_go` + the onnxruntime shared lib (bundle in Docker image).
- Tokenizer: `github.com/sugarme/tokenizer` loads HF `tokenizer.json` directly.
- Pipeline: tokenize → run session → mean-pool token embeddings with attention mask → L2-normalize. Normalize HERE, once, so search is a plain dot product.
- **Fallback if bindings fight you for >half a day:** tiny FastAPI sidecar with `fastembed`, same container, localhost HTTP. Swap later; the `Embed(string) []float32` interface doesn't change.

**Acceptance:** unit test: cosine("birthday party", "celebration of birth") > cosine("birthday party", "dynamodb throughput"). Benchmark: < 10ms/call on your laptop.

---

## Phase 5 — Enrichment Lambda + extraction prompt (2 days)

**Goal:** stream-triggered Lambda turns raw notes into canonical + entities + dates + facts + embeddings + edge items.

**Implementation details:**
- Trigger: Dynamo Streams event source, filter to INSERTs where `enriched=false` (event filter pattern, saves invocations).
- LLM call: Claude Haiku, strict-JSON prompt. Contract:

```json
{
  "canonical": "...", "type": "event|reminder|fact|idea|journal",
  "subtype": "...", "entities": [{"name":"...","kind":"person|place|thing"}],
  "when": {"date":"YYYY-MM-DD"} | {"month":M,"day":D,"recurrence":"yearly"} | null,
  "tags": ["..."], "facts": ["...", "..."]
}
```

- Prompt rules to encode: resolve relative dates against `created_at` ("Friday" → concrete date); birthdays/anniversaries → recurring; do NOT invent entities not in the text; 1–4 facts max; facts must each stand alone.
- **Golden tests first:** `pkg/extract/testdata/*.json` — 15–20 real-ish inputs with expected outputs (relative dates, recurring vs one-off, multi-fact, typo'd names, notes with no date). Run against the live API in a `-tags=integration` test. Iterate the prompt until green. This is the highest-leverage testing in the project.
- Embed each fact via `pkg/embed` (runs in-Lambda; bundle ONNX in the image — Lambda container image, not zip).
- Write-back: single `TransactWriteItems` — update note (canonical, facts, embeddings as Binary list, `enriched=true`, conditional on `enriched=false` for idempotency) + put edge items + put GSI reminder attrs if `type=reminder`.

**Acceptance:** text a note (via web), watch the item gain `canonical/facts/embeddings` and edge items appear within ~2s. Replay-safe: re-driving the same stream record is a no-op.

---

## Phase 6 — Go search service (3–4 days; the heart)

**Goal:** `searchd` on Fly.io. `GET /search?q=` returns previews in <50ms end-to-end; `GET /notes/{id}` proxies GetItem.

**Implementation details, in build order:**

1. **`pkg/index` structures** (pure, unit-testable, no I/O):
   - `VectorIndex`: `[][]float32` + parallel `[]FactRef{noteIdx, factIdx}`; `TopK(q, k)` flat scan.
   - `Trie` over entity names, tags, title tokens → note indices.
   - `Inverted`: `map[token][]noteIdx` with BM25-lite scoring (or plain tf — fine at this scale).
   - `Trigram`: `map[gram][]noteIdx`, Jaccard score for typo tolerance.
   - `RRF(k int, lists ...[]Scored) []Scored` — `Σ 1/(60+rank)`.
2. **Boot rebuild** (`cmd/reindex`, also called by searchd on start): paginated Query of `USER#joshua` partition, decode stored embeddings (no re-embedding), build all structures, log `rebuilt N notes in Xs`.
3. **Prefix cache**: enumerate 1–5 char prefixes of corpus vocabulary, embed once at boot (or lazily on first miss + memoize). `map[string][]float32`.
4. **Search handler**: lowercase/trim → resolve vector (prefix map hit, else live `embed.Embed` in goroutine) → run exact tiers concurrently (trie title hits, inverted body hits, trigram fuzzy hits) → semantic TopK → tier-ordered RRF merge → top 8 previews `{id, title, snippet, tags, score}`.
5. **Freshness**: simplest correct thing first — poll Dynamo every 30s for items with `created_at > lastSeen` (cheap Query, newest-first, stop early). Optimization later: enrichment Lambda POSTs `/internal/refresh` to the service.
6. **Deploy**: Dockerfile bundling onnxruntime lib + model files; `fly launch`, 256MB, single region near you (sea/yyz). IAM: a scoped access key in Fly secrets with `dynamodb:Query/GetItem` on the table only.

**Acceptance:** `hey -n 500 'https://.../search?q=jad'` p99 < 60ms; typo query `jadne` returns Jaden notes; novel long query returns sane semantic results; kill + restart rebuilds in seconds.

---

## Phase 7 — Search UI + SMS capture (1–2 days)

**Search UI (`web/`):**
- Input with 30–50ms debounce; render previews; **stale-response guard** (tag request with query string, drop responses that no longer match the box); entity rows above note previews; click → fetch full note.

**SMS — you create:**
- [ ] Twilio account, buy a local number (~$1.15/mo), A2P registration if required for your number type.
- Point Messaging webhook → new API GW route → reuse `ingest` Lambda (validate `X-Twilio-Signature`; map From-number → `USER#joshua`).
- Reply TwiML "✓ saved" so capture is confirmed in-thread.

**Acceptance:** text the number → "✓ saved" → searchable from the web UI within ~2s.

---

## Phase 8 — Ask mode + reminders (2 days)

**Ask mode** (`POST /ask` on searchd, or a route that runs on enter-key):
- Local fast paths first: date parser (`when.codes`/`araddon/dateparse`) catches "march 12", "this friday"; known-entity match catches "jaden" — both skip the LLM.
- Otherwise one Haiku call parsing the question into the SAME schema as extraction → route: entity edge query / date edge queries (R# + literal, union) / type query / semantic fallback → merge → answer with retrieved notes; optional summarize call if the question asks for synthesis.

**Reminders:**
- EventBridge rule, rate(15 minutes) → `lambdas/remind`: GSI1 query `GSI1SK between now and now+15m` → Twilio outbound → mark `notified_at` (conditional, idempotent).

**Acceptance:** "what's happening march 12" returns Jaden's birthday in both 2026 and 2027 test data; a reminder created via SMS arrives as an SMS.

---

## Phase 9 — Held-in-reserve optimizations (do not build now)

Documented so the README can say "known scaling path":
- int8 quantization of vectors (4× RAM, ~free quality cost) — when matrix > ~500MB
- SIMD dot product (cgo/assembly kernel) — when flat scan > 1ms
- HNSW behind the same `TopK` interface — when scan > 5ms (≈ hundreds of thousands of facts)
- Client-side index bundle (transformers.js MiniLM in browser) — for offline/zero-RTT keystroke search

---

## Suggested order & calendar

| Week | Phases | Milestone |
|---|---|---|
| 1 | 0–3 | Capturing real notes daily via web + Shortcut |
| 2 | 4–5 | Notes auto-enriched; extraction prompt green on goldens |
| 3 | 6 | Search-as-you-type live against your real corpus |
| 4 | 7–8 | SMS capture, ask mode, reminders — feature complete |

Each phase ends in something independently useful, so you can pause anywhere and still have a working tool.
