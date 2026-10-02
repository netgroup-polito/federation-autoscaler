# Test guide

This guide explains how to run the Federation Autoscaler tests, all of which live in
`federation-tests/`: what each one measures, what the machine needs, how to launch them,
what to change to run a different experiment, and how to tell whether a run is valid. It
is written for people who do not know the project: if this is your first time, read
sections 1, 2 and 3, then the section of the test you care about.

The starting point: **everything is automated**. You do not create clusters, generate
certificates, deploy components or clean anything up by hand. A single command does the
whole round, and in the normal case the only thing you touch is a YAML file in
`federation-tests/configs/`.

---

## 1. The test suites

| Suite | What it measures | Configuration | Typical duration | Details |
|---|---|---|---|---|
| `comparative-eco` | Random versus Eco: how "green" the chosen provider is, over the same sequence of conditions | `configs/eco-*.yaml` | ~22 min for the smoke test; ~2 h 20 min with 1 h per phase and 100 agents | sections 4–12 |
| `comparative-latency` | Random versus Latency: RTT to the chosen provider | `configs/latency-*.yaml` | same as eco | sections 4–12 |
| `consumerchoice` | The ConsumerChoice policy end to end: a local LLM (Ollama) makes the choice and it becomes a real reservation | `consumerchoice/configs/*.yaml` | ~30 min with `default.yaml`; ~50 min with `all-scenarios.yaml` | section 13 and [README](consumerchoice/README.md) |
| `scalability` | How the Broker responds as the load grows: latencies, errors, CPU and RAM | `configs/scalability.yaml` | ~6–8 min per load level | section 14 and [README](scalability/README.md) |

The commands, to be run **always from the repository root**:

```bash
go run ./federation-tests/comparative-eco/     --config federation-tests/configs/eco-test.yaml
go run ./federation-tests/comparative-latency/ --config federation-tests/configs/latency-test.yaml
bash federation-tests/consumerchoice/run-consumerchoice.sh --config federation-tests/consumerchoice/configs/default.yaml
bash federation-tests/scalability/run-scalability-test.sh
```

### What is in the folder

| Path | What it is |
|---|---|
| `comparative-eco/` | Harness of the Random vs Eco test (carbon intensity) |
| `comparative-latency/` | Harness of the Random vs Latency test (RTT) |
| `consumerchoice/` | End-to-end validation of ConsumerChoice: a local LLM (Ollama) picks the provider. Has its own README |
| `configs/` | The YAML files describing the experiments — **this is where you work** |
| `testlib/` | Shared library: orchestration, deployment, clients, CSV writing |
| `scripts/` | Python scripts to analyse and verify the results |
| `mock-eco-test/`, `mock-geo-test/` | The **controllable** carbon-intensity and geolocation services. They answer like the production mocks but also expose `POST /admin/carbon` and `POST /admin/geo`, which is how the harness sets the conditions and replays the same sequence in both phases (section 4). It builds them, loads them into the central cluster and patches the mock deployments over: you never run them by hand |
| `scalability/` | Load test of the Broker API, independent of everything else. Configured in `configs/scalability.yaml` (only how many consumers and providers); has its own README |

---

## 2. Before you start

### The machine

- **Linux x86_64 (amd64).** The tests were developed and run on Ubuntu 24.04
  (section 15). Windows and macOS are not supported for the suites that create clusters:
  the deploy scripts download Linux amd64 binaries and install packages with `apt`.
- **A user with `sudo` and in the `docker` group.** The scripts install some missing
  tools on their own (see below), and Docker must work without `sudo`.
- **Internet access on the first run.** It is needed for the Docker images (Kind nodes,
  Liqo, Ollama), the Go modules, the `liqoctl`/`helm` binaries if missing and, for
  consumerchoice, the `llama3.2` model (~2 GB). Later runs reuse everything from the
  cache.
- **Resources.** In the comparative tests and in consumerchoice every agent is a Kind
  cluster, that is a Docker container running a full Kubernetes: the CPU, RAM and disk
  needed grow with the number of agents. As a reference, on the machine of section 15 the
  runs with 100 agents (30 consumers + 70 providers) went without problems; on a smaller
  machine, reduce the number of agents. The scalability test instead uses a single Kind
  cluster and is light: at 1000 simulated agents the Broker uses ~4 cores and ~150 MiB.

### Tools

| Tool | Version | Who installs it |
|---|---|---|
| Go | ≥ 1.24.5 (the one in `go.mod`) | you |
| Docker | recent, running | you |
| Kind | recent | you |
| `kubectl`, `curl`, `openssl` | recent | you (the deploy scripts would install them, but the scalability script expects them to be there already) |
| `liqoctl` | v1.1.2 | the deploy scripts, if missing |
| `helm`, `git`, `tar` | recent | the deploy scripts, if missing |
| Python 3 with `pandas` and `matplotlib` | Python ≥ 3.10 | you, only for the charts |

On Ubuntu the simplest way to get the Python packages is
`sudo apt install python3-pandas python3-matplotlib`. The alignment verification scripts
use only the standard library.

### Kernel limits (inotify)

