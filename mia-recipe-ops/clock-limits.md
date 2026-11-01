# CPU and GPU clock limits on two DGX Sparks

Set CPU and GPU clock limits on each node, independently of the Mia recipe and its memory/context settings.

| Control | Setting on each node |
| --- | --- |
| CPU maximum | 2,808 MHz on all 20 cores |
| GPU graphics-clock range | 0–2,200 MHz; zero leaves the lower bound unconstrained |
| Persistence | systemd oneshot services on each host |
| GLM restart / host reboot during application | Neither required |

CPU and GPU limits are separate: `cpupower` controls CPUs, while `nvidia-smi -lgc` controls the GPU. The CPU command below applies the maximum to all cores.

## Apply live on each node

Run on each node:

```bash
sudo cpupower -c all frequency-set -u 2808MHz
sudo nvidia-smi -lgc 0,2200
```

These commands alone reset on reboot. The persistent configuration below reapplies them through equivalent CPU sysfs controls and the same GPU command. Before adapting to another host, inspect its existing frequency services so an older cap does not overwrite the new one.

## Persistent configuration

Back up any existing scripts and units before replacing them. Record the current CPU limits if you need to restore them later.

Create `/usr/local/sbin/gx10-thermal-throttle`, owned by root and executable (`0755`):

```sh
#!/bin/sh
set -eu
limit_khz=2808000
for cpu in /sys/devices/system/cpu/cpu[0-9]*; do
  if [ -w "$cpu/cpufreq/max_perf" ]; then
    printf '%s\n' "$limit_khz" > "$cpu/cpufreq/max_perf"
  elif [ -w "$cpu/cpufreq/scaling_max_freq" ]; then
    printf '%s\n' "$limit_khz" > "$cpu/cpufreq/scaling_max_freq"
  fi
done
```

Create `/etc/systemd/system/gx10-thermal-throttle.service` (`0644`):

```ini
[Unit]
Description=Cap GX10 CPU frequency at 2.808 GHz for thermal control
After=systemd-modules-load.service nv-cpu-governor.service
Before=mia-recipe.service

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/gx10-thermal-throttle
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
```

Ordering after NVIDIA's CPU governor service ensures the boot-time governor setup precedes the cap.

Create `/etc/systemd/system/glm-gpu-clock-cap.service` (`0644`):

```ini
[Unit]
Description=Cap NVIDIA GPU graphics clocks at 2200 MHz
After=systemd-modules-load.service nvidia-persistenced.service
Before=mia-recipe.service

[Service]
Type=oneshot
ExecStart=/usr/bin/nvidia-smi -lgc 0,2200
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
```

After writing the files, run on **each node**:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now gx10-thermal-throttle.service glm-gpu-clock-cap.service
```

If updating an already active CPU service, run `sudo systemctl restart gx10-thermal-throttle.service` to apply the new limit. Adjust `Before=` if your inference service uses another name. The cap services do not make inference depend on successful cap application; check them after boot or driver changes.

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

Expected CPU maximum: `2808000` kHz for every policy. The GPU service log should confirm `gpuClkMin 0, gpuClkMax 2200`. Allow the GPU clock reading to settle after changing the cap. GPU hardware `Max Clocks` and `Applications Clocks` fields are not proof of the active locked-clock ceiling. Check service status and limits again after reboot.

## Rollback

To restore an earlier CPU limit, restore your backed-up script and unit, run `sudo systemctl daemon-reload`, then `sudo systemctl restart gx10-thermal-throttle.service`.

To remove the GPU ceiling on each node:

```bash
sudo systemctl disable --now glm-gpu-clock-cap.service
sudo nvidia-smi -rgc
```

Stopping this oneshot service alone does not undo the clock setting; `-rgc` resets GPU locked clocks. Keep the disabled unit for reference or remove it later. Neither rollback requires restarting GLM.
