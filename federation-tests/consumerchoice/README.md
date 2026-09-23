# ConsumerChoice end-to-end validation

A focused, automated functional test of the **ConsumerChoice** placement policy:
a consumer describes what it wants in natural language, a small local LLM picks one
provider from everything the Broker offers, and that choice becomes a real reservation
federated through the normal Broker, agent and Liqo workflow.

> **What this is, and is not.** This is a functional validation of AI-assisted provider
> selection. It is **not** a benchmark of LLM intelligence. A selection is judged only on
> measurable properties (carbon intensity, distance, cost, resources), and an aggregate pass
> rate is evidence that choices are consistent with the configured scenarios, **not** proof
> that they are optimal in general.

---

## What the test demonstrates

For every decision the run records evidence that:

1. real provider agents advertised carbon intensity, prices, capacity and location;
2. the Broker, under ConsumerChoice, returned **every** eligible provider instead of
   pre-selecting one;
3. the consumer's own selector (`internal/agent/ollama`, the code the agent ships) sent the
   request, the consumer's location and the candidate list to a local model through Ollama.
   The model receives raw data only; nothing, distance included, is computed for it, though the
   prompt tells it how to compare coordinates when the request is about proximity;
4. the model returned a ranking, and its first entry, exactly as the model wrote it, is the
   selected provider;
5. that answer was validated as untrusted input before anything happened;
6. exactly one reservation was created for it and reached `Peered`;
7. the capacity was released and the federation returned to its starting state.

Once per scenario, the run also checks the **agent path**: the consumer agent decides on its
own, as a deployed consumer does. A manual reservation made from the consumer console goes
through the agent's local API, which asks the model and masks the list. The run then checks
that the agent asked the model, that the reservation became `Active` (a borrowed virtual
node) on the provider the model ranked first, and that it was released cleanly.

The model is queried **exactly as the consumer agent queries it**: plain JSON mode, Ollama's
default sampling, the same timeout. No schema, temperature or seed makes the harness's
decisions tidier than the agent's, so results describe the agent as deployed. Choices can
therefore vary between repetitions; repeatability measures how much.

If the Broker masks the list (so there is no real choice), the run aborts with
`ConsumerChoice unavailable`. It never falls back to running another policy.

---

## Workflow

Each scenario runs two parts.

**Recorded decisions** (`repetitions` times). The harness drives the agent's selector itself,
so every prompt, raw answer and check is recorded:

```
provider agents ──advertise──▶ Broker ◀──heartbeat (policy: ConsumerChoice)── consumer agent
                                  │
harness (as consumer-1) ──GET /api/v1/nodegroups──▶ all eligible providers
      │
      ├─ NodeGroupViewToProviderInfo: normalise ──▶ the provider list in prompt.txt
      ├─ SelectDetailed: request + consumer location + providers
      │                  ──▶ Ollama (/api/generate, JSON mode, default sampling)
      ├─ validate: JSON, ID in the list sent, still listed, still has capacity
      ├─ fallback only if validation failed (always recorded)
      ├─ POST /api/v1/reservations (X-Reservation-Id) ──▶ poll to Peered
      │                                    real agents perform Liqo peering
      └─ release ──▶ wait for baseline capacity ──▶ artifacts
```

**Agent path** (once). The harness only asks for capacity; the consumer does the rest:

```
harness ──POST /api/reservation──▶ consumer console ──▶ ResourceRequest
      ResourceRequest controller (gRPC server) ──GET /local/nodegroups──▶ agent local API
            agent ──▶ Broker (all eligible providers)
            agent ──▶ Ollama (its own call: request + its location + providers)
            agent masks the list to the model's first choice ──▶ controller reserves it
      ──▶ Liqo virtual node ──▶ ResourceRequest Active
harness: reads the agent's "ConsumerChoice selection finished" log line,
         checks the reservation landed on that choice, releases it
```

For the recorded decisions the harness acts as the consumer with consumer-1's own mTLS
identity, like the other comparative suites. The Cluster Autoscaler is disabled, so the only
reservations are the harness's and the agent path's.

The consumer location in the prompt is the one the consumer discovered for itself, read
from its console. It is the same discovery the agent's heartbeat uses, and the agent
passes it to the model the same way: latitude, longitude and region code, next to each
provider's own coordinates. How close a provider is, the model works out by itself.