Every Kind cluster consumes inotify instances and watches. With Ubuntu's default values,
already with about ten clusters new nodes fail to start (errors such as "too many open
files" in the Kind logs). Raise them once:

```bash
sudo sysctl fs.inotify.max_user_watches=1048576
sudo sysctl fs.inotify.max_user_instances=8192
# to keep them after a reboot:
printf 'fs.inotify.max_user_watches=1048576\nfs.inotify.max_user_instances=8192\n' \
  | sudo tee /etc/sysctl.d/99-kind.conf
```

These are the values of the machine the tests were run on (section 15).

---

## 3. The first time

A full round with the eco smoke test, about 22 minutes:

```bash
git clone <repository url> federation-autoscaler
cd federation-autoscaler

# 1. the test (the first time it builds the images: a few extra minutes)
go run ./federation-tests/comparative-eco/ --config federation-tests/configs/eco-smoke.yaml

# 2. is the run valid? (section 9)
python3 federation-tests/scripts/verifyReplayAlignment.py \
  --input results/comparative-eco/<timestamp>/nodegroups.csv

# 3. the chart
python3 federation-tests/scripts/ecoDiagramMaker.py \
  --input results/comparative-eco/<timestamp>/reservations.csv
```

`<timestamp>` is the folder the test prints at the end of the run. If something goes
wrong, section 11 collects the most common problems.

---

## 4. How a comparative test works

Both harnesses run **two consecutive phases on the same federation**:

- **Phase A — Random**: the baseline. The broker picks a provider at random.
- **Phase B — Eco / Latency**: the policy under test.

The idea is a **paired experiment**: the two phases must see the *same* environment, so
that the only difference between them is the policy. This is not an abstract
methodological detail — it is what makes it legitimate to overlay the two charts and say
"under the same conditions, Eco did better".

To achieve it the harness does two things:

1. **Sequence replay.** The generator of environmental conditions (carbon intensity in
   eco, `tc netem` delays in latency) is restarted at the beginning of each phase with the
   **same random seed**, derived from the RunID. Phase B therefore replays exactly the
   values Phase A produced, at the same instants relative to the start of its own phase.
   In eco the values reach the federation through the controllable mocks' admin API
   (`mock-eco-test/`, `mock-geo-test/`): that is what those two services are for.

2. **Symmetric waits.** Every pause before sampling is identical in the two phases. If
   one phase waited even a few seconds longer, its samples would fall on a different
   point of the replayed sequence and the alignment would break.

### What the harness generates is not what the consumers see

This is the concept you need in order to pick good config values.

When the harness writes a new carbon value, that value **does not reach** the consumer
immediately. It has to go through two stages:

1. the provider's cache must expire → up to `ecoCacheTTL`
2. the provider must republish its advertisement → up to **30 s**, hardcoded in the
   agent, not configurable

We call the sum of the two the **observation delay**. Each provider has its own offset
within that window, and that offset differs between Phase A and Phase B.

**The rule of thumb: the observation delay must be much smaller than the interval at
which the environment changes.** Otherwise, when you observe a value, you do not even
know which "round" it belongs to — and the two phases do not overlap, even though the
generated sequence was identical.

| Configuration | Delay | Interval | Ratio | Measured outcome |
|---|---|---|---|---|
| `35s` / `35s` (old) | 65 s | 35 s | 1.86 | 15.5 % match — unusable |
| `2m` / `5s` (current) | 35 s | 120 s | 0.29 | ~90 % expected |

On the latency side the chain is shorter — the consumer probes the provider directly,
without going through the advertisements — so the delay is only the prober's cache (15 s,
also hardcoded). With `latencyRefreshInterval: 2m` the ratio is 0.12.

The harness **checks these ratios at startup** and prints a warning if the delay exceeds
half of the interval. If you see it, the run will not produce overlapping charts: stop it
and fix the config.

---

## 5. Running a test

### Prerequisites

Those of section 2. At startup the harness checks `docker` (running), `kind` and `go`,
and stops immediately if something is missing; `kubectl` and `liqoctl` are used by the
deploy scripts, which install `liqoctl` on their own if missing.

### Command

```bash
go run ./federation-tests/comparative-eco/     --config federation-tests/configs/eco-test.yaml
go run ./federation-tests/comparative-latency/ --config federation-tests/configs/latency-test.yaml
```

That is all. From here on the harness works on its own, in this order:

```
PREREQUISITES → BUILD IMAGES → RETAG IMAGES → PRELOAD LIQO + UDPECHO IMAGES
→ CREATE CLUSTERS → LOAD IMAGES → DEPLOY COMPONENTS → CAP PROVIDER CAPACITY
→ WAIT FOR READINESS → [APPLY TC DELAYS] → PHASE A: Random → TRANSITION
→ PHASE B: Eco|Latency → EXPERIMENT CLEANUP → DONE → CLEANUP
```

`APPLY TC DELAYS` belongs to the latency test only: it is where the simulated delays are
installed. `CLEANUP` comes **after** `DONE` because tearing the clusters down is a
deferred step: the results are already written when it starts, so interrupting it costs
you the clusters, not the run.

It creates one Kind cluster for the broker, one for each consumer and one for each
provider, generates the PKI, deploys broker/agents/mocks, runs the two phases, writes the
results and tears everything down.

### Available flags

