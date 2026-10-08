# Librescoot Battery Service

Part of the [Librescoot](https://librescoot.org/) open-source platform.

`battery-service` monitors the unu Scooter Pro Battery Packs (Propulsion
Battery Packs) through PN7150 NFC readers
and publishes battery state to Redis. It supports two reader slots and tracks
battery presence, status, temperature, and faults.

## Capabilities

- Reads BMS status from configured NFC devices.
- Publishes per-pack state, measurements, identity data, and fault state.
- Monitors pack temperature and derives `cold`, `ideal`, `hot`, or `unknown`
  temperature state.
- Coordinates reader activity with vehicle and seatbox state.
- Selects the higher-voltage pack as the initial supply in dual-battery mode
  and coordinates admission of the second pack.

## Operation and Redis interface

Each reader maintains a `battery:<index>` hash (`battery:0` and, when enabled,
`battery:1`). Published fields include presence, BMS state, voltage, current,
charge, four temperatures, temperature state, cycle count, state of health,
serial number, manufacture date, and firmware version. Changed fields are
announced on the matching `battery:<index>` channel.

Active pack faults are held in `battery:<index>:fault`; fault events are also
appended to `events:faults` with the corresponding battery group. The service
subscribes to `vehicle`, `settings`, and `aux-battery`. Vehicle `state` and
`seatbox:lock` changes control reader behavior.

## Configuration

Run `bin/battery-service -help` after building for the authoritative flag list.
Command-line configuration selects Redis, update and heartbeat intervals,
reader device paths, reader roles, logging, and debug output. Reader 0 is active
by default. Reader 1 can be enabled as active with a command-line option or by
the `scooter.dual-battery` Redis setting; it can also be disabled entirely.

The following `settings` hash fields are read at startup and reloaded on
notification:

- `scooter.dual-battery`
- `scooter.battery-keep-active-on-seatbox-open`
- `scooter.max-voltage-delta`
- `scooter.battery-aux-low-keep-active-enter-mv`
- `scooter.battery-aux-low-keep-active-exit-mv`

The two aux-low keep-active settings drive a Schmitt trigger over the
`aux-battery` voltage: the override engages below the enter threshold and
disengages at or above the exit threshold. The latch lives in memory only, so
the first aux reading after a service start is judged against the exit
threshold rather than the enter one. If `aux-battery` has no voltage yet, the
override stays off until the first reading arrives.

Temperature, battery activation, and voltage-difference settings affect power
behavior. Restrict their modification to trusted configuration components.

## Dual-battery activation

With `scooter.dual-battery=true` (or `--battery1-active`), both readers share
an admission policy. Every ON command passes through it, including activation
following a seatbox cycle, role change, or observed pack deactivation.

- When both packs are confirmed inactive, the higher-voltage usable pack is
  selected first. Charge percentage breaks an equal-voltage tie; slot 0 breaks
  an exact tie. A pack reporting a fault or the BMS empty condition is not
  selected as a new supply.
- Initial activation is allowed in `stand-by`, `waiting-seatbox`, or `parked`.
  A pack joining an existing supply must wait for `parked`.
- A lower-voltage running supply is not switched off to select the fuller pack.
  Selection is reconsidered when both packs are confirmed inactive.
- Already-active packs retain their ON heartbeats despite voltage drift or
  missing measurements. A fresh inactive report requires new admission. No
  inactive pack is automatically added in `ready-to-drive`.
- Seatbox, empty-pack and explicit disable behavior still control OFF commands.
  ON after an OFF attempt requires post-command inactive evidence.

Admission uses complete NFC reads, not cached Redis voltage. Both readers must
provide fresh data, or the unused slot must have a recent successful no-tag
observation. A frame change is required as evidence that the pack has written
to its tag within the last 10 seconds; repeated identical reads do not extend
that deadline. Truncated frames and sensor-artifact substitutions are excluded.
There is no timeout that turns unknown data into permission to activate.

For a second pack to join, readings must be at most 10 seconds old and no more
than 3 seconds apart. Both packs need at least three samples spanning 6 seconds,
with absolute current at or below 1000 mA and voltage variation at or below
100 mV per pack. Every voltage pair in that window must be at or below
`scooter.max-voltage-delta` minus an admission margin of 10% (capped at 100 mV).
The default limit is 1000 mV, giving a 900 mV admission bound. Setting the limit
to `0` disables only the voltage-difference check, not freshness, load, vehicle
state, or command coordination.

While admission is pending in parked, a 3-second poll requests early heartbeats
from readers resting between command cycles. Polling does not interrupt command
settling delays. The running pack receives its ON heartbeat before the status
read. Initial selection also polls in stand-by or waiting-seatbox until a supply
is confirmed. Both-active operation uses the normal heartbeat interval.

An ON reservation prevents simultaneous independent activation decisions. A
failed write remains a possible live supply until an inactive report confirms
otherwise; unconfirmed ON retries are bounded to 10 seconds. Losing an active
pack's NFC tag does not prove its output is OFF and can therefore defer another
pack's activation until power state is confirmed. A different tag identity
starts a new admission episode. Decision changes and deferral reasons appear
in the service log as `Battery activation decision`.

Admission state is not persisted across service restarts. At startup, an
unchanged ACTIVE tag snapshot alone cannot authorize ON; the service waits for
write evidence. Restart power continuity therefore needs explicit bench testing.

The load, timing and voltage bounds require bench validation before vehicle
rollout. Voltage agreement is an admission condition, not a guarantee of a
particular equalization current or an exact current split between packs.

## Build and test

```bash
make build         # Linux ARMv7 binary: bin/battery-service
make build-host    # local-development binary: bin/battery-service-host
make build-native  # local-development binary: bin/battery-service
make test
make lint          # requires golangci-lint
```

## Deployment and operations

The Yocto layer ships `librescoot-battery.service`, which requires Valkey,
starts after the vehicle service, and wants systemd-logind. The service requires
a reachable Redis-compatible datastore, access to the configured PN7150 NFC
devices (defaults are `/dev/pn5xx_i2c0` and `/dev/pn5xx_i2c1`), and hardware
compatible with the battery reader protocol.

It handles `SIGINT` and `SIGTERM`, then stops readers before closing Redis.
Redis status is useful for monitoring but should not replace electrical safety
controls or direct diagnosis of a pack.

## License

This project is licensed under the [Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License](LICENSE).

Made with ❤️ by the Librescoot community