ConsumerChoice is switched on **before** the harness checks that providers advertise their
profiles. Every other policy makes the Broker hide all providers but one
(`maxSize = currentReserved`), and that would also hide the capacity the check needs to see.
If providers stay hidden after ConsumerChoice is active, the run stops with
`ConsumerChoice unavailable`.

---

## Prerequisites

On the machine that runs the test:

- `go`, `docker` (running), `kind`, `kubectl` on `PATH`;
- enough CPU and RAM for 11 Kind clusters (1 Broker, 1 consumer, 9 providers);
- Internet access **on the first run only**, to pull the Ollama image and the model.

Nothing else. Ollama does **not** need to be installed.

The general setup (supported host, kernel limits for many Kind clusters) and the machine
the published results come from are in sections 2 and 15 of the
[test guide](../guide.md).

---

## How Ollama is used

With the default `ollama.managed: true`, every run:

1. starts a dedicated container from a pinned image (`ollama/ollama:0.34.0`), listening on
   a random loopback port, **before** the clusters are created;
2. waits for the server;
3. pulls the model if it is not already cached — in the background, while clusters are
   being created. Ollama downloads it with the container's own network; if that network
   cannot reach the registry (some hosts give containers no DNS), the run retries once
   through a short-lived Ollama container on the host's network, using the host's own
   `/etc/resolv.conf`, that writes to the same cache volume, then checks the run's
   container lists the model;
4. still in the background, makes one synthetic selection to prove the runtime answers the
   request. If any of steps 2–4 fails, cluster creation is cancelled and the run stops within
   minutes instead of after the whole federation is up;
5. once the clusters are up, attaches the container to the Kind Docker network and points
   the consumer agent at it (`agent-config` `ollamaUrl`/`ollamaModel`/`ollamaTimeout`, the
   same keys `consumer-up.sh --ollama-url` sets), then restarts the agent;
6. repeats the synthetic selection just before the first scenario, so model load time is not
   booked as the first decision's latency;
7. removes the container at the end.

Only a synthetic selection that gets no answer at all (server unreachable, HTTP error) stops
the run. A timeout stops it only in step 6: in step 4 the setup is compiling images and
starting clusters on the same CPUs, so a slow answer there proves nothing. An answer that
fails validation is recorded as a warning in the summary, because it is a result about the
model and not a broken setup.

The model lives in the Docker volume `federation-autoscaler-ollama`, so it is downloaded
once and reused by every later run. Set `removeModelCache: true` to delete it afterwards.

To use an Ollama you already run, set `managed: false` and `baseUrl` (how the harness on the
host reaches it) and, for the agent path, `agentBaseUrl` (how a pod in the consumer's Kind
cluster reaches it). If the model is missing and `pullIfMissing` is `false`, the run stops
and prints the exact `ollama pull <model>` command to run.

### Enabling the LLM on a real consumer

The agent path uses the same switch a real deployment uses. Without it, a consumer with a
ConsumerChoice policy never asks an LLM and always uses the deterministic fallback:

```bash
deploy/standalone/consumer-up.sh … --ollama-url http://<ollama-host>:11434 \
                                   [--ollama-model llama3.2] [--ollama-timeout 120s]
```

With Ansible, set `fa_ollama_url` (and optionally `fa_ollama_model`, `fa_ollama_timeout`).
The URL must be reachable from the consumer agent's pod.

No cloud API, key or Internet connection is used while decisions are being made.

---

## Run it

```bash
./federation-tests/consumerchoice/run-consumerchoice.sh --config federation-tests/consumerchoice/configs/default.yaml
```

That is the whole procedure. `default.yaml` is the smoke test: one eco-oriented decision
and one reservation.

Optional flags:

| Flag | Effect |
|---|---|
| `--keep-clusters` | Keep clusters and the Ollama container for inspection (same as `cleanup: false`) |
| `--skip-build` | Reuse built images. **Do not use** after changing Broker or agent code |
| `--run-id <id>` | Force the run ID |

The first run after a code change must rebuild the images, so leave `--skip-build` off.
Manifests always come from the repository but images do not: the consumer manifest now
passes `--ollama-url/--ollama-model/--ollama-timeout` to the agent, and an agent image built
before that change does not know those flags and will not start. This applies to every
suite (eco and latency too), not only this one.

If the image build fails in `go mod download` with a DNS error, the host's containers cannot
resolve names. Build through the host's network instead:

```bash
DOCKER_BUILD_FLAGS=--network=host bash federation-tests/consumerchoice/run-consumerchoice.sh \
  --config federation-tests/consumerchoice/configs/default.yaml
```