| Flag | Effect | When you need it |
|---|---|---|
| `--config` | Path of the YAML file | Always |
| `--skip-build` | Skips `make docker-build`, so the broker and agent images are reused | From the second run on, if you have not touched the Go code — saves several minutes. The two controllable mock images are rebuilt either way: they are quick, and the replay depends on them |
| `--keep-clusters` | Does not destroy the clusters at the end of the run | Debugging: you can go in with `kubectl` and look at what happened |
| `--run-id` | Forces the RunID instead of generating it | Very rare. **Careful**: the RunID is the replay seed, so two runs with the same `--run-id` see exactly the same sequence of conditions |

> **Careful with `--skip-build` after a code update.** The manifests are always taken from
> the repository, the images are not. The consumer manifest now passes the
> `--ollama-url/--ollama-model/--ollama-timeout` flags to the agent: an agent image built
> before this change does not know them and the agent does not start. After updating the
> repository, the first run of **any** test (eco, latency, consumerchoice) must be done
> without `--skip-build`.

### How long it takes

The duration is dominated by the two phases, plus a deployment overhead that grows with
the number of agents (measured: ~4 min with 7 providers, ~5 min with 17, ~12 min with
70).

With `timer: 1h` and 100 agents it is about **2 h 20 min**.

---

## 6. The config: what to change

In the normal case you edit **only** a file in `federation-tests/configs/`.

**The blocks below are the shipped configs, not the defaults.** They reproduce
`configs/eco-test.yaml` and `configs/latency-test.yaml`, which is what you want for the
charts. An omitted key does take a default, but the code's defaults are the conservative
ones a bare config would get, and several differ from what the experiments use:

| Key | In the shipped configs | Default if omitted |
|---|---|---|
| `mode` | `reserve` | **`observe`** |
| `duration` | `time` | **`iterations`** |
| `iterations` | 15 | 10 |
| `phasePause` | 35s | 30s |
| `policyPropagationWait` | 35s | 20s |
| `carbonRefreshInterval` / `latencyRefreshInterval` | 2m | 3m |

The first row is the one that bites: write a config from scratch, leave `mode` out, and
you get an observe run — no reservations, no `reservations.csv`, nothing to chart. Start
from a shipped config rather than from an empty file.

### Topology

```yaml
consumers: 30
providers: 70
```

These are real Kind clusters: the total is bounded by the machine's RAM and CPU, not by
the code. The more agents, the longer the deployment (section 5).

You can also pin the providers' regions with `providerRegions:` (one per provider); if
you omit the key they are drawn at random from a list of 50 real regions.

### Experiment parameters

```yaml
experiment:
  mode: reserve            # "reserve" = actually reserve (Liqo peering)
                           # "observe" = only look at who would win, do not reserve
  duration: time           # "time" = time-based | "iterations" = number of rounds
  timer: 1h                # duration of EACH phase (only with duration: time)
  iterations: 15           # rounds per phase (only with duration: iterations)
  phasePause: 35s          # pause between one iteration and the next
  policyPropagationWait: 35s
  advertisementLag: 35s
```

`duration: time` is the mode to use for the charts: each consumer runs independently for
the given time, instead of waiting for the others at every round. In this mode the
`iterations` field is **ignored** (it stays in the config only as a fallback).

### Eco parameters

```yaml
  carbonRefreshInterval: 2m   # how often the environmental conditions change
  ecoCacheTTL: 5s             # how long the provider caches the value
  carbonLow: 50               # centre of the "green" range
  carbonHigh: 800             # centre of the "dirty" range
  carbonGreenFractionMin: 0.3 # minimum fraction of green regions at each round
  carbonGreenFractionMax: 0.7 # maximum fraction
```

At every round the harness draws which fraction of regions is green and gives each one a
value around `carbonLow` or `carbonHigh`, with a ±30 % jitter.

### Latency parameters

```yaml
  latencyRefreshInterval: 2m
  latencyMinMs: 30
  latencyMaxMs: 250
```

The simulated delay is redrawn for every (consumer, provider) pair at every round. **Do
not raise `latencyMaxMs` above 250**: the prober has a 300 ms deadline per probe, and a
slower provider would be unreachable and disappear from the CSVs instead of showing up as
"far".

### Recommended values

| Key | Value | Why |
|---|---|---|
| `duration` | `time` | The charts reason on elapsed time |
| `timer` | `1h` | With 2-minute ticks it gives ~30 steps per phase. With `30m` it gives 15 and the chart comes out blocky |
| `carbonRefreshInterval` | `2m` | The minimum that keeps the observation delay below a third of the tick |
| `ecoCacheTTL` | `5s` | Any value below 30 s is equivalent; **do not tie it to the refresh interval** |
| `latencyRefreshInterval` | `2m` | Symmetry with eco; the constraint here is looser |
| `phasePause` | `35s` | Below 30 s you sample faster than the environment changes |
| `latencyMaxMs` | `250` | Ceiling set by the prober's deadline |

### The other keys

These appear in every shipped config without needing to be touched. They are listed here
so that a key you see in a YAML file is never a mystery.

