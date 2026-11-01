# Wi-Fi recovery on two DGX Sparks

Configured on **both head and worker on 2026-11-01 UTC**. This is a host NetworkManager/systemd configuration, separate from the Mia recipe and inference watchdog.

## Reconnect behavior

The configuration:

- Sets `connection.autoconnect=yes` and `connection.autoconnect-retries=0` (retry forever) for the saved `frontier` profile.
- Runs a systemd check approximately every minute, starting 90 seconds after boot.
- When Wi-Fi is disconnected, activates that specific saved profile by UUID.
- Leaves connected, activating, unavailable and unmanaged devices alone. A disabled Wi-Fi radio is also left alone.
- Does not restart NetworkManager, inference, Docker, the hosts or ConnectX-7 interfaces.

The check detects a **disconnected Wi-Fi device**, not broken Internet access or an unresponsive router while association remains active. It cannot repair an unavailable access point, failed radio or powered-off host. NetworkManager may still consider other saved profiles during its own autoconnect attempts; this timer specifically targets `frontier` and will not replace an already active connection.

## Install or adapt on each node

Use a saved, working Wi-Fi profile; no passwords belong in the script or this repository. The deployed interface is `wlP9s9` and the profile is `frontier`. Verify your own values first:

```bash
nmcli device status
nmcli -f NAME,UUID,TYPE,DEVICE connection show
nmcli -f connection.id,connection.autoconnect,connection.autoconnect-retries connection show frontier
```

Back up any existing files before replacing them. Record the original autoconnect/retry values for rollback. Set unlimited retries without disconnecting the active connection:

```bash
sudo nmcli connection modify frontier \
  connection.autoconnect yes connection.autoconnect-retries 0
nmcli -g connection.uuid connection show frontier
```

Create `/usr/local/sbin/glm-wifi-reconnect`, owned by root and executable (`0755`). Replace `YOUR-SAVED-FRONTIER-PROFILE-UUID` with the UUID printed above on **that host**:

```sh
#!/bin/sh
set -eu
iface='wlP9s9'
profile_uuid='YOUR-SAVED-FRONTIER-PROFILE-UUID'
# Never disturb an active connection, an ongoing activation, or a disabled radio.
[ "$(nmcli -g WIFI general)" = "enabled" ] || exit 0
state=$(nmcli -g GENERAL.STATE device show "$iface")
case "$state" in
  "30 "*) ;;
  *) exit 0 ;;
esac
echo "Wi-Fi disconnected; reactivating saved frontier profile"
exec /usr/bin/nmcli --wait 35 connection up uuid "$profile_uuid" ifname "$iface"
```

State `30` is NetworkManager's disconnected state. UUID selection avoids ambiguous duplicate connection names and prevents the script from automatically selecting a saved hotspot profile.

Create `/etc/systemd/system/glm-wifi-reconnect.service` (`0644`):

```ini
[Unit]
Description=Reconnect disconnected GLM host Wi-Fi using saved frontier profile
After=NetworkManager.service
Wants=NetworkManager.service

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/glm-wifi-reconnect
TimeoutStartSec=45
```

Create `/etc/systemd/system/glm-wifi-reconnect.timer` (`0644`):

```ini
[Unit]
Description=Check GLM host Wi-Fi connection every minute

[Timer]
OnBootSec=90s
OnUnitInactiveSec=60s
AccuracySec=5s
Unit=glm-wifi-reconnect.service

[Install]
WantedBy=timers.target
```

Enable the timer and run the connected-device no-op check:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now glm-wifi-reconnect.timer
sudo systemctl start glm-wifi-reconnect.service
```

The next check runs 60 seconds after the previous check finishes, with up to five seconds of timer coalescing. An attempted activation can take up to 35 seconds; this is not a guarantee of recovery within one minute. Failed activations are retried on subsequent timer runs.

## Verify and troubleshoot

```bash
nmcli -f connection.autoconnect,connection.autoconnect-retries connection show frontier
nmcli -f GENERAL.STATE device show wlP9s9
systemctl is-enabled glm-wifi-reconnect.timer
systemctl is-active glm-wifi-reconnect.timer
systemctl list-timers glm-wifi-reconnect.timer --no-pager
systemctl show glm-wifi-reconnect.service -p Result -p ExecMainStatus
journalctl -u glm-wifi-reconnect.service -b --no-pager
journalctl -u NetworkManager --since '1 hour ago' --no-pager
```

After deployment, both profiles reported retries `0 (forever)`, both timers were active, and both devices remained connected. The service's connected-device no-op returned success on each host; GLM remained healthy with an active decoding request. We did **not** deliberately disconnect either host or reboot to test recovery. Timer enablement provides boot persistence, but a forced-outage recovery test remains unperformed.

If the head loses LAN access while the worker remains reachable, SSH through the worker to the head's ConnectX-7 address and reactivate the saved profile. This requires suitable SSH authentication on both hops.

## Backup and rollback

Original profile settings were recorded privately in `profile-settings.json` on each host:

- Head: `/home/random/.cache/tensorfold-glm53/config-backups/20261101-223905-wifi-reconnect/`
- Worker: `/home/random/.cache/tensorfold-glm53/config-backups/20261101-223909-wifi-reconnect/`

To stop the additional recovery check:

```bash
sudo systemctl disable --now glm-wifi-reconnect.timer
```

Restore that host's saved `connection.autoconnect` and `connection.autoconnect-retries` values with `nmcli connection modify`. The deployment had no preexisting reconnect script/service/timer; the new disabled files may be retained for reference. If removing them, run `sudo systemctl daemon-reload` afterward. Neither disabling the timer nor restoring these profile settings requires restarting GLM or disconnecting Wi-Fi.
