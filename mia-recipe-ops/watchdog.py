#!/usr/bin/env python3
"""External supervisor for Mia's two-node TensorFold recipes (Python standard library)."""

import json
import logging
import os
from pathlib import Path
import shlex
import subprocess
import sys
import time
import urllib.error
import urllib.request

ROOT = Path(os.environ.get("MIA_STATE_DIR", "~/.cache/mia-recipe-ops")).expanduser()
BASE = os.environ.get("MIA_API_BASE", "http://127.0.0.1:8888").rstrip("/")
MODEL = os.environ.get("MIA_MODEL", "GLM-5.3-Flash-EXL3")
CONTAINER = os.environ.get("MIA_CONTAINER", "glm53-flash-tf")
WORKER = os.environ.get("MIA_WORKER", "")
SERVICE = os.environ.get("MIA_SERVICE", "mia-recipe.service")
POLL = float(os.environ.get("MIA_POLL_S", "20"))
DECODE_LIMIT = float(os.environ.get("MIA_DECODE_STALL_S", "240"))
PREFILL_LIMIT = float(os.environ.get("MIA_PREFILL_STALL_S", "1800"))
ITERATION_LIMIT = float(os.environ.get("MIA_ITERATION_STALL_S", "210"))
METRIC_PREFIX = os.environ.get("MIA_LIVE_METRIC_PREFIX", "tensorfold:kv_cache_usage_ratio")
PROGRESS_ENABLED = os.environ.get("MIA_PROGRESS_CHECK", "1") == "1"
STACK_SIGNAL = os.environ.get("MIA_STACK_SIGNAL", "0") == "1"
SSH = ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
       "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=2", "--", WORKER]


def run(args, timeout=15):
    try:
        p = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
        return p.returncode, p.stdout + p.stderr
    except (OSError, subprocess.TimeoutExpired) as exc:
        return -1, str(exc)


def remote(args):
    return SSH + [shlex.join(args)]


def get(path, timeout=6):
    with urllib.request.urlopen(BASE + path, timeout=timeout) as response:
        return response.read().decode()


def live_metrics(metrics):
    return tuple(sorted(line for line in metrics.splitlines() if line.startswith(METRIC_PREFIX)))


def signature(health, metrics):
    group = health.get("multi_prefill", {})
    return (health.get("requests_total"), health.get("completion_tokens_total"),
            health.get("requests_running"), json.dumps(health.get("streams", {}), sort_keys=True),
            group.get("chunks"), group.get("rows"), live_metrics(metrics))


def diagnose(reason):
    directory = ROOT / "recovery" / (time.strftime("%Y%m%d-%H%M%S", time.gmtime()) + "-" + str(os.getpid()))
    directory.mkdir(parents=True, mode=0o700)
    (directory / "reason.txt").write_text(reason + "\n")
    history = ROOT / "watchdog-history.jsonl"
    if history.exists():
        (directory / "history.jsonl").write_bytes(history.read_bytes())
    logging.error("Capturing recovery evidence in %s: %s", directory, reason)
    # State only: full docker inspect includes credentials in Config.Env.
    state = ["docker", "inspect", "--format", "{{json .State}}", CONTAINER]
    commands = {
        "head-state.json": state,
        "worker-state.json": remote(state),
        "head-gpu.txt": ["nvidia-smi", "-q"],
        "worker-gpu.txt": remote(["nvidia-smi", "-q"]),
        "head-memory.txt": ["sh", "-c", "free -m; cat /proc/pressure/memory; ps -eo pid,stat,etime,wchan:32,comm --sort=-rss | head -25"],
        "worker-memory.txt": remote(["sh", "-c", "free -m; cat /proc/pressure/memory"]),
        "head-connections.txt": ["ss", "-tanp"],
        "worker-connections.txt": remote(["ss", "-tanp"]),
        "head-kernel.log": ["journalctl", "-k", "-n", "250", "--no-pager"],
        "worker-kernel.log": remote(["journalctl", "-k", "-n", "250", "--no-pager"]),
        "service.log": ["journalctl", "-u", SERVICE, "-n", "250", "--no-pager"],
    }
    for path in ("/health", "/metrics"):
        try:
            output = get(path, 3)
        except Exception as exc:
            output = str(exc)
        (directory / (path[1:] + ".txt")).write_text(output)
    # Opt in only after verifying the image installs a nonfatal SIGUSR1 handler.
    # Some images terminate on this signal; worker handlers can also be overwritten.
    if STACK_SIGNAL:
        for number in range(2):
            signal = ["docker", "kill", "--signal=USR1", CONTAINER]
            for name, command in (("head", signal), ("worker", remote(signal))):
                rc, output = run(command, 12)
                (directory / f"{name}-stack-signal-{number}.txt").write_text(f"rc={rc}\n{output}")
            time.sleep(3)
    logs = ["docker", "logs", "--tail", "2000", CONTAINER]
    commands.update({"head-container.log": logs, "worker-container.log": remote(logs)})
    for name, command in commands.items():
        rc, output = run(command, 18)
        (directory / name).write_text(f"rc={rc}\n{output}")
    return str(directory)