| Key | Default | What it is |
|---|---|---|
| `cleanup` | `true` | Destroy the clusters at the end. `false` is the same as `--keep-clusters` |
| `output.dir` | `results` | Root the run folder is created under |
| `infra.readinessTimeout` | `10m` | How long to wait for every agent to become ready before giving up |
| `infra.liqoProvider` | `kind` | Which Liqo provider `liqoctl` installs |
| `experiment.warmupTimeout` | `5m` | Wait for the federation to settle before Phase A starts |
| `experiment.reservationPoll` | `5s` | How often a pending reservation is polled |
| `experiment.reservationTimeout` | `10m` | When a reservation that never reaches `Peered` is given up on |
| `experiment.federationSampleInterval` | `1m` | The rate `federation.csv` is sampled at |
| `experiment.tcDelays` / `consumerDelays` | — | Latency test: fixed delays instead of drawn ones, one per provider or per (consumer, provider) pair. Setting them replaces the random draw, so the replay no longer varies |

The topology is checked before anything is built: `providers` must be at least 2,
`consumers` at least 1, and `providerRegions` — when given — must have exactly one entry
per provider. With `duration: time`, `timer` must be greater than zero. A config that
breaks one of these fails at load time rather than half an hour into the deployment.

---

## 7. Changing the chunks available per provider

This is the only change that requires touching Go code, and it is the most effective
knob to make the test interesting.

In `testlib/experiment.go`, in the `=== CAP PROVIDER CAPACITY ===` block:

```go
if err := SetProviderCapacity(ctx, spec.Kubeconfig, "4000m", "8Gi"); err != nil {
```

**A standard chunk is 2 CPU / 4 GiB.** So:

| Value | Chunks per provider | Consumers that fit |
|---|---|---|
| `"2000m", "4Gi"` | 1 | 1 |
| `"4000m", "8Gi"` | 2 (default) | 2 |
| `"8000m", "16Gi"` | 4 | 4 |

**Why it matters.** Without this cap, every provider would advertise the real capacity
of its Kind node — that is, all the CPU and RAM of the host machine — and all consumers
would fit in it at the same time. The greenest provider would never fill up and everyone
would always end up there: an artefact of the test environment, not a property of the
policy.

With a low cap, instead, when the best provider fills up the policy is **forced** to move
to the second best — which is exactly the behaviour the test has to measure.

**Typical configurations.** With 1 chunk per provider and 3 consumers / 7 providers, each
consumer takes a whole provider and the three compete for the greenest ones: competition
is at its highest and the differences between the policies stand out very clearly. This
is the configuration to use to show the effect in the most readable way.

Raising the number of consumers with the same chunks increases the pressure; raising the
chunks reduces it. As a rule: **consumers ≈ providers × chunks** means a saturated
federation, where the policy spends its time looking for room. Far fewer means an empty
federation, where every policy always finds its favourite free.

---

## 8. What a run produces

The files end up in `results/<test-type>/<UTC timestamp>/`, for example
`results/comparative-eco/20260912T015958Z/`.

| File | Content |
|---|---|
| `summary.md` / `summary.json` | Readable summary: durations, number of iterations, distribution of choices, mean metric per phase |
| `reservations.csv` | Reserve mode only: one row per reservation — who, where, when, outcome, metric and carbon intensity of the chosen provider |
| `selections.csv` | One row per placement decision, even when it does not lead to a reservation |
| `nodegroups.csv` | **Eco test only, and the richest file**: one row for every time a consumer looked at a provider, with the value it saw at that moment. It is the basis for verifying the alignment and for the chart's shaded range |
| `federation.csv` | Reserve mode only: snapshot of the whole federation every `experiment.federationSampleInterval` (1 min by default), independent of the iteration pace |
| `probes.csv` | Latency test only: the measured RTTs. It plays for latency the role `nodegroups.csv` plays for eco — alignment check and shaded range |

The `phase` field (`phase-a` / `phase-b`) is present everywhere and is the key to separate
the two phases in the analysis.

The two suites do not produce the same set: eco writes `nodegroups.csv` and latency
writes `probes.csv`, never both. That is why the next section has one script per suite,
and why the chart scripts read a different second file in each case.

---

## 9. Checking that the run is valid

**Do it before looking at the charts.** A run can complete without errors and still be
unusable for the comparison, if the two phases did not see the same environment.

**For the eco test**, from its `nodegroups.csv`:

```bash
python3 federation-tests/scripts/verifyReplayAlignment.py --input results/.../nodegroups.csv
```

The script groups the observations by round, compares what each provider showed in
Phase A against Phase B at the same elapsed time, and prints the match percentage
together with a **chance baseline** (obtained by shuffling the provider labels). The
baseline matters: a percentage alone cannot be read, because similar values also happen
by luck.

| Outcome | Threshold | Exit code | What to do |
|---|---|---|---|
| PASS | ≥ 80 % | 0 | Go ahead |
| INCONCLUSIVE | 50–80 % | 2 | Usually too few rounds. Look at the ratio to the baseline |
| FAIL | < 50 % | 1 | Do not overlay the charts. Check the observation delay |

If you changed `carbonRefreshInterval`, also pass `--tick-seconds` with the new value in
seconds, otherwise the script groups with the wrong window. **The same applies to the
latency script below and `latencyRefreshInterval`**: both default to 120 s, which is the
value every shipped config uses, and neither can tell that you changed it.

### The latency test uses a different script

```bash
python3 federation-tests/scripts/verifyLatencyReplayAlignment.py --input results/.../probes.csv
```

It reads `probes.csv`, not `nodegroups.csv`, and reasons differently for two reasons.

