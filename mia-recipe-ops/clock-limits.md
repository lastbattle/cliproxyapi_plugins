# CPU and GPU clock limits on two DGX Sparks

Applied and verified on both nodes on **2026-11-01 UTC** (2026-10-31 Pacific), following [0xWhiteMage's recommendation](https://x.com/0xWhiteMage/status/2116573213086130592). This is a record of the deployed host configuration, independent of the Mia recipe and its memory/context settings.

| Control | Head and worker |
| --- | --- |
| CPU maximum | 2,808 MHz on all 20 cores |
| GPU graphics-clock range | 0–2,200 MHz; zero leaves the lower bound unconstrained |
| Persistence | Enabled systemd oneshot services on each host |
| GLM restart / host reboot during application | Neither required |

The CPU maximum was previously **2,200 MHz**, enforced by an existing service. Before this change the GPUs were observed at 2,444–2,457 MHz. CPU and GPU limits are separate: `cpupower` controls CPUs, while `nvidia-smi -lgc` controls the GPU. The quoted upstream experiment capped performance cores and left efficiency cores alone; the linked tweet's `-c all` command applies the maximum to all cores, as this deployment does.

## Apply live on each node

The tweet's commands are:

```bash
sudo cpupower -c all frequency-set -u 2808MHz
sudo nvidia-smi -lgc 0,2200
```

These commands alone reset on reboot. The deployed configuration below reapplies them through equivalent CPU sysfs controls and the same GPU command. Before adapting to another host, inspect its existing frequency services so an older cap does not overwrite the new one.

## Persistent configuration actually deployed

The existing `/usr/local/sbin/gx10-thermal-throttle` sets `limit_khz=2808000` and iterates over `/sys/devices/system/cpu/cpu[0-9]*`. For each core it writes the limit to `cpufreq/max_perf` if writable, otherwise to `cpufreq/scaling_max_freq`. All 20 policies were verified afterward through `scaling_max_freq`.

The existing `/etc/systemd/system/gx10-thermal-throttle.service` now contains:

```ini
[Unit]
Description=Cap GX10 CPU frequency at 2.808 GHz for thermal control
After=systemd-modules-load.service nv-cpu-governor.service
Before=deepseek-dspark.service glm53-exl3.service

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/gx10-thermal-throttle
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
```

The `gx10` names and legacy `deepseek-dspark.service` ordering are retained from the existing host setup; the machines serve GLM. Ordering after NVIDIA's CPU governor service ensures the boot-time governor setup precedes the cap.

New `/etc/systemd/system/glm-gpu-clock-cap.service`:

```ini
[Unit]
Description=Cap NVIDIA GPU graphics clocks at 2200 MHz
After=systemd-modules-load.service nvidia-persistenced.service
Before=glm53-exl3.service

[Service]
Type=oneshot
ExecStart=/usr/bin/nvidia-smi -lgc 0,2200
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
```

After writing the files, the deployment ran on **each node**:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now glm-gpu-clock-cap.service
sudo systemctl restart gx10-thermal-throttle.service
```

The CPU service was already enabled. Restarting it changes host CPU limits, not the inference service. If adapting to the generic overlay service `mia-recipe.service`, adjust the `Before=` ordering to that name. The cap services do not make inference depend on successful cap application; check them after boot or driver changes.

## Verify

Run on both nodes:

```bash
systemctl is-enabled gx10-thermal-throttle.service glm-gpu-clock-cap.service
systemctl is-active gx10-thermal-throttle.service glm-gpu-clock-cap.service
for p in /sys/devices/system/cpu/cpufreq/policy*; do
  printf '%s: ' "${p##*/}"
  cat "$p/scaling_max_freq"
done
journalctl -u glm-gpu-clock-cap.service -b --no-pager
nvidia-smi --query-gpu=clocks.current.graphics,clocks.current.sm,temperature.gpu --format=csv
```

Expected CPU maximum: `2808000` kHz for every policy. The GPU service log should confirm `gpuClkMin 0, gpuClkMax 2200`. Allow the GPU clock reading to settle after changing the cap; the immediate first reading still showed the old clock. Subsequent live readings were **2,177 MHz on the head** and **2,184 MHz on the worker**, both below the limit. GPU hardware `Max Clocks` and `Applications Clocks` fields are not proof of the active locked-clock ceiling.

The head's `/health` remained healthy with a live decoding request after application. No reboot persistence test was performed; both services were enabled and active. Temperature and throughput improvements have not been benchmarked for this GLM deployment. The tweet's reported performance/temperature results concern another workload and are not a guarantee here.

## Backups and rollback

Pre-change CPU script and service copies are stored privately on their respective hosts:

- Head: `/home/random/.cache/tensorfold-glm53/config-backups/20261101-065735-clock-caps/`
- Worker: `/home/random/.cache/tensorfold-glm53/config-backups/20261101-065740-clock-caps/`

To restore the previous **2,200 MHz CPU** limit, copy that host's backed-up `gx10-thermal-throttle` and `gx10-thermal-throttle.service` to their original paths, run `sudo systemctl daemon-reload`, then `sudo systemctl restart gx10-thermal-throttle.service`.

To remove the new GPU ceiling on each node:

```bash
sudo systemctl disable --now glm-gpu-clock-cap.service
sudo nvidia-smi -rgc
```

Stopping this oneshot service alone does not undo the clock setting; `-rgc` resets GPU locked clocks. Keep the disabled unit for reference or remove it later. Neither rollback requires restarting GLM.

## Relation to the head outage

The head stopped abruptly around 2026-10-31 06:32 UTC. Its previous boot had no recorded overtemperature warning, OOM kill, GPU Xid or kernel panic, and no historical temperature samples survived. The old 2,200 MHz CPU cap was already configured before that incident. These new settings reduce the GPU ceiling while raising the CPU ceiling to the tweet's value; they do **not** establish that overheating caused the outage or that a recurrence is prevented.
