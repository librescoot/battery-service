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
- Serializes pack activation independently of physical slot, with mandatory
  voltage admission and optional conservative qualification.

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

A closed seatbox is latched while the vehicle is `ready-to-drive`. Opening the
seatbox during that state does not disable an enabled battery or interrupt its
ON heartbeats, including across heartbeat restarts. On leaving `ready-to-drive`,
an open seatbox resumes normal seatbox-open handling. This protection is always
on, independently of the keep-active setting and experimental admission flags;
it does not override empty-pack, voltage-difference or explicit disable decisions.

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

The shared activation guard is always enabled, including when the experimental
setting is false, missing or unreadable. The flag enables higher-voltage selection, confirmed-OFF handovers and the
conservative stable-window qualification below; it cannot disable the basic
voltage gate. The operating modes are:

- Single battery: slot 0 is eligible; slot 1 is commanded off. Its possible
  live-supply state is still tracked until OFF is confirmed.
- Ordinary dual battery: slot 0 starts first when both packs are inactive.
  A compatible second pack can join; a mismatched incoming pack stays off in
  either insertion order without displacing the running supply.
- Experimental admission: a sole pack in either slot can run. With two packs,
  voltage takes priority, SOC breaks a voltage tie, and compatible packs can
  run together. A higher-voltage incompatible pack can replace a lower-voltage
  running pack through a confirmed-OFF handover.

The shared rules are:

- Both readers share a gate for every ON command. Slot 0 breaks an exact tie
  in experimental voltage/SOC selection.
- Initial activation is allowed in `stand-by`, `waiting-seatbox`, or `parked`.
  A second pack can join only in `parked` by default; a separate stopped-vehicle
  exception is described below.
- A mismatched second pack cannot be added alongside the running supply.
  Stale readings alone do not withdraw a confirmed supply's heartbeat.
- OFF commands still follow seatbox, empty-pack and disable handling. An OFF
  attempt needs a later inactive reading before ON is allowed again.

Admission uses complete NFC reads with a frame change in the last 10 seconds,
not cached Redis voltage. Missing, stale, truncated or substituted data cannot
admit a pack. Shared sampling rounds prompt both reader FSMs at most every 3 seconds
without interrupting commands. With both readers configured for active roles,
rounds are attempted regardless of vehicle state, including `stand-by`, before
and after both packs become active. Sampling alone does not authorize activation;
the second-pack gate remains parked-only unless the stopped-vehicle exception is
explicitly enabled.
The baseline guard requires a fresh paired acquisition within the configured
voltage limit before a second pack can join. Pending second-pack ON retries
recheck admission and retain their reservation even when a write fails.
With conservative qualification enabled, a second pack needs three paired
samples over 6 seconds: successful NFC acquisition age ≤10 seconds,
STATUS0 acquisition skew ≤1 second, absolute current ≤1000 mA, and voltage
variation ≤100 mV per pack.
Conservative qualification also requires the voltage difference to stay within
`scooter.max-voltage-delta` minus 10% (capped at 100 mV). The baseline accepts the
configured limit itself. A limit of 0 disables only the voltage check.
Inactive-role readers remain sampled so disabling a slot cannot hide a live
supply or discard an outstanding OFF command.

A newly selected tag needs one observed payload change to confirm that its
memory has been refreshed; the opposite-side tag can hold an old snapshot.
After that per-identity confirmation, unchanged measurements remain valid when
complete NFC reads succeed. Read freshness expires when acquisition stops or
fails, not when voltage, current or temperature stays constant. Frame-change age
is diagnostic and is used separately to resolve uncertain commands; it is not a
BMS measurement timestamp.

ON writes reserve the slot before I/O. Failed writes and lost tags do not prove
the pack is off. Reservations need inactive readings to clear; unconfirmed ON
retries stop after 10 seconds. An unchanged inactive payload cannot clear an
uncertain ON or OFF: changed inactive evidence after the relevant command and
its settling delay is required. Tag changes retain unresolved power reservations
and their settling deadlines. Admission state is not persisted across restarts.

If a fresh paired observation finds both packs already active above the limit,
`parked`, `stand-by` and `waiting-seatbox` permit isolation of one pack. The
healthy higher-voltage running pack is retained; the other is selected once and
commanded OFF. Voltage ordering changes cannot reverse that selection. Every OFF
write and retry requires fresh confirmation that the retained pack is active.
A failed or ambiguous OFF remains reserved until a subsequent inactive reading
confirms it. This is separate from the user enable setting and seatbox latch.
Experimental handovers also permit a healthy, higher-voltage inactive pack to
replace the sole running pack after both readings settle. The lower pack must
be positively OFF before the target receives ON. A removed target with no ON
reservation releases the completed handover so the old pack can resume.
Handovers and corrective OFF commands use `parked`, `stand-by` or
`waiting-seatbox`, not `ready-to-drive`. The stopped-motion exception below
permits parallel admission, not a power-interrupting handover.

If driving begins during an uncertain handover OFF, fresh post-attempt ACTIVE
proof can maintain the old sole supply while the target remains confirmed
inactive. Its OFF reservation is retained; another OFF after that keepalive
and subsequent inactive confirmation are required before the target can start.
This avoids treating a paused handover as permission to activate both packs.

### Two-pack decision diagnostics