The first: here the value is a **measured RTT**, not an exact number read from an API.
The comparison therefore uses a tolerance (`--tolerance-ms`, default 10). The question it
answers is "did the two phases see comparable conditions?", not "do they match to the
millisecond": 10 ms against 13 ms is fine, 10 ms against 400 ms is not. On real data the
distinction is sharp — readings of the same condition are ~0.1 ms apart, different draws
tens or hundreds of ms — so the exact tolerance value is not critical.

The second: **latency's Phase A is structurally sparse**. Under Random the broker masks
down to a single provider, so a (consumer, provider) pair is measured only when chance
picks it — on a one-hour 3×7 run that was 7 windows out of 30, against 28 out of 30 in
Phase B. That is why the output reports a **Coverage** line: always read the percentage
together with the number of comparable windows, because a high percentage on very few
cells is worth little.

In the output you also find:

- **Estimated refresh grid** — the script does not assume where the tick boundaries fall,
  it derives them from the observed value changes (all pairs change together, on a single
  ticker). If a phase is too sparse to find them on its own it borrows the other phase's
  estimate, and says so. If a warning about the grid's reliability appears, the rest of
  the result cannot be interpreted.
- **allowing +/-1 window** — how much of the mismatch is only uncertainty about the
  window boundary rather than a different environment. If this number is much higher than
  the strict one, the problem is positioning, not conditions.

If the verdict is FAIL or a stubborn INCONCLUSIVE, look at the raw values before
concluding anything:

```bash
python3 federation-tests/scripts/dumpLatencySequence.py --input results/.../probes.csv \
  --consumer consumer-1 --provider provider-3
```

It prints the two phases side by side, window by window. In a few seconds you see whether
the columns look alike (replay fine, the score is measuring sample scarcity) or are
unrelated numbers (then the replay is what to look at).

### Charts

One chart per test, built the same way: **direct overlay**, with Random (red) and the
policy under test (green) on the same X axis, each measured from the start of its own
phase.

```bash
python3 federation-tests/scripts/ecoDiagramMaker.py     --input results/comparative-eco/<timestamp>/reservations.csv
python3 federation-tests/scripts/latencyDiagramMaker.py --input results/comparative-latency/<timestamp>/reservations.csv
```

- `ecoDiagramMaker.py` — Y axis: **sum** of the carbon intensities of the chosen
  providers. Output `carbon_intensity_comparison.*` and `carbon_summary.md`.
- `ecoDiagramMakerLog.py` — the same chart with a **logarithmic** Y axis, written with an
  `_log` suffix so it sits beside the linear one. Under Eco the federation runs an order of
  magnitude cleaner than under Random, so on a linear axis Phase B is a flat line on the
  bottom and its own variation cannot be read. Same options, same numbers: it is
  `ecoDiagramMaker.py --log-scale` under its own name. The first minute of a phase, when
  only part of the consumers have peered, falls below the frame; the summary says how many
  points that is.
- `latencyDiagramMaker.py` — Y axis: **mean RTT** to the chosen provider, across the
  active consumers. Output `latency_comparison.*` and `latency_summary.md`.

The Y axes differ on purpose: a sum of milliseconds has no physical meaning and would
grow with the number of consumers, while the mean stays in real ms and on the same scale
from 3×7 to 30×70.

Both take `--input` and `--output-dir` (default: an `analysis/` folder next to the CSV)
and write 300 DPI PNG, PDF, CSV, a markdown summary and a **`.tex` to paste into the
thesis** (see below). If you pass the wrong CSV they exit with a message naming the right
script. They need `pandas` and `matplotlib`; the verification scripts use only the
standard library.

#### The `.tex` beside the PNG

Every chart is also written as `pgfplots` source, so a chapter can carry the vector figure
itself instead of a bitmap of it. The preamble needs

```latex
\usepackage{pgfplots}
\pgfplotsset{compat=1.18}
```

and nothing else — `\usepgfplotslibrary{statistics}` on top of that for the boxplot. Each
file opens with those lines as a comment, plus the `figure` block to include it with
`\input`. Without them LaTeX does not know `tikzpicture` and typesets the file as text
rather than drawing it, which looks like a broken figure instead of a missing package.

The code is generated from the numbers, not converted from the matplotlib figure: a step
curve is `const plot` (the pgfplots equivalent of matplotlib's `where="post"` — a value
holds until the next point, which is what the data means), and a box is `boxplot
prepared`. That is a handful of readable lines each, editable by hand — size, font,
colours — instead of a machine-made dump. (`tikzplotlib`, which does the conversion, has
been archived since 2023 and does not work with current matplotlib.)

The shaded range is **one closed outline** — up along the ceiling, back along the floor,
a vertex per step corner — rather than two paths and a `fill between`. The staircase is
already ours to describe, so writing it out costs nothing and spares the preamble the
`fillbetween` library: a preamble missing it is exactly how the figure ends up with its
dashed edges drawn and the fill silently absent. A long Y label is broken over two lines
(`\\` plus `ylabel style={align=center}`) so it does not steal width from the plot.

The shared code lives in `pgfplotsWriter.py`, and the `.tex` is written from the same
objects that drew the PNG, so the two cannot drift apart. A run without
`nodegroups.csv`/`probes.csv` simply gets a file with the two curves and no range.

#### The shaded range behind the curves

The eco chart shades the space the federation left open, so a curve can be read against
what was possible and not only against the other policy. At every point the band runs
from the best case to the worst case for the consumers placing then: with 3 consumers,
the sum of the 3 greenest providers and the sum of the 3 dirtiest. The summary says how
much of that range each policy captured — 100% means it matched the best available, 0%
the worst.

