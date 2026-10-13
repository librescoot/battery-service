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
- Provides optional experimental higher-voltage pack selection and coordinated
  admission of the second pack.

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

`experimental.battery.dual-pack-admission` defaults to **false**. It is read at
startup; changes require a service restart. It does not enable reader 1: that
still requires `scooter.dual-battery=true` or `--battery1-active`.

With the flag off, slot 1 uses `scooter.max-voltage-delta` against the published
slot 0 voltage. Startup and role changes check the limit. The insertion check
waits for tag confirmation and latches until the pack is reinserted.

With the flag on:

- Both readers share a gate for every ON command. The higher-voltage usable
  pack starts first; charge breaks a voltage tie, then slot 0 breaks an exact tie.
- Initial activation is allowed in `stand-by`, `waiting-seatbox`, or `parked`.
  A second pack can join only in `parked`.
- Active packs keep their ON heartbeats. The service does not switch off a
  running pack to select another, or add an inactive pack while driving.
- OFF commands still follow seatbox, empty-pack and disable handling. An OFF
  attempt needs a later inactive reading before ON is allowed again.

Admission uses complete NFC reads with a frame change in the last 10 seconds,
not cached Redis voltage. Missing, stale, truncated or substituted data cannot
admit a pack. Pending activation polls every 3 seconds without interrupting commands.
A second pack needs three samples over 6 seconds: data age ≤10 seconds, reader
skew ≤3 seconds, absolute current ≤1000 mA, and voltage variation ≤100 mV per pack.
The voltage difference must stay within `scooter.max-voltage-delta` minus 10%
(capped at 100 mV). A limit of 0 disables only the voltage check.

ON writes reserve the slot before I/O. Failed writes and lost tags do not prove
the pack is off. Reservations need inactive readings to clear; unconfirmed ON
retries stop after 10 seconds. Admission state is not persisted across restarts.

The experimental path, including restart power continuity, needs bench testing
before vehicle use.

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
