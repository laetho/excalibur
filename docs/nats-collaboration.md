# NATS shared boards and live collaboration

Excalibur combines durable shared storage and an ephemeral live message bus.
This is useful because the same NATS deployment can hold the canonical board
files and connect every application currently editing them, without a separate
collaboration service.

It is important to distinguish this protocol from a CRDT. The current protocol
provides live collaboration with whole-scene, last-update-wins semantics. NATS
does not merge Excalidraw documents or create CRDT behavior by itself.

## Architecture

```text
                         NATS server
              ┌─────────────────────────────┐
              │                             │
              │  JetStream Object Store     │
              │  bucket / board.excalidraw  │
              │       durable snapshot      │
              │                             │
              │  Core NATS subject          │
              │  wb.collab.bucket.board     │
              │    scenes and pointers      │
              │                             │
              └──────────┬───────┬──────────┘
                         │       │
                    ┌────┘       └────┐
                    ▼                 ▼
              Excalibur A      Excalibur B
```

The two NATS facilities have separate responsibilities:

| Facility | Purpose | Retained? |
| --- | --- | --- |
| JetStream Object Store | Stores the complete `.excalidraw` board | Yes |
| Core NATS pub/sub | Distributes live scene and pointer messages | No |

Object Store is the source used when opening or reloading a board. Core NATS is
the fast path between applications that already have that board open.

## Subject selection

Opening a remote board subscribes the application to:

```text
wb.collab.<bucket>.<board>
```

Characters outside letters, digits, `_`, and `-` are replaced by `-` before
constructing the subject. Applications collaborate only when they use the same
NATS server, bucket, and board identifier.

Local filesystem boards do not join a collaboration subject.

## Local edit flow

1. Excalidraw reports a drawing change.
2. The frontend debounces changes so it does not save on every pointer event.
3. It serializes the complete Excalidraw document, including elements, relevant
   app state, and embedded files.
4. The backend writes that document to the board's JetStream Object Store
   object.
5. The frontend also publishes a scene message to the board's collaboration
   subject:

```json
{
  "type": "scene",
  "json": "{ complete serialized .excalidraw document }"
}
```

The Object Store write makes the edit durable. The subject message makes it
visible immediately to connected peers.

## Remote edit flow

1. The Go backend receives a message from the board's NATS subscription.
2. It forwards the payload into the webview as a `canvas:collab` Wails event.
3. The frontend validates the message and parses the serialized scene.
4. It applies the remote elements without adding the replacement to local undo
   history.
5. It marks that update as remote so Excalidraw's resulting change event is not
   saved and published again.

This suppression is what prevents two applications from endlessly echoing the
same scene back and forth. The originating application is responsible for the
Object Store write; recipients use the live message for their current view.

## Pointer messages

Pointer movement uses the same subject but a smaller message:

```json
{
  "type": "pointer",
  "peerId": "anonymous-session-id",
  "x": 320,
  "y": 180,
  "tool": "pointer"
}
```

Each application maps these messages into Excalidraw's collaborator display.
Pointer messages are intentionally ephemeral and are never written to Object
Store.

## Opening a board and reconnecting

Core NATS does not replay old collaboration messages. A newly opened or
reconnected application therefore reads the latest complete board from Object
Store and then subscribes for new live messages. This snapshot-plus-live-update
pattern avoids requiring the pub/sub subject to retain history.

## Consistency and simultaneous edits

Shared storage does not merge JSON documents. If two applications independently
edit the same starting scene, each publishes and stores a complete replacement:

```text
Initial scene S0
  ├─ Application A adds a rectangle → scene S1
  └─ Application B adds a circle    → scene S2
```

Depending on message and storage-write ordering, `S1` may replace `S2` or `S2`
may replace `S1`. Unrelated concurrent additions are not guaranteed to survive.
The last Object Store write is the durable board state, while the most recently
applied live scene is what an open application displays.

NATS preserves message order from an individual publisher, but that does not
provide a deterministic merge of concurrent scenes from different publishers.
Two applications can temporarily disagree until another edit or reload causes
one complete scene to win.

This behavior is best described as **whole-scene last-update-wins**, not CRDT
convergence.

## What Object Store provides—and does not provide

Object Store provides:

- durable board snapshots;
- shared access from multiple Excalibur installations;
- JetStream-managed object metadata and storage;
- a stable state from which late joiners can start.

It does not provide:

- JSON field or Excalidraw element merging;
- conflict-free simultaneous editing;
- operation ordering across independent writers;
- deletion tombstones or logical clocks.

Those behaviors must be defined by the application protocol.

## Path to a genuine CRDT

A future CRDT protocol can retain the same overall NATS architecture:

1. Give operations stable peer and operation identifiers.
2. Send element-level changes rather than complete scene replacements.
3. Attach Lamport timestamps, version vectors, or another causal ordering
   mechanism.
4. Represent deletions with mergeable tombstones.
5. Deduplicate and deterministically merge concurrent operations.
6. Put operations in a retained JetStream stream so disconnected peers can
   replay what they missed.
7. Periodically compact the merged state into an Object Store snapshot.

Object Store would remain useful for snapshots, while a JetStream operation
stream would provide history and recovery. The CRDT implementation—not NATS or
shared storage by itself—would guarantee that every application eventually
converges on the same board.
