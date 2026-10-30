# MyVacControl

Controls an iHome AutoVac robot vacuum from MyHomeServicesHub. It runs on the Pi on the home network and connects to the vacuum directly with the Tuya LAN protocol (3.3), so no cloud is involved once it's set up.

- **Control:** start (auto, edges, spot, random, mop), pause and resume, dock, stop, suction power, locate (beep), and driving it by hand with arrow buttons.
- **Status:** state, battery, mode, suction, the time and area of the clean in progress, and faults in words ("Main brush stuck").
- **Schedules:** cleans at set times on chosen weekdays or once on a date, run by the Pi in its local time, with optional quiet hours that schedules skip.
- **History:** every clean (length, area, battery used, who or what started it, how it ended) and every command sent, kept in MariaDB `vac_db`.
- **Statistics:** cleans, time and area for the last 30 days and all time, and a chart of cleaning time per week.
- **Maintenance:** wear on the brushes and filter, from the cleaning hours since each was replaced.

| | |
|---|---|
| Port | 8085 (bound to 127.0.0.1) |
| Hub path | `/vac` |
| Roles | admin, user |
| Database | `vac_db` / `vac_app` |
| Service | `vac-control` |

## 1. Get the vacuum's device ID and local key

The iHome AutoVac app is built on Tuya. The vacuum only accepts LAN commands encrypted with its **local key**, which you get once from Tuya's developer site:

1. Create a free account at <https://platform.tuya.com> and create a **Cloud project** (Smart Home, your data centre, e.g. "Western America").
2. Under **Devices → Link App Account**, scan the QR code with the **Smart Life** app.
   Linking only works with Tuya's own apps (Smart Life / Tuya Smart), not with OEM apps like iHome AutoVac. If the vacuum isn't in Smart Life yet, add it there. That re-pairs it and changes its local key, so do it before you read the key. The iHome app stops working when you re-pair, but Smart Life controls the vacuum the same way.
3. The vacuum then appears under **Devices**. Its **Device ID** is listed there. To read the **local key**, either:
   - use **API Explorer → Device Management → Query device details** with the device ID and copy `local_key`, or
   - run `python -m tinytuya wizard` on any PC and paste in the project's Access ID and Secret.

Re-pairing the vacuum (in any app) changes the local key. If the page shows it offline with a decrypt error, fetch the key again.

## 2. Find it on the network

