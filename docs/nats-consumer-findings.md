# NATS consumer lifecycle findings

Date: 2026-11-17
Scope: NATS JetStream consumer behavior observed for the `excalibur-plattform`
account (NKey `ADL7UZ2FQDO2SWKZPJFJSCDTQEAKHL72XBJTHUVCQRIF446YSNHDMH5H`,
account pk `166`) on the production cluster. Observed through the Synadia
Insights telemetry database (`hx` schema).

This is a hand-off summary for whoever next touches `nats.go`. It describes a
consumer-lifecycle problem that shows up as transient JetStream health findings,
and the likely root cause in this codebase.

## TL;DR

The app creates a **new ephemeral, non-durable consumer on every connection /
reconnect** instead of reusing one durable consumer per logical stream. This
causes heavy consumer churn and transient ack-buildup findings that "self-heal"
because the consumer that tripped the check is replaced before the next scrape.
The fix is to reuse a single durable consumer per stream.

## Evidence

### Consumer churn

`hx.consumer_ident` records one row per consumer ever created. Instance counts
for the account over ~4 days:

| Stream | Consumer instances |
| --- | --- |
| `OBJ_excalibur-library` | 103 |
| `OBJ_Design` | 39 |
| `OBJ_Research` | 19 |
| `EXCALIBUR_CRDT_0072E6B934945D5B` | 12 |
| `EXCALIBUR_CRDT_CD74053C54816E47` | 5 |
| `EXCALIBUR_CRDT_979D6300FD9884FF` | 6 |

Every consumer is non-durable (`durable_name` empty), has a random name, and
expires after 5s (`inactive_threshold: 5s`). On `EXCALIBUR_CRDT_0072…` two
consumers were alive at the same time (`bU95oi2B` and `AdSPxqSV` reporting at
the same epochs) — i.e. two app instances / two connections each created their
own consumer.

### Transient ack-buildup findings

At 2026-11-16 12:51 UTC the insights checks flagged:

- `consumer-017` (Ack Pending Buildup) on `OBJ_Design` and `KV_excalibur-locks`
- `consumer-019` (Consumer ACK Floor Divergence) on `OBJ_Design`

As of the latest scrape **no consumer has `num_ack_pending > 0`**. The findings
cleared because the offending consumer was torn down and replaced, not because
the underlying issue was fixed. A fresh consumer starts consuming a backlog,
pending climbs while the app catches up, then the consumer is replaced and the
next one starts clean.

### Consumer configurations observed

- **CRDT streams** (`EXCALIBUR_CRDT_*`): `ack_policy: explicit`,
  `ack_wait: 30s`, `max_ack_pending: 1000`, `max_deliver: -1`,
  `deliver_policy: by_start_sequence`, push consumer, `flow_control: false`.
  These are where ack buildup actually applies.
- **Object stores** (`OBJ_*`): `ack_policy: none`, `flow_control: true`,
  `deliver_policy: last_per_subject`, `max_deliver: 1`. These are the object
  store's internal notification consumers — no ack involved.

## Likely root cause in this codebase

`nats.go` uses `jetstream.New(nc)` and the object store `Watch()` API. In
`watchLibrary` (around line 818) the watcher is created with
`store.Watch(nats.IgnoreDeletes())`, which mints a fresh ephemeral push consumer
on each call. The watcher is held in `c.libraryWatcher` and reset to `nil` on
close, so every reconnect (and every app instance) creates a new consumer. This
matches the churn pattern above.

## Recommended next steps

1. **Reuse a single durable consumer per logical stream.** Give each long-lived
   watcher a fixed `nats.Durable("...")` name (per stream, not per connection)
   so reconnects resume the same consumer and its position instead of minting a
   new one from `by_start_sequence`. For object store watchers this means
   managing the underlying consumer explicitly (or binding to a named durable)
   rather than relying on the implicit ephemeral consumer from `Watch()`.
2. **Create consumers once at startup**, not per operation or per reconnect
   callback.
3. **For the explicit-ack CRDT consumers:** ack every message on every path
   (including error/early-return paths), set `ack_wait` above worst-case handler
   time (30s is the current default), and keep `max_ack_pending` aligned with
   in-flight × processing time. Make handlers idempotent since `max_deliver: -1`
   means redelivery is expected after a crash.
4. **Re-verify** after the change: the consumer-instance counts per stream should
   collapse toward 1, and the `consumer-017` / `consumer-019` findings should no
   longer recur.

## How to reproduce / verify

- Consumer instances per stream:
  `SELECT s.name, count(*) FROM hx.consumer_ident ci JOIN hx.stream_ident s ON s.pk = ci.stream_pk WHERE s.account_pk = 166 GROUP BY s.name;`
- Current ack state:
  `SELECT s.name, c.name, c.num_ack_pending, c.num_redelivered FROM hx.consumers c JOIN hx.stream_ident s ON s.pk = c.stream_pk WHERE s.account_pk = 166;`

## Status

- **Applied:** the shared library mirror now binds to a durable push consumer
  per app run (`library-watch-<run>`) on the library stream instead of using
  Object Store `Watch()`. The ordered consumer that was rebuilt after every
  reconnect — the source of the 103 `OBJ_excalibur-library` instances — is gone,
  and the mirror now acks every message on every path.
- **Not applied:** per-join collaboration consumers stay ephemeral on purpose.
  Each join needs the replay window the renderer derives from the snapshot
  sequence, and the consumer is deleted when the canvas closes, so its churn
  stays bounded to one consumer per open canvas rather than one per reconnect.
  See `docs/nats-collaboration.md`.
- **Re-verify** after the next release by re-running the queries above: the
  `OBJ_excalibur-library` instance count should grow by roughly one per app run
  instead of continuously, and `consumer-017`/`consumer-019` should not recur on
  the CRDT streams.