class Progress:
    def __init__(self):
        self.key = None
        self.since = None

    def observe(self, health, metrics, now):
        if not health.get("requests_running"):
            self.key = self.since = None
            return None, 0
        key = signature(health, metrics)
        if key != self.key:
            self.key, self.since = key, now
        age = now - self.since
        # Completion totals only advance when requests finish. Without a live metric,
        # fail open on the elapsed-progress check rather than kill healthy long replies.
        streams = health.get("streams", {})
        filling = streams.get("prefilling", 0) + streams.get("filling", 0)
        limit = PREFILL_LIMIT if filling else DECODE_LIMIT
        if health.get("iteration_s", 0) >= ITERATION_LIMIT:
            return f"single scheduler iteration stalled >={ITERATION_LIMIT:g}s", age
        if live_metrics(metrics) and age >= limit:
            return f"no live cache growth or request progress for {age:.0f}s (limit {limit:g}s)", age
        return None, age


def probe():
    body = {"model": MODEL, "messages": [{"role": "user", "content": "Reply exactly OK"}],
            "max_tokens": 8, "stream": False}
    request = urllib.request.Request(BASE + "/v1/chat/completions", json.dumps(body).encode(),
                                     {"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=15) as response:
            if not json.load(response).get("choices"):
                raise RuntimeError("completion probe returned no choices")
        logging.info("Idle completion probe succeeded")
    except urllib.error.HTTPError as exc:
        if exc.code != 429:
            raise
        logging.info("Idle probe raced with admission; skipping HTTP 429")


def main():
    failures, progress, last_probe = 0, Progress(), 0
    while True:
        now, reason = time.monotonic(), None
        try:
            inspect = ["docker", "inspect", "-f", "{{.State.Running}}", CONTAINER]
            for command in (inspect, remote(inspect)):
                rc, output = run(command, 12)
                if rc or output.strip() != "true":
                    raise RuntimeError("rank not running: " + output.strip())
            health, metrics = json.loads(get("/health")), get("/metrics")
            if not health.get("ok") or "requests_running" not in health:
                raise RuntimeError("health not OK or incompatible schema; adapt this overlay to the recipe")
            reason, age = progress.observe(health, metrics, now) if PROGRESS_ENABLED else (None, 0)
            if PROGRESS_ENABLED and health["requests_running"] and not live_metrics(metrics):
                logging.warning("Live metric missing; elapsed-progress recovery disabled for this sample")
            record = {"time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "health": health,
                      "progress_age_s": round(age, 1), "live_cache_metrics": live_metrics(metrics)}
            history = ROOT / "watchdog-history.jsonl"
            if history.exists() and history.stat().st_size > 8 * 1024 * 1024:
                os.replace(history, ROOT / "watchdog-history.previous.jsonl")
            with history.open("a") as handle:
                handle.write(json.dumps(record) + "\n")
            if not health["requests_running"] and now - last_probe >= 300:
                probe()
                last_probe = now
            failures = 0
            logging.info("progress active=%s streams=%s pool_free=%s kept_bytes=%s unchanged_s=%.0f",
                         health["requests_running"], health.get("streams"), health.get("pool_free_tokens"),
                         health.get("kept_bytes"), age)
        except Exception as exc:
            failures += 1
            logging.warning("Cluster health failure %s/3: %s", failures, exc)
            if failures >= 3:
                reason = f"three cluster health/probe failures: {exc}"
        if reason:
            diagnose(reason)
            return 1  # systemd restarts the supervisor, which restarts both ranks
        time.sleep(POLL)


if __name__ == "__main__":
    os.umask(0o077)
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s", stream=sys.stdout)
    if not WORKER or WORKER.startswith("-"):
        sys.exit("Set MIA_WORKER to the worker's key-authenticated SSH target")
    ROOT.mkdir(parents=True, exist_ok=True, mode=0o700)
    if "--capture-only" in sys.argv:
        print(diagnose("manual capture; no recovery requested"))
    else:
        sys.exit(main())