Give the vacuum a DHCP reservation. Then, from a machine on the same network or VLAN as the vacuum (the broadcasts don't cross VLANs):

```bash
./vac-control -discover
# 172.16.32.246    id ebxxxxxxxxxxxxxxxxxxxx  version 3.3  product lzpkmnfu2wrws8cf
```

Only protocol **3.3** is supported. The Eclipse and Nova both use it.

The vacuum can be on an isolated VLAN. The service always opens the connection itself (TCP 6668), and the vacuum's replies and pushes come back over it, so an established/related firewall rule is enough. The vacuum never needs to start a connection.

## 3. Configure and check the data points

```bash
cp config.example.yaml config.yaml   # fill in host, device_id, local_key, DB password
./vac-control -dump autovac          # prints every data point for 30 s
```

`model:` picks the data points. Each comes from the Tuya cloud's thing model for that vacuum.

| | eclipse (iHRV1/iHRV2), default | nova |
|---|---|---|
| Start | set the mode (25) | 2 |
| Pause / stop | 26 = stop (stops where it is, status 0); resume sets the mode again | 2 = false / mode standby |
| Mode | 25: smart, wallfollow, sprial, single, mop, chargego, idle | 3: smart, wall_follow, spiral, random, mop, chargego, standby |
| Suction | 27: ECO, normal, strong | 14: gentle, normal, strong |
| Status | 38: 0 standby/stopped, 1 cleaning, 2 mopping, 3 edges, 4 returning to dock, 5 on dock charging, 6 spot, 7 paused (Smart Life's pause), 8 one room | 5: named states |
| Battery | 39 | 6 (reads 0 locally; the battery level comes from 102) |
| Area / time | 41 (m²) / 42 (min) | inside 102 (base64 JSON) |
| Fault | 11 (bitmap, see below) | 110 |
| Drive | 26: forward, backward, turnleft, turnright, stop | none |
| Locate | none | 13 |

The Eclipse's status codes, faults and drive values come from its thing model, and 0, 1, 4, 5 and 7 were also seen with `-dump` during a start, pause, resume and dock cycle. A code outside the list shows as "(state N)" on the page, and it counts as cleaning while the mode is a cleaning mode. To name one, add it under `status_labels` in config.yaml. The Eclipse ignores its start switch (33) over the LAN and never reports it, so the module starts it by setting the mode and pauses it with 26 = stop. A stopped clean stays open in the history until the vacuum docks.

To read a model's data points from the cloud: in your Tuya project, go to **API Explorer** and call **Device Control → Query Things Data Model** with the device ID.

### Faults and driving

- **Faults (11):** a bitmap. Bit 0 side brush, 1 main brush, 2 left wheel, 3 right wheel, 4 dustbin, 5 cliff sensor / lifted, 6 bumper, 7 water tank missing, 8 boundary strip sensor, 9 robot stuck. A bit outside these shows as "Fault bit N". To rename them, set `fault_bits` (bit 0 first).
- **Driving (26):** forward, backward, turnleft, turnright and stop, set with `directions:`. To try a value by hand, stop the service first (`sudo systemctl stop vac-control`), because the vacuum takes only one LAN connection. Put the vacuum off the dock in open space, then run these back to back. Nothing stops it in between, so it drives until the second command:
  ```bash
  ./vac-control -dump autovac -seconds 2 -set '{"26":"forward"}'
  ./vac-control -dump autovac -seconds 2 -set '{"26":"stop"}'
  ```

While you hold a drive button, the page sends the direction again every second. If 2.5 s pass with no repeat (the tab closed, the phone locked or Wi-Fi dropped), the service sends stop.

### Parts

Wear is worked out from the time logged in clean history since each part was last marked as replaced, pauses included. The default lives are typical robot vacuum figures: main brush 300 h, side brush 200 h and filter 150 h. To use the figures from your manual, set `parts:` (see config.example.yaml). Until a part is first marked as replaced, all recorded cleans count toward its wear.

## 4. Database

```sql
CREATE DATABASE vac_db CHARACTER SET utf8mb4;
CREATE USER 'vac_app'@'localhost' IDENTIFIED BY '<password>';
CREATE USER 'vac_app'@'127.0.0.1' IDENTIFIED BY '<password>';
GRANT ALL ON vac_db.* TO 'vac_app'@'localhost';
GRANT ALL ON vac_db.* TO 'vac_app'@'127.0.0.1';
```

The tables (`cleans`, `command_log`, `schedules`, `part_resets`, `quiet_hours`) are created on first start, and an older `schedules` table gets its new `run_date` column then too (this needs MariaDB 10.0.2 or later). Add `vac_db` to the nightly backup in MyHomeServicesHub/backup.

## 5. Install on the Pi

```bash
cd /home/myhomeservices/go/src/MyVacControl && git pull
./install.sh        # as a user with sudo
```

Then add the hub tile and route. MyHomeServicesHub's main.go already proxies `/vac` to port 8085. Add this to the hub's config.yaml:

```yaml
  - name: Vacuum
    description: Start, dock and schedule the robot vacuum
    icon: 🤖
    path: /vac
    enabled: true
    allowed_roles: [admin, user]
```

## How it works

- One persistent TCP connection per vacuum on port 6668, with a heartbeat every 10 s. It reconnects with backoff from 5 s up to 60 s. The vacuum pushes every state change over this connection, and the page polls the service every 3 s.
- A clean starts when the vacuum starts cleaning and ends when it docks, goes idle or stops. A pause doesn't end it. A dropped connection doesn't end a clean. A clean is credited to whoever sent Start within the previous 2 minutes, otherwise to "vacuum" (its own app or button).
- Schedules are checked every 20 s. A schedule is skipped and logged if the vacuum is already cleaning, or if its time falls in the quiet hours. Quiet hours are checked when a schedule comes due, so changing them applies to existing schedules too. Starting a clean by hand ignores them.

## Development

```bash
go test ./...                       # includes a fake Tuya vacuum on a local port
go run . -config config.yaml        # set server.allow_direct: true to open it without the hub
```