The build normally reuses the Go modules already downloaded, so this only shows up after
`go.mod` changes. Do not run `go mod tidy` without a real reason: reordering `go.mod` alone
discards that cache.

---

## Configuration

Every file in `configs/` is complete and self-contained. Normally you only edit
`scenarios[].userRequest`.

| File | Contents |
|---|---|
| `default.yaml` | Smoke test: S1 eco-oriented, 1 repetition |
| `eco-oriented.yaml` | S1, 5 repetitions |
| `proximity-oriented.yaml` | S2, 5 repetitions |
| `balanced.yaml` | S3, 5 repetitions |
| `ambiguous.yaml` | S4, 5 repetitions |
| `all-scenarios.yaml` | S1–S4 on one deployment, 5 repetitions each (the thesis run) |

### Main options (`consumerChoice:` section)

| Key | Default | Meaning |
|---|---|---|
| `mode` | — | Must be `consumerchoice`; anything else aborts |
| `repetitions` | `1` | Decisions per scenario, each with its own reservation, under the same provider state |
| `metadataTimeout` | `5m` | Wait for providers to advertise their profiles |
| `policyTimeout` | `2m` | Wait for the Broker to report ConsumerChoice |
| `ollama.model` | `llama3.2` | Same default as the agent's `--ollama-model` |
| `ollama.timeout` | `120s` (the agent's `--ollama-timeout` default) | One decision, and the agent's `--ollama-timeout` for the run. A 3B model on CPU needs tens of seconds; a timeout rejects the answer as the model's own, and the fallback reserves instead |
| `ollama.agentBaseUrl` | — | Only with `managed: false`: how the consumer agent reaches Ollama |
| `agentPath.enabled` | `true` | Run the agent-path check once per scenario |
| `agentPath.cpu` / `agentPath.memory` | `1` / `1Gi` | Size of the manual reservation; must fit in one chunk |
| `agentPath.timeout` | `10m` | Until the reservation is `Active` |
| `fallback.mode` | `agent-default` | See [Safety and fallback](#safety-and-fallback) |
| `providerProfiles` | — | Carbon, prices and capacity per provider, aligned with `providerRegions` |
| `scenarios` | — | Name, request, criterion and optional reference weights |

An unknown key in the `consumerChoice:` section is an error, so a misspelt or retired setting
(for example `temperature`) fails the run at load time rather than being silently ignored.

The shared keys (`consumers`, `providers`, `providerRegions`, `experiment.reservationPoll`,
`experiment.reservationTimeout`, `infra`, `cleanup`, `output`) behave as in the other
suites. `providerRegions` must be listed explicitly, because distances are part of the
experiment.

### The provider catalogue

The consumer is placed in `providerRegions[0]` (Milan), so provider-1 is co-located with it.

The Distance column is **not** given to the model. The harness computes it (great-circle,
the formula the Latency policy ranks on) only to judge the choice afterwards, and it
appears in `providers.csv` and `federation.csv` for the same purpose.

| Provider | Region | Distance | Carbon (gCO2eq/kWh) | Cost per chunk per hour | Chunks |
|---|---|---|---|---|---|
| provider-1 | Milan | 0 km | 520 | 0.076 | 2 |
| provider-2 | Zurich | ~220 km | 45 | 0.114 | 2 |
| provider-3 | Paris | ~640 km | 60 | 0.090 | 3 |
| provider-4 | Frankfurt | ~520 km | 350 | 0.052 | 2 |
| provider-5 | Vienna | ~620 km | 150 | 0.076 | 1 |
| provider-6 | London | ~960 km | 230 | 0.056 | 3 |
| provider-7 | Helsinki | ~1 940 km | 35 | 0.064 | 2 |
| provider-8 | Mumbai | ~6 470 km | 720 | 0.026 | 4 |
| provider-9 | Montreal | ~6 130 km | 25 | 0.082 | 2 |

The trade-offs are deliberate: the nearest provider is a high-carbon outlier, the greenest
is one of the farthest, the cheapest is the dirtiest and farthest. Otherwise one provider would win every
request and the scenarios could not be told apart.

---

## Scenarios

Ranks are computed among the eligible providers the model was shown: 1 is the lowest value,
tied values share a rank. "Worst quartile" means the `ceil(0.25 · n)` worst ranks.

| | Request | Criterion | Providers that pass (default catalogue) |
|---|---|---|---|
| **S1** eco-oriented | *Prioritize the greenest provider. Latency is secondary.* | carbon rank ≤ 2 | Helsinki, Montreal |
| **S2** proximity-oriented | *Choose a provider close to me, and preferably with low emissions.* | distance rank ≤ 3, and not a worst-quartile carbon choice while a cleaner nearby provider exists | Zurich |
| **S3** balanced | *I need a good balance between low emissions and proximity.* | neither worst-quartile carbon nor worst-quartile distance | Zurich, Paris, Vienna, London |
| **S4** ambiguous | *Choose the best provider for my workload.* | valid, eligible and Peered; also records whether it matches the prompt's rule for vague requests (lowest cost) | any |

The "providers that pass" column is enforced by a unit test (`TestShippedCatalogue_PassingSets`),
so a change to the catalogue or the rules cannot silently change what a pass means.

Each scenario can also define `referenceWeights`: a transparent weighted sum of
min-max-normalised carbon, distance and cost. The selected provider's rank on it is
reported as a yardstick, never as the right answer.

The model's own `reason` and `confidence` are saved for a human to read. They are **never**
used to judge a choice.

---

## Output

Each run writes to `results/consumerchoice/<UTC timestamp>/`:

```
configuration.json        run ID, git commit, scenarios, catalogue, model, image, options, timeouts
summary.json / summary.md verdict, metrics, per-repetition outcomes, warnings
providers.csv             the federation once: one row per provider (region, city, coordinates,
                          distance from the consumer, carbon, cost, capacity, and the rank of each),
                          and model_chose_in: the scenarios the model chose it in, as
                          "eco-oriented (3/6) - ambiguous (4/6)", out of each scenario's decisions
                          (recorded repetitions plus agent path); fallback picks do not count
federation.csv            one row per scenario × repetition × candidate, with the reservation outcome
logs/                     broker, consumer agent, grpc-server, every provider agent, Ollama
scenarios/<scenario>/
  prompt.txt              system prompt, user request, consumer location, provider list, output format
  broker_nodegroups.json  the raw Broker response of the scenario's first repetition
  rep-NN/
    model_response.json     raw response, parsed response, Ollama timings, request/response times, errors
    reservation_result.json reservation ID, request, phase transitions with timestamps, final phase, durations
  agent-path/
    agent_decision.json     the agent's own decision from its log: source (ai / fallback), ranking,
                            raw model answer, error, duration; and the provider it had to reserve
    resource_request.json   manual reservation: phases with timestamps, reserved provider,
                            criterion, release, failures, passed
```

Every profile is held fixed for the whole run and capacity returns to the baseline after every
reservation, so every decision is made on the same federation, and every repetition of a scenario
is sent the same prompt: `providers.csv` and `prompt.txt` are each written once. The run checks
that rather than assuming it: if any decision saw different provider data, or was sent a different
prompt, the summary names it in a warning.

No kubeconfig path, certificate, key or token is written, and an Ollama URL is stored without
credentials or query string.

Reservation IDs identify the scenario by position (`test-<run-id>-cc1-consumer-1-<rep>`,
`cc2`, …), not by name. The ID becomes a Kubernetes label value and part of the virtual node's
name, both limited to 63 characters, and scenario names would push it past that limit.

---

## Metrics

| Metric | Definition |
|---|---|
| Valid-ID rate | model answers that passed validation ÷ AI calls |
| Unusable answers, the model's own | rejected answers whose every failure is the model's (`timeout`, `invalid_json`, `empty_provider_id`, `unknown_provider_id`), out of the AI calls |
| Reservation success rate | reservations reaching `Peered` ÷ valid AI selections |
| Prompt-alignment rate | AI selections meeting their criterion ÷ AI selections evaluated (fallback choices excluded) |
| Fallback rate | fallback activations ÷ AI calls |
| Selected provider ranks | carbon, distance, cost, reference score |
| Decision latency | request start → validated selection, mean and p95 (nearest rank) |
| Peering latency | reservation submitted → `Peered` first observed, mean and p95 |
| Repeatability | most frequent choice ÷ AI calls, per scenario, with the distribution |
| Failure breakdown | count per category: `invalid_json`, `unknown_provider_id`, `empty_provider_id`, `timeout`, `unreachable`, `http_status`, `provider_no_capacity`, `provider_no_longer_listed`, `reservation_rejected`, `reservation_timeout`, `reservation_failed_phase`, `release_error`, `capacity_leak`, … |

A rate with no samples is reported as `n/a`, not 0%.

### Verdict and exit status

The run **passes** only if every repetition shows the whole chain working (the model was
actually asked, its answer was checked, the reservation for the resulting choice reached
`Peered`, and the capacity came back) and, with `agentPath.enabled`, every scenario's agent
path passed:

- the agent logged a decision: made by the model (`source: ai`), or by the fallback after an
  unusable answer that was the model's own doing;
- the manual reservation became `Active`;
- it landed on the first provider of the agent's ranking that had free capacity;
- it was released and the capacity came back.

The process then exits 0.

These are results about the model and do **not** fail the run; they are reported in the summary:

- a criterion miss;
- an unusable answer that is the model's own doing: it timed out (a small model can loop
  until the timeout), wrote broken JSON, or named no provider or one it was not given. The
  fallback still has to reserve, and that reservation still has to reach `Peered`.

These do fail it, because they are not about the model's judgement:

- an answer rejected for any other reason: Ollama unreachable or answering with an error, the
  Broker changing under the selection, a fallback with nothing to pick; on the agent path, a
  fallback for such a reason or with no Ollama configured;
- a model that never answered usably in the whole run, recorded decisions and agent path
  together: that points at the model, the prompt or Ollama;
- a single-candidate decision, because the model was not exercised.

---

## Safety and fallback

The model's answer is untrusted. Before any reservation, the run checks that:

1. the answer is valid JSON;
2. the selected ID (the first entry of the model's ranking) is not empty;
3. it is in the exact list sent to the model. The agent's selector skips invented IDs and would
   act on the next valid one, but that does not rescue the answer here: an invented first
   choice is an invalid answer. Invented IDs further down the ranking are recorded in
   `model_response.json` (`unknownIds`) without invalidating it;
4. the Broker still lists that provider on a fresh read;
5. it still has free capacity;
6. it carries every field a reservation needs;
7. the Broker still applies ConsumerChoice.

If any check fails, no reservation is made for that provider and `fallback.mode` decides:

| Mode | Behaviour |
|---|---|
| `agent-default` | The agent's own `DeterministicFallback`: cheapest, then lowest carbon, then most capacity |
| `cheapest-eligible` | Lowest cost per chunk, ties broken by carbon |
| `lowest-carbon-eligible` | Lowest carbon, ties broken by cost |
| `fail` | No reservation for that repetition |

A fallback is always recorded in `summary.json` (`source`, `fallbackUsed`, `failureCategories`)
and counted in the fallback rate. It is
never presented as the model's choice.

---

## Cleanup

With `cleanup: true` (default) the run releases its reservation after every repetition,
checks that capacity returned to the baseline, saves logs, deletes the Kind clusters and
removes the Ollama container. The model cache volume is kept.

With `cleanup: false` or `--keep-clusters`, everything stays up and the run prints the
`kind delete cluster …` and `docker rm -f <run-id>-ollama` commands for that run.
Reservations are still released.

---

## Limitations

- **One consumer.** The harness acts as consumer-1; extra consumers stay idle.
- **The consumer is co-located with provider-1** by construction of the harness geo
  registration. The catalogue makes that a trade-off rather than a trivial win.
- **Model size matters.** `llama3.2` (3B) may miss trade-off criteria that a larger model
  would meet. That is recorded, not hidden; the model is one config field away.
- **Proximity is the model's own estimate.** The model gets coordinates, not distances, so a
  small model may misjudge which provider is close. That shows up in the proximity and
  balanced criteria; it is measured, not corrected.
- **Static conditions.** Carbon, prices and capacity do not change during a run, so
  repetitions measure the model's consistency, not its reaction to change.
- **Phases are sampled.** Reservation phase timestamps are "first observed" at the polling
  interval, not the exact transition time.
- **Two views of the same decision.** Recorded decisions call the agent's selector from the
  harness, so everything is recorded but the agent's local API is not involved. The agent
  path goes through the local API but is known only through the agent's log line, and it
  runs once per scenario. Since sampling is not fixed, the two need not choose the same
  provider; each is checked against its own decision.
- **No Cluster Autoscaler.** The agent path is triggered by a manual reservation, which uses
  the same local API as the Cluster Autoscaler but not CA's own scale-up logic. CA stays
  disabled, as in the other suites.
- **The agent's decision cache.** The agent reuses a decision for the same request for up to
  60 s. The check pairs a reservation with the decision logged before it.