Two details make the band trustworthy, and both are read from the data:

- **The curve is what you are holding now.** It is the carbon intensity the chosen
  providers carry at that minute, taken from `nodegroups.csv`, not the value recorded
  when each consumer reserved. Holding a provider that has turned dirty counts as dirty,
  and curve and band read the same snapshot — which is what makes floor ≤ curve ≤ ceiling
  hold everywhere. The old value stays in the CSV as `aggregate_at_choice`.
- **Consumers per provider come from `max_size`.** If a provider can host two consumers,
  the best case puts two on the greenest one, so it counts twice in the floor. Assuming
  one when it was two lifts the floor above the Eco curve — it is the single number the
  band is most sensitive to. `--chunks-per-provider N` overrides the detection.

**The latency chart has one too, read differently.** The simulated delays are never written
to a CSV, so its range can only be built from the RTTs the consumers measured — one
provider per iteration under Random, the three nearest under Latency. The floor is
therefore the fastest anybody happened to measure, at or above the true best, so it
understates the margin rather than flattering the policy; the summary prints the coverage
(measured pairs out of consumer × provider: 77% at 3×7, 13% at 30×70).

What makes it trustworthy is the grouping. The delay of a pair is redrawn once per
`latencyRefreshInterval` and constant in between, so probes are pooled per refresh window
and every value the curve averages is in the pool its edges come from — the curve cannot
fall outside. The window boundaries are estimated from the data, not assumed: the refresh
ticker starts after the policy wait, so windows nailed to minute zero would straddle two
draws. Under Random a pair is probed once and dropped, so that phase borrows the policy
phase's estimate and the summary says so.

If the second file is missing the chart is drawn exactly as before, with a note in the
summary: older result folders keep working.

#### Every flag of every script

Each script also answers `--help`. This table is the same information, in one place.

| Flag | Scripts | Default | What it does |
|---|---|---|---|
| `--input PATH` | all | required | The CSV to read: `reservations.csv` for the charts, `nodegroups.csv` / `probes.csv` for the verifiers |
| `--output-dir PATH` | eco, latency, boxplot | an `analysis/` folder beside the input | Where the figures are written |
| `--nodegroups PATH` / `--probes PATH` | eco / latency | next to `--input` | Read the second file from somewhere else |
| `--chunks-per-provider N` | eco, boxplot | read from `nodegroups.csv` | Override how many consumers one provider can host |
| `--range-window-minutes N` | latency, boxplot | `2.0` | The refresh window the probes are grouped into — the `latencyRefreshInterval` of every shipped config |
| `--no-range` | eco, latency | off | Only the two curves, and the curve stays the value recorded at reservation time |
| `--log-scale` | eco, boxplot | off | Logarithmic Y axis, written with an `_log` suffix beside the linear one. The latency chart has none: its two phases sit on the same order of magnitude |
| `--grid regular\|events` | eco, latency | `regular` | How each phase's timeline is built. `regular` resamples onto an evenly spaced grid; `events` uses every distinct event timestamp instead, giving the exact step function at the cost of a busier chart |
| `--grid-minutes N` | eco, latency, boxplot | `1.0` | The grid spacing, with `--grid regular` |
| `--labels A,B,…` | boxplot | the topology read from the data | One label per run, in place of `3x7`, `8x17`, … |
| `--tick-seconds N` | verifiers, dump | `120.0` | The refresh interval of the run, in seconds. Change it whenever you changed `carbonRefreshInterval` or `latencyRefreshInterval` |
| `--tolerance-ms N` | latency verifier | `10.0` | How far two RTT readings may differ and still count as the same conditions |
| `--windows N` | verifiers | `6` | Minimum comparable windows before a verdict is trusted |
| `--shuffles N` | verifiers | `200` | Shuffles used to build the chance baseline |
| `--max-bracket-seconds N` | latency verifier | `45.0` | How far from a value change the refresh boundary may be estimated |
| `--consumer ID` / `--provider ID` | dump | required | The pair whose sequence to print |

`ecoDiagramMakerLog.py` takes exactly the flags of `ecoDiagramMaker.py`: it is that script
with `--log-scale` already set.

#### All the runs in one figure

`phaseBoxplotMaker.py` puts several runs side by side instead of one per chart: one box per
phase per scale, so it is visible at a glance whether the policy's advantage survives as
the federation grows, and how spread out each phase was.

```bash
python3 federation-tests/scripts/phaseBoxplotMaker.py   results/comparative-eco/<run-3x7> results/comparative-eco/<run-8x17>   results/comparative-eco/<run-15x35> results/comparative-eco/<run-30x70>
```

Give it run folders of one suite (it tells eco from latency by reading the data, and refuses
a mix) and it writes `eco_phase_boxplot.*` or `latency_phase_boxplot.*` into an `analysis/`
folder beside them. Values are **per consumer** — carbon intensity per active consumer, mean
RTT — because a sum grows with the scale and would flatten the small runs. The labels
(`3x7`, `8x17`, …) come from the data, and `--labels` overrides them.