While both packs have observations, the service logs decisions when their reason
changes and at least every 30 seconds at INFO. `--log=4` logs each 3-second poll;
it does not require `--debug` or verbose NFC protocol logging. Slot-1 reader
logging can remain disabled with `--log1=0`.

`Dual-pack decision` records include both voltages and currents, charge, faults,
vehicle state, observation and frame-change ages, paired round, sample counts,
stability, reservations, ON/OFF ages and pending OFF confirmations. They show:

- `command_on`: permission for the current command, including maintaining an
  existing supply; this is not evidence that a new pack would qualify.
- `baseline_new_on`: whether the same measurements would admit this slot as a
  new supply under the mandatory gate, with a reason.
- `strict_new_on`: the corresponding conservative qualification decision.

Each preview uses an independent snapshot and never changes live reservations,
qualification history or sampling state. Actual activation decisions and
isolation OFF deferrals are logged separately by the readers.

### Experimental stopped-vehicle admission

`experimental.battery.dual-pack-admission-while-stopped` defaults to **false**.
It is read at startup, requires `experimental.battery.dual-pack-admission=true`,
and also requires a service restart to change. It permits second-pack admission
in `ready-to-drive` only under all of these additional conditions:

- The peer is a confirmed running supply with no pending OFF command. This
  exception cannot start the first supply in `ready-to-drive`.
- Fresh ECU motion observations show zero RPM, zero raw speed and throttle off
  throughout the stable sampling window. Zero whole-km/h speed alone is not
  sufficient. A receive gap longer than 500 ms, movement, throttle-on, transport
  error or receive-queue overflow requires a new qualification window.
- Three successful paired acquisitions span at least 6 seconds after the current
  stopped/throttle-off condition began. Existing low-load, voltage-span,
  voltage-difference and NFC acquisition freshness checks still apply.
- The latest pair's earliest STATUS0 start is at most 3 seconds old. An ECU
  observation acquired after both complete battery frames must confirm stopped and
  throttle-off, and must be at most 500 ms old when the ON write is attempted.

The gate is checked immediately before every command write attempt, including
arbiter retries. An ON reservation consumes its pairing evidence; an unconfirmed
RTD activation needs a new qualified window before another ON attempt. A failed
gate never substitutes OFF or releases a supply reservation. Already-confirmed
running packs continue their heartbeats even if motion telemetry disappears.

When enabled, the service subscribes to direct values on `engine-ecu:motion`.
Battery-service opens no CAN socket and contains no ECU frame decoder. The
[versioned motion contract](MOTION_IPC.md) carries complete observations with
source/sequence identifiers and kernel receive timestamps from ecu-service's
existing receiver. Missing fields, stale/future observations, sequence loss,
reconnects and clock discontinuities restart qualification. Cached `engine-ecu`
hash values cannot enable this gate. An older ECU service without this channel
leaves RTD admission unavailable; ordinary parked admission remains unchanged.
Remove any obsolete `--admission-can-device` unit argument before upgrading.

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
Busy readers may miss rounds; heartbeats never wait for the peer. Sampling does
not bypass OFF confirmation, wake a stopped reader or prevent service shutdown.

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
starting the first supply. Single-reader configurations do not create rounds.
With both readers enabled, inactive-role packs also participate so that a role
change does not conceal a possible supply or outstanding OFF confirmation.

Service debug logs emit at most one round summary per scheduler tick: round ID,
result, conservative skew, STATUS0 durations and full-frame durations from
STATUS0 start. Missing observations have zero durations. Sampling adds no public
Redis status fields.

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
5. With both packs active, record ongoing rounds and traffic for an extended run,
   including `stand-by`. Confirm that sampling continues across vehicle states
   while second-pack admission remains parked-only with the stopped-vehicle
   exception disabled.
   Compare NFC data against independently timestamped load/voltage changes to
   characterize tag refresh latency and inter-pack measurement age. Report those
   separately from the NFC acquisition bound; do not claim measurement synchronization
   from successful rounds alone.
6. On the protected bench, separately enable the stopped-vehicle setting. Verify
   RTD admission only after a full stopped/throttle-off stable window and a fresh
   pair. Exercise nonzero RPM with zero rounded speed, throttle pulses during
   reads and retries, stale/queued CAN frames, receive gaps, bus errors, missing
   peer power confirmation and OFF pending. Verify the pending pack stays off
   when denied and running supplies keep heartbeating. Repeat with the setting
   disabled to confirm the default parked-only gate.

## Build and test

```bash
make build         # Linux ARMv7 binary: bin/battery-service
make build-host    # local-development binary: bin/battery-service-host
make build-native  # local-development binary: bin/battery-service
make test
make lint          # requires golangci-lint
```

The production-policy simulation covers 216 mode/insertion-order/voltage/SOC
combinations, startup states and a disabled voltage limit, parked/stopped
qualification, sag, load, faults, confirmed-OFF handovers and target removal.
It also runs 256 reproducible seeds of 256 events involving lost reads, command
failures, role/threshold changes, tag churn and simulated heartbeat expiry.
Constant-value acquisitions are exercised without artificial sensor noise,
including cold start, parallel admission, missing reads and uncertain-command
barriers. Failures include the event trace and seed. Run or reproduce a case with:

```bash
go test -race ./battery -run '^TestSimulation' -count=1
go test ./battery -run '^TestSimulationSeededEvents$/^seed42$' -count=1
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
