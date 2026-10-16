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
admit a pack. Shared sampling rounds prompt both reader FSMs at most every 3 seconds
without interrupting commands. Rounds continue with both packs active in `parked`
and `ready-to-drive`; they do not permit second-pack admission while driving.
A second pack needs three paired samples over 6 seconds: data age ≤10 seconds,
STATUS0 acquisition skew ≤1 second, absolute current ≤1000 mA, and voltage
variation ≤100 mV per pack.
The voltage difference must stay within `scooter.max-voltage-delta` minus 10%
(capped at 100 mV). A limit of 0 disables only the voltage check.

ON writes reserve the slot before I/O. Failed writes and lost tags do not prove
the pack is off. Reservations need inactive readings to clear; unconfirmed ON
retries stop after 10 seconds. Admission state is not persisted across restarts.

### Coordinated acquisition

Each round has a process-local increasing ID and one read attempt per reader.
A 10-second round deadline leaves room for command settling and discovery before
acquisition. Each complete frame must also finish within 2 seconds of its STATUS0
start. The scheduler checks every 3 seconds and skips ticks while a round remains
in flight; there are no overlapping rounds or repeated prompts within a round.
A healthy pair can therefore be sampled every 3 seconds, while busy readers slow
the cadence. An unanswered round is retired on the first tick after its deadline,
normally at 12 seconds. This bounds extra NFC traffic. Readers execute
concurrently on their independent NFC controllers through the existing FSMs and
`nfcMu` locks. A poll starts a normal heartbeat only in `wait_update`: commands,
400 ms settling delays, OFF confirmation, discovery and retries take precedence.
Each read claims its round at entry, before discovery; a read already in flight
when a round opens cannot be relabeled into it.
There is no barrier, extra NFC worker, catch-up loop or retry of a failed round.
Busy readers may miss rounds; heartbeats never wait for the peer.

STATUS0 contains voltage/current. Its start and end are recorded around the
verified read, including any retries; full-frame completion is recorded separately.
For two intervals `[start0,end0]` and `[start1,end1]`, the conservative skew bound
is `max(end0,end1) - min(start0,start1)`. Only bounds ≤1 second are accepted.
Thus even identical start times cannot hide a slow read. Both complete frames
must belong to the same round, meet its deadline and pass existing tag freshness
checks. Completion timestamps alone do not establish alignment.

Incomplete rounds expire at the first scheduler tick after their deadline;
results beyond the 10-second round or 2-second frame limits are already ineligible.
Failed, stale, slow, late or superseded reads clear the paired stability window. Tag, role and vehicle changes, activation
reservations, OFF attempts and shutdown also invalidate it. Late results cannot
join a newer round. An unpaired observation can still establish supply state or
OFF confirmation, but cannot combine with an older peer observation to admit a
second pack. Sampling failure itself issues no OFF command and does not release
a supply reservation. Missing packs retain the existing fresh-absence rules for
starting the first supply. Single-reader configurations do not create rounds;
with the experimental flag disabled, the existing path is unchanged.

Service debug logs emit at most one round summary per scheduler tick: round ID,
result, conservative skew, STATUS0 durations and full-frame durations from
STATUS0 start. Missing observations have zero durations. No new Redis fields
or settings are introduced.

Synchronized NFC acquisition does **not** guarantee synchronized BMS measurements.
The tags contain independently refreshed data without proven sample timestamps.
The 1-second bound applies to NFC acquisition, not to the underlying voltage or
current measurement instants. Frame-change freshness is not a BMS sample clock.

### Bench validation procedure

This implementation has not been hardware validated. Before vehicle use, validate
on an electrically protected, stationary dual-reader bench with independently
logged pack voltages/currents and bus voltage. Do not infer continuity from Redis.

1. Establish a flag-disabled baseline and a single-enabled-reader baseline.
   Record command order, settling intervals, heartbeat spacing and OFF confirmation.
2. Enable the experimental flag and both readers, restart the service, and capture
   service debug logs alongside NFC traces and electrical measurements. Verify
   STATUS0 interval bounds, round/frame deadlines and three successful rounds
   spanning at least 6 seconds of acquisition starts before parked second-pack
   admission. Include discovery delays longer than one scheduler tick.
3. Exercise reader delay, arbiter busy, discovery, dropped reads, unchanged tag
   data and removal of either pack. Verify missed/late rounds are rejected without
   tight retries, cross-round pairing, lost supply reservations or sampling-induced
   OFF commands. Confirm heartbeat deadlines and command settling still hold.
4. Test voltage mismatch, load above 1000 mA, moving vehicle state, OFF in progress,
   role changes, shutdown and restart. Confirm no unsafe second-pack admission and
   measure running-supply continuity, including restart behavior.
5. With both packs active, record ongoing rounds and traffic for an extended run.
   Compare NFC data against independently timestamped load/voltage changes to
   characterize tag refresh latency and inter-pack measurement age. Report those
   separately from the NFC acquisition bound; do not claim measurement synchronization
   from successful rounds alone.

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