Each box shows the median, the quartiles and whiskers at 1.5 IQR; there is no mean marker, because under the policy the distribution is skewed enough that the mean lands above the box and reads as a mistake (the mean is in the CSV and the summary). `--log-scale` writes an `_log` version beside the linear one, worth it for eco where the two phases sit an order of magnitude apart.

It takes the band's flags too — `--chunks-per-provider` for eco, `--range-window-minutes`
for latency — and they mean exactly what they mean for the charts, because the third box
is built from the same range. The full list is in the flag table above.

Each scale gets a third box, in sand, for the best the federation offered at those same moments —
"Achievable minimum" for eco, where every provider's reading is known, "Measured minimum" for
latency, where only the probed pairs are. It is what turns the figure into a statement about how
much room a policy left on the table, and the summary prints that distance as a multiple of the
floor (on these runs: Eco 1.00–1.01×, Latency 1.6–2.0×, Random 2.9–10.9×).

Besides the PNG and PDF the script writes a **`.tex` to paste into the thesis**, like the two
chart scripts: one `\addplot` per box with `boxplot prepared`, colours declared with
`\definecolor` and the scale separators as extra ticks. This is the one figure that wants a
pgfplots library in the preamble, `\usepgfplotslibrary{statistics}`; the rest is as described
above.

The series are not recomputed here: the script imports the pipeline of the two chart scripts,
so a box and the curve of the same run always agree. Alongside the figure it writes a CSV with
n, min, Q1, median, Q3, max and mean per box, and a summary with the gap between the two
medians at each scale.

---

## 10. Quick test before a long run

Before committing two hours, it pays to do a short run that uses **the same real cadence**
but 6-minute phases and a reduced topology:

```bash
go run ./federation-tests/comparative-eco/ --config federation-tests/configs/eco-smoke.yaml
```

~22 minutes. It checks that the alignment is there, not to draw conclusions about the
policy: with only 3 rounds per phase the verdict will often be `INCONCLUSIVE`, and in that
case what counts is the **ratio to the chance baseline**, not the absolute percentage.

---

## 11. When something goes wrong

**The run stops with "phase A leaked capacity".** During Phase A a release failed and the
broker still considers that slot taken. Phase B would run on a smaller federation than
Phase A, so the harness stops instead of producing an invalid comparison. The message
says which providers are involved. Run it again.

**Warning at startup about the observation delay.** The config puts the delay above half
of the refresh interval: the charts will not overlap. Lower `ecoCacheTTL` or raise
`carbonRefreshInterval`.

**Clusters left behind after an interruption.** If you stopped the run with Ctrl-C or
used `--keep-clusters`, the Kind clusters remain. List them with `kind get clusters` and
delete them with `kind delete cluster --name <name>`.

**The deployment times out.** Raise `infra.readinessTimeout` (default 10 min). On a busy
machine or with many agents it can take longer.

**The image build fails in `go mod download`** with a DNS error
(`lookup proxy.golang.org … i/o timeout`). On some servers Docker containers cannot
resolve names while the host can. Usually you do not notice, because the build reuses the
modules already downloaded; the problem shows up when `go.mod` changes. Run again,
building with the host network:

```bash
DOCKER_BUILD_FLAGS=--network=host go run ./federation-tests/comparative-eco/ --config federation-tests/configs/eco-test.yaml
```

This applies to all tests, consumerchoice included. For the same reason, do not run
`go mod tidy` without a real reason: even just reordering `go.mod` invalidates that cache.

---

## 12. Things worth knowing before interpreting the results

**The two phases run a different number of iterations.** This is normal, not a bug. In
Phase A the Random policy changes provider almost every round, and every change means a
release plus a new peering — tens of seconds. In Phase B the policy often stays where it
is, so a round closes in a few milliseconds and more of them fit in the same time. The
chart scripts account for this: they resample on a regular time grid, so they average
over time and not per row. **The per-row averages in `summary.md` do not**: they are
weighted differently in the two phases, so do not use them for the final comparison.

**The alignment is good, not perfect.** With the ratio at 0.29 about 10 % of the points
still fall on different rounds in the two phases. Near the boundaries between one round
and the next the two curves can diverge: this is expected.

**The final number moves from one run to the next.** With ~30 conditions per phase, the
mean of the Random phase is estimated on a limited sample. The order of magnitude of the
improvement is stable; the exact figure is not.

---

## 13. The ConsumerChoice test

`federation-tests/consumerchoice/` is not a two-phase comparison: it checks that the
ConsumerChoice policy works end to end. The Broker hands the consumer **all** the
eligible providers, a local LLM picks one based on a natural-language request, and that
choice becomes a real reservation that reaches `Peered`.

