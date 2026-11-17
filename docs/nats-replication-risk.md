# NATS replication risk and remediation

Date: 2026-11-17
Status: remediation implemented in `master` (`f7070c7`); this doc records the
risk, its source, and the plan so the decision and its rationale stay visible.

This is the R1-redundancy finding from
[`nats-consumer-findings.md`](nats-consumer-findings.md) (insights check
`stream-030`, "R1 Streams in Multi-Node Clusters"), tracked separately from the
consumer-lifecycle work.

## The risk

Every durable JetStream resource in the `excalibur-plattform` account was a
single-replica (R1) stream on a multi-node cluster. R1 means the stream's data
lives on exactly one server with **no redundancy**: if that server fails, the
stream is lost — there is no peer to fail over to. For a collaboration tool this
is the highest-impact availability gap because the affected resources hold the
only durable copy of board data:

- per-bucket collaboration update streams (`wb.crdt.v2.*`),
- board Object Store buckets (`OBJ_*`, including the shared `excalibur-library`),
- the `excalibur-locks` KV bucket.

Observed stream state (account pk `166`, cluster `nats`): all streams R1,
including `EXCALIBUR_CRDT_*`, `OBJ_*`, and `KV_excalibur-locks`.

## Source of the risk

The streams were created as R1 because the client did not request a replication
factor. `nats.go` created resources with the default (single-replica) config:

- `c.js.CreateObjectStore(&nats.ObjectStoreConfig{Bucket: bucket})` — no
  `Replicas` set, so JetStream defaulted to 1.
- `c.js.AddStream(&nats.StreamConfig{...})` for the collaboration stream — no
  `Replicas` set.
- `c.js.CreateKeyValue(&nats.KeyValueConfig{Bucket: lockBucket})` — no
  `Replicas` set.

JetStream does not expose the cluster size to clients, so the client could not
know whether more replicas were placeable; it simply accepted the default.

## The plan to correct it

Request a replication factor of **3** for all durable data, falling back to 2,
then 1, only when the cluster rejects the placement. A non-clustered server can
only ever host one replica and is left at 1. Existing streams created by older
builds are upgraded in place the next time they are opened.

Implementation (in `nats.go`):

- `replicaTarget = 3` and `replicaCandidates()` returning `[3, 2, 1]` for a
  clustered server, `[1]` otherwise.
- `withReplicas()` tries each candidate and stops on the first success; a
  placement rejection (`isReplicaError()`) falls through to the next candidate,
  while any other error is surfaced immediately.
- Object Stores are created with `Replicas` set and upgraded in place via
  `ensureStreamReplicas("OBJ_" + bucket)`.
- Collaboration update streams are created with `Replicas` set and upgraded in
  place inside `ensureCollabStream`.
- The `excalibur-locks` KV bucket is intentionally **excluded**: it uses memory
  storage and a single replica because its leases and snapshot index are
  coordination state, not board data. A restart drops the leases (clients elect
  a new compactor) and the snapshot index (which rebuilds from the immutable
  snapshot objects). This is a deliberate exception and is expected to keep
  tripping `stream-030`; it is not a data-loss risk.

## Verification

- `TestIsReplicaError` covers the placement-error detection used by the fallback.
- `TestReplicaCandidates` covers the fallback order for clustered vs.
  non-clustered servers.
- After the fix, the per-stream replica count should be 3 (or 2/1 only where the
  cluster cannot place more). Confirm with:
  `SELECT name, num_replicas FROM hx.streams WHERE account_pk = 166;`
- `stream-030` should stop firing for `OBJ_*` and `EXCALIBUR_CRDT_*`; it will
  continue to fire for `KV_excalibur-locks` by design.
