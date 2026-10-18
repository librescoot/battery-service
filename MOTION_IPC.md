# Fresh ECU motion observations

`ecu-service` publishes direct JSON values on Redis Pub/Sub channel
`engine-ecu:motion`. Existing `engine-ecu` hash fields and field-name notifications
are unchanged. This local-MDB contract is additive; it does not change other
services' CAN access or command interfaces.

## Version 1

A valid observation has all these fields, including explicit zero/false values:

```json
{"version":1,"source":"cd0ed924a45d4af18b65395fd632bd11","sequence":42,"valid":true,"observed_at":"2026-10-17T10:00:00.123456789Z","rpm":0,"raw_speed":0,"throttle_on":false}
```

- `source`: random 128-bit identifier, encoded as 32 lowercase hex characters.
  A receive reconnection, detected receive discontinuity or ECU power-generation
  change starts a new source.
- `sequence`: positive uint64 assigned before bounded queues can drop an
  observation. It increases within a source; gaps invalidate continuity.
- `observed_at`: UTC RFC3339Nano kernel reception time, not decode, publication
  or subscriber delivery time.
- `rpm`: unsigned 16-bit value; `raw_speed`: unsigned byte; `throttle_on`: boolean.
  All three come from one complete standard eight-byte motion frame.

Every motion frame yields an observation, including unchanged values. Other ECU
frames do not refresh motion. Invalidations contain no fabricated zero values:

```json
{"version":1,"source":"cd0ed924a45d4af18b65395fd632bd11","sequence":43,"valid":false,"reason":"rx-overflow"}
```

`reason` is diagnostic. Every invalidation ends qualification regardless of its
reason. Reasons include receive/queue overflow, malformed frames, missing or
invalid timestamps, clock change, expired dispatch/publication, powered-off,
transport error and silence. Consumers must expire evidence independently of
invalidations: neither a disconnected publisher nor a disconnected Redis server
can reliably deliver one.

## Delivery and freshness

The ECU's single SocketCAN socket supplies native kernel receive timestamps and
receive-overflow metadata. One receive pump feeds a bounded ordered dispatcher;
legacy Redis work runs on that dispatcher, not on the socket pump. A separate
bounded publisher has 100 ms network deadlines and no automatic retries of old
observations. Each queue holds at most 64 entries. Overflow discards queued work
and explicitly interrupts continuity. Buffered frames older than 500 ms are not
replayed as fresh controller traffic or motion.

Consumers reject unsupported versions, malformed/oversized messages (over 1024
bytes), absent/null required values, invalid numeric ranges, future timestamps,
and observations older than 500 ms. Initial subscription, reconnect, source
change, sequence gaps/duplicates/reordering, non-increasing observation time,
and invalidation restart qualification. Wall-clock versus monotonic-clock
changes exceeding 5 ms also interrupt continuity, including suspend/resume.
Clock conversion cannot extend an observation's lifetime.

There is no retained-message or cached-hash bootstrap. A fresh stopped frame
requires RPM zero, raw speed zero and throttle off together. It starts a new
window after discontinuity; it cannot continue the previous window.

## Battery admission

The optional stopped-RTD admission gate consumes these observations. It retains
all battery-side freshness, voltage/load, paired-round and reservation checks.
Its latest motion observation must be acquired at or after both complete battery
frames, not merely delivered after them. Motion loss blocks new optional RTD
admission, never the heartbeat of an established supply or ordinary parked
admission. An older ECU service without this channel leaves the exception
unavailable. The two existing experimental settings remain default-off.

The 500 ms budget needs cadence/jitter validation on each supported controller;
no controller broadcast rate is changed by this interface.

## Isolated cross-service test

Build ecu-service's test binary and pass its absolute path to battery-service:

```sh
# In the ECU checkout:
GOTOOLCHAIN=go1.25.7 go test -race -c -o /tmp/ecu-motion.test
# In the battery checkout:
ECU_MOTION_TEST_BINARY=/tmp/ecu-motion.test GOTOOLCHAIN=go1.25.7 \
  go test -race -count=1 -run '^TestMotionIPCIntegration$' ./battery
```

This launches isolated Redis on a temporary Unix socket. Kernel-timestamped Unix
datagrams carry CAN-shaped frames through the actual receive adapter, decoder,
JSON publisher and battery subscriber, with synthetic NFC acquisitions. The test
checks qualification, the post-pair barrier and disconnect behavior. It opens no
CAN interface and does not replace controller-specific hardware validation.
