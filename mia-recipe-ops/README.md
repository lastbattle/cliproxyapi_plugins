# Mia recipe operations for two DGX Sparks

A reusable operations overlay for [MiaAI-Lab's GLM TensorFold recipe](https://github.com/MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks-TensorFold): bounded admission, coordinated recovery, progress monitoring, and diagnostics captured before recovery. It runs beside the recipe on the head node. It is **not a CLIProxyAPI native plugin** and does not change inference code.

## What differs from stock Mia?

Comparison baseline: Mia `main` commit [`3fd60dfc836fab0f1ed0fd4f0afaf2a1d62b2aab`](https://github.com/MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks-TensorFold/tree/3fd60dfc836fab0f1ed0fd4f0afaf2a1d62b2aab), checked against upstream main on 2026-10-23 UTC. TensorFold `v0.6.0`, recipe patch hash `e66c657a433d`. Hardware profile: **two DGX Sparks with 128 GB unified memory each**. These are deployment settings and an external supervisor; all model-engine patches remain upstream Mia's.

| Setting or behavior | Stock recipe at this revision | Deployed profile / overlay |
| --- | --- | --- |
| Active concurrency | 4 with DFlash2, TP=2 | 4 with DFlash2, TP=2 |
| Per-request prompt + reply window | 1,048,576 tokens with FP8 KV; startup can reduce it to fit | 786,432 tokens |
| Default reply limit | 32,768 tokens | 32,768 tokens |
| Host memory reserve | 14.5 GiB at 4 streams / 32-row verify window | 21 GiB |
| Extra shared pool budget, `KV_POOL_GIB` | Up to 12.5 GiB per rank, subject to startup memory | Up to 8 GiB per rank |
| Waiting requests, `MAX_QUEUED` | Unset: unbounded queue | 6 waiting, beyond 4 active; further arrivals receive 429 + Retry-After |
| Kept-state byte cap | Disabled (`TF_GLM_KEPT_BYTES_GIB=0`) | 2 GiB |
| Own turn-boundary states per chat | Unlimited (`TF_GLM_KEEP_PER_CHAT=0`) | 2; shared states are accounted separately |
| Effort line at prompt tail | Opt-in, disabled | `TF_GLM_EFFORT_TAIL=1` |
| Loop guard | Available in the recipe | Explicitly opted in: `TF_GLM_LOOP_GUARD=1` |
| Native per-step watchdog | Stack report after 300 s; exit disabled | Report after 180 s and exit enabled |
| External recovery | Recipe provides start/stop and foreground mode | systemd supervises the external watchdog and restarts both ranks |
| Evidence before external recovery | Rank logs / native stack report | Health, metrics, history, both nodes' memory/GPU/container/kernel/network state and rank logs |
| Client disconnect cancellation | **Already stock Mia** (patch 0073 and related patches) | Retained and verified through CLIProxyAPI |

`ABLIT=1` is also selected on the deployed cluster. That selects different model weights and the recipe's corresponding thinking default; it is an existing Mia option, not an operations-layer modification. This package deliberately leaves model selection, model revisions, HF credentials, networking, and quantization choices in your existing recipe configuration.

### Memory and queue limits

This profile is an observed operating point, **not a guarantee that four full-size contexts fit simultaneously**. The shared pool was 1,150,976 token rows after the latest restart; four full windows would require 3,145,728 rows before considering sharing or eviction. Pool size varies with startup memory and kept-state reservations. The scheduler may wait for memory even with an execution lane free.

`KV_POOL_GIB` is a budget cap, not a dedicated block guaranteed to reach that size. The host reserve reduces the memory budget available at startup; it is not a separate allocation or hard runtime RAM limit. Runtime work can consume part of the reserve. These machines use unified CPU/GPU memory, so queue RAM competes with the model too.

For the current model and cache settings, token-dependent storage costs about 6,748 bytes per row, so each additional GiB of pool holds roughly 159,120 rows once fixed-state reservations are met. `KV_POOL_GIB` is budget beyond the baseline context allocation and partly holds fixed states, so its cap does not translate into a direct multiple of that figure.

Waiting requests retain their request body, token lists, connection and server/proxy bookkeeping. They generally do not get an active lane's GPU KV cache until admission, but prompt parsing/tokenization and optional stored-state reads can happen earlier. Six waiting requests smooth bursts; they do not increase throughput. Many large prompts can still use material RAM, and long queues can exceed client/proxy timeouts.

Idle kept prompts can occupy pool rows. They are reusable cache and can be evicted for new work; an almost-full pool alone is not proof of an OOM or a deadlock. Even with these safeguards, a stall can recur; automatic recovery reduces its duration, not its underlying probability to zero.

## Files

- [`recipe-overrides.example.sh`](recipe-overrides.example.sh): the nonsecret deployment profile to merge into a recipe's existing `scripts/local.sh`.
- [`watchdog.py`](watchdog.py): Python standard-library supervisor; configurable API, model, SSH worker, container, metric and time limits.
- [`supervise.sh`](supervise.sh): runs the recipe's coordinated `start.sh restart`, then the watchdog.
- [`render-service.py`](render-service.py): stages a systemd service and runtime files without modifying or restarting a deployment.
- [`verify-cancellation.py`](verify-cancellation.py): opt-in live check through CLIProxyAPI for plain chat, streaming chat and streaming Responses requests.
- [`clock-limits.md`](clock-limits.md): CPU/GPU clock caps deployed on both DGX Sparks, boot persistence, verification and rollback.

### CPU and GPU clock limits

Both nodes use a **2,808 MHz CPU maximum on all 20 cores** and a **2,200 MHz GPU graphics-clock maximum**, applied live and persisted with systemd. These are host settings, separate from `scripts/local.sh`; GLM did not need a restart. See [clock-limits.md](clock-limits.md) for the configuration and verification details. Lower clocks can reduce heat, but this deployment has not measured their throughput or temperature benefit under comparable loads.

## Install on the head node

Requirements: Linux/systemd, Python 3.10+, Bash, Docker access for the service account, `nvidia-smi`, `journalctl`, `ss`, and key-based noninteractive SSH from the head to the worker. The service account needs permission to read kernel/service logs for complete diagnostics. API health, metrics and completion probes assume a trusted local TensorFold endpoint without API authentication; adapt the HTTP helper if your recipe requires authentication.

1. Keep the Mia checkout intact and retain its working network/model settings. Merge the sample profile into `scripts/local.sh`; do not overwrite an existing file containing credentials. Retain `WORKER`, `FABRIC_PEER`, model selection and tokens privately. Changing a recipe setting requires a coordinated restart.
2. On the head, stage runtime files as the account that runs the recipe. Substitute your actual checkout path and worker SSH address:

   ```bash
   python3 mia-recipe-ops/render-service.py \
     --recipe-dir "$HOME/GLM-5.3-Flash-EXL3-2x-DGX-Sparks-TensorFold" \
     --worker user@192.0.2.2 \
     --output-dir "$HOME/.local/share/mia-recipe-ops"
   ```

   Use `--api-base`, `--model`, `--container`, `--service` and `--user` for other deployments. The output directory is the permanent runtime location; keep it after installation. The renderer refuses to overwrite existing runtime files. Back them up and choose a new directory when upgrading.
3. Inspect the generated `runtime.env` and `mia-recipe.service`. Confirm `start.sh restart` waits until this recipe is actually ready; otherwise add a readiness wait to `supervise.sh` before enabling recovery. Do not run two supervisors for the same containers. If migrating the earlier `glm53-exl3.service`, stop and disable it in the maintenance window first:

   ```bash
   # Only when replacing that existing service; this stops current requests.
   sudo systemctl disable --now glm53-exl3.service
   ```

4. Install and enable the generated service when ready for a restart. This starts/restarts both ranks and can interrupt active requests:

   ```bash
   sudo install -m 644 "$HOME/.local/share/mia-recipe-ops/mia-recipe.service" /etc/systemd/system/mia-recipe.service
   sudo systemctl daemon-reload
   sudo systemctl enable --now mia-recipe.service
   journalctl -u mia-recipe.service -f
   ```

The account must own the runtime directory and be able to read the recipe checkout. Docker-group access is privileged host access; use the same trusted service account already operating the recipe.

### Recovery behavior

- Every 20 s: inspect both containers and read `/health` and `/metrics`.
- Three consecutive health/container/probe failures: capture diagnostics, exit with failure, and let systemd restart the coordinated supervisor after 60 s.
- During work: compare live per-stream KV usage, prefill chunks/rows, stream state and request/completion counters. **Completed-only counters are not live decode progress.**
- No observed progress: allow 240 s during decode or 1,800 s during prefill. A single scheduler iteration of 210 s also triggers recovery. The native 180 s **per-step** watchdog can exit first; 30 minutes of total prefill is different from one frozen step.
- Every five minutes while idle: submit a tiny real completion, with a 15 s timeout. A 429 race is skipped. Any response containing choices counts as a successful inference probe; this does not test answer quality.
- systemd allows at most three starts in 30 minutes. After fixing a repeated startup failure, use `sudo systemctl reset-failed mia-recipe.service` followed by `sudo systemctl start mia-recipe.service`.

An external detector's recovery captures evidence before it exits. A native watchdog exit, OOM kill or host crash can happen first, so pre-failure stack capture is not guaranteed; the recipe's stop/start logs and rotating telemetry still help. A host/network outage cannot be repaired by this service while the head is unreachable.

Optional `MIA_STACK_SIGNAL=1` sends SIGUSR1 to both containers during capture. **Leave it disabled for new images until their nonfatal stack handler is verified.** The deployed recipe's head handler was verified; the worker handler can be overwritten by CUDA/LLVM, so worker manual stack dumps are not guaranteed. Native per-rank timer dumps are independent.

Diagnostics live under the configured `MIA_STATE_DIR/recovery/`; telemetry rotates at about 8 MiB with one previous file. Recovery folders are retained until manually pruned. Full Docker environments and HTTP request bodies are not collected by this overlay, but engine logs can still contain sensitive information: review bundles before sharing.

Manual capture without an intentional restart (stack signals remain opt-in):

```bash
sudo systemd-run --wait --pipe --collect --uid "$(id -un)" \
  --property="EnvironmentFile=$HOME/.local/share/mia-recipe-ops/runtime.env" \
  /usr/bin/python3 "$HOME/.local/share/mia-recipe-ops/watchdog.py" --capture-only
```

## Verify CLIProxyAPI cancellation

The installed local CLIProxyAPI **v8.0.21** was checked end-to-end on 2026-10-23 UTC. With all four GPU lanes occupied, disconnecting a queued proxy request removed its upstream request in about **0.25 seconds**, without additional GPU prefill chunks. Plain and streaming `/v1/chat/completions` and streaming `/v1/responses` passed. All six waiting requests also completed successfully in a separate admission check; the eleventh simultaneous request received HTTP 429.

That result applies to the tested build and routes. Repeat after proxy/plugin/recipe changes. A dead network peer that has not produced a TCP close/reset can take longer to detect than an explicit client disconnect. Cancellation cannot undo a GPU kernel already in flight.

The verifier below creates four long-running synthetic streams, queues one request at a time through CPA, disconnects it, and verifies removal while all four lanes remain active. It checks actual prefill chunk counters; `prompt_tokens_total` can include cancelled request accounting and must not be treated as GPU work. It skips if user traffic is already active; run it in a quiet maintenance window because requests arriving afterward can invalidate results. It closes synthetic streams in cleanup.

```bash
export CPA_API_KEY='your-local-proxy-key'  # do not commit it
python3 mia-recipe-ops/verify-cancellation.py \
  --backend http://127.0.0.1:8888 \
  --proxy http://127.0.0.1:8317 \
  --backend-model GLM-5.3-Flash-EXL3 \
  --proxy-model glm-5.3-flash-exl3
unset CPA_API_KEY
```

From another host, expose the backend through an SSH local forward if necessary. Backend and proxy do not have to be on the same machine. Configure access to the trusted backend separately if it requires authentication.

## Adapt to a future Mia recipe

1. Pin and record its recipe commit, image digest, model revision and hardware. Compare that revision's own `scripts/config.sh` and `/health` schema before carrying settings over.
2. Confirm it supports these knobs. `MAX_QUEUED` is the recipe setting exported as `TF_GLM_MAX_QUEUED`; native watchdog exit, kept-state caps, effort tail and loop guard are engine/recipe-specific features.
3. Keep model/quantization/network settings in the new recipe. Update runtime API/model/container/worker and the supervisor's start/stop commands as needed. This package currently targets **two nodes**, not arbitrary distributed topologies.
4. Check that `tensorfold:kv_cache_usage_ratio` changes during live decode and that `multi_prefill` counters represent actual chunk work. Update `MIA_LIVE_METRIC_PREFIX` or the progress signature for a new engine. If live metrics disappear, the elapsed-progress check skips recovery for that sample and logs a warning; health, idle probes and iteration checks remain active. `MIA_PROGRESS_CHECK=0` disables progress/iteration checks while adapting a schema.
5. Recalculate reserve and pool under sustained concurrent prompts. A fresh idle memory reading is insufficient. Check both nodes' `MemAvailable`, swap, memory PSI, GPU usage, OOM/Xid logs and pool pressure. Do not assume this profile fits a different model or GPU count.
6. Verify real completions, saturation/queue behavior, client cancellation and long prefill behavior before relying on recovery. Retain enough client/proxy timeout budget for queue wait **plus** prefill and decode.

### Historical observations

Earlier incidents included confirmed OOM kills. Later stalls occurred while health/model-list endpoints still responded, GPUs were busy, and no OOM/Xid was captured. The latter cause was not conclusively established. This overlay addresses observability, load smoothing and recovery; it does not claim to fix an engine deadlock or prove OOM is impossible. No driver/firmware changes or host reboots are required by this package.