For each scenario the test does two things: the **recorded decisions** (the harness calls
the agent's selector and saves everything: prompt, response, validation) and the **agent
path** (a manual reservation from the console, which the consumer agent decides on its
own by asking the model). The model is queried exactly as the agent queries it: plain
JSON, Ollama's default parameters, same timeout.

Here too everything is automatic, **Ollama included**: the test starts its container,
downloads the model the first time (then it stays cached) and removes it at the end.

```bash
bash federation-tests/consumerchoice/run-consumerchoice.sh --config federation-tests/consumerchoice/configs/default.yaml
```

In the YAML you usually change only `scenarios[].userRequest`. Scenarios, evaluation
criteria, metrics and output files are described in
`federation-tests/consumerchoice/README.md`.

Besides the JSON and CSV artifacts, the run writes `reasoning.txt`: every decision as three
lines — the prompt, the provider chosen, and the model's own account of why it chose it —
separated by `-------`. It is the file to read when the question is *why*, and it is the
model's account, never the evidence a choice was right; that is what the criteria in
`summary.md` are for.

Two practical differences from eco and latency:

- the standard topology is **1 consumer and 9 providers**, with fixed profiles (carbon,
  price, capacity, region) chosen so that no provider wins on everything;
- the first run after touching Broker or agent code must **not** use `--skip-build`.

The run script passes `--keep-clusters`, `--skip-build` and `--run-id` through to the
harness, exactly as the comparative tests take them (section 5). Its own outputs, and the
`consumerChoice:` config keys, are in the suite's README.

---

## 14. The scalability test

`federation-tests/scalability/` measures how the Broker responds as the load grows. No
real agents, provider clusters or Liqo: a program simulates many consumers and providers
talking to a single Broker, with the agents' real certificates and HTTP client and the
same intervals (advertisements, heartbeats, instruction polling, `/nodegroups`
requests). It measures latencies, errors and throughput of every endpoint, plus the
Broker's CPU and RAM.

The only thing to change is `federation-tests/configs/scalability.yaml`:

```yaml
consumers: 5
providers: 5
```

Then:

```bash
bash federation-tests/scalability/run-scalability-test.sh
```

Everything else is fixed in the script (5 minutes of measurement, intervals, timeouts),
so only the load changes. The file accepts only those two keys: any other one is an
error. To point it at a different file, pass `--config FILE`; `--keep-cluster` leaves the
Kind cluster up between runs.

Besides `summary.md` the run writes `summary.json`, `configuration.json`, the raw
per-request CSVs (`raw_evaluations.csv`, `raw_provider_requests.csv`,
`raw_consumer_requests.csv`), `broker_resource_usage.csv` and the logs. They are
described in [scalability/README.md](scalability/README.md).

- **For the curve**, do one run per scale, changing the two numbers (for example 5, 10,
  25, 50, 100 per type). With `--keep-cluster` the Kind cluster stays between runs; at
  the end, `kind delete cluster --name scaltest`.
- **The Broker's CPU and RAM** are measured only on Linux: elsewhere the script says so
  and goes on without them.
- **Results** in `results/scalability/<date-time>/`. In `summary.md` check that errors and
  the Cancelled column are 0, that the CPU/RAM samples are there and that the attempts
  are roughly the expected ones (about 60 evaluations per consumer).

The details (what counts as an error, retries, the measured policy) are in
`federation-tests/scalability/README.md`.

---

## 15. Environment the tests were run on

All the results reported in the thesis (comparative eco and latency, ConsumerChoice,
scalability) were obtained on a single dedicated Linux machine, one suite at a time:

| Item | Value |
|---|---|
| Operating system | Ubuntu 24.04.4 LTS, kernel 6.8.0-138-generic, x86_64 |
| CPU | 2 × Intel Xeon Gold 6442Y, 24 cores per socket, 2 threads per core: 48 cores, 96 threads |
| RAM | 503 GiB, no swap |
| Disk | 1.5 TB, `/` (also holds `/var/lib/docker`) |
| Limits | `fs.inotify.max_user_watches = 1048576`, `fs.inotify.max_user_instances = 8192`, `ulimit -n = 65536` |
| Docker | 29.7.2 |
| Kind | v0.33.0-alpha |
| kubectl | v1.36.4 |
| Go | 1.24.5 |
| liqoctl | v1.1.2 |
| Python | 3.12.3 |

**What this means for the results.** In the scalability test, at 1000 simulated agents
the Broker used ~4.1 cores on average: about 4% of the machine's 96 threads. The measured
times therefore depend on the Broker's own work, not on a machine at its limit. On a
different machine the absolute values (latencies, CPU) change; the trends as the load
grows are what should be compared.

### How to get the same data on your machine

To compare your machine with this one, or to record where you ran the tests, run:

```bash
{
echo "## os";      grep PRETTY_NAME /etc/os-release; uname -r; uname -m
echo "## cpu";     lscpu | grep -E '^Model name|^CPU\(s\)|^Thread|^Core|^Socket'; nproc
echo "## memory";  free -h
echo "## disk";    df -h / /var/lib/docker 2>/dev/null
echo "## limits";  sysctl fs.inotify.max_user_watches fs.inotify.max_user_instances; ulimit -n
echo "## tools";   docker version --format 'docker {{.Server.Version}}'; kind version
                   kubectl version --client 2>/dev/null | head -1; go version
                   liqoctl version --client 2>/dev/null | head -1; python3 --version
} 2>&1 | tee server-env.txt
```

What to look at:

- **CPU:** `CPU(s)` is the number of threads; `Core(s) per socket` × `Socket(s)` is the
  number of physical cores. `nproc` tells how many threads your user can use.
- **RAM:** the `total` column of `free -h`; `available` is what is actually free at that
  moment.
- **Disk:** `Avail` of `df -h` on the partition holding `/var/lib/docker`, where images
  and Kind nodes end up.
- **Limits:** the two inotify values must be at least those of section 2.

During a run, to see how much the test consumes on the machine: `htop` (CPU and RAM),
`docker stats` (per container, that is per Kind cluster) and `df -h` (disk).
