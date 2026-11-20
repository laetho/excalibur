<p align="center">
  <img src="excalibur.svg#gh-light-mode-only" alt="Excalibur logo" width="180">
  <img src="excalibur-dark.svg#gh-dark-mode-only" alt="Excalibur logo" width="180">
</p>

# Excalibur — The board of legend

A Wails v3 desktop wrapper for Excalidraw with local `.excalidraw` boards, NATS JetStream Object Store sources, and live collaboration over NATS subjects.

## NATS collaboration

Remote boards use NATS as both their shared storage and collaboration transport,
without a separate collaboration server:

- a per-bucket JetStream stream retains versioned element updates;
- JetStream Object Store holds immutable snapshots, images, oversized updates,
  and an ordinary `.excalidraw` materialization;
- JetStream KV elects one snapshot compactor and indexes the active snapshot;
- one shape library is shared by all boards and buckets on the same server and
  syncs live between clients;
- Core NATS carries only ephemeral pointer presence; and
- a persistent local outbox makes edits durable before remote replication, so
  drawing and board navigation remain safe while the server is unavailable.

Clients restore the latest snapshot and replay retained updates in order.
Excalidraw's element versions and reconciliation rules merge concurrent drawing
changes; snapshots preserve deletion tombstones, while shared canvas background
changes use deterministic Lamport-style version metadata. The UI reports
`Synced` only after local outbox replication and ordered replay/application have
both completed.

See [docs/nats-collaboration.md](docs/nats-collaboration.md) for the architecture,
subjects and storage layout, durability model, join/replay flow, compaction, and
consistency behavior.

## Development

Requirements: Go 1.26+, Node.js, pnpm, [Task](https://taskfile.dev/), Wails v3, and its platform dependencies.

```sh
go install github.com/wailsapp/wails/v3/cmd/wails3@v3.1.0
cd frontend && pnpm install
cd .. && task dev
```

Build for the host with `task build` and run checks with `task test`.

`task dev` starts both Wails and a local JetStream-enabled NATS server at
`nats://127.0.0.1:42222`, with persistent development data in `.dev/nats`.
Run only the server with `task dev:nats`. An already-running local server is
reused after verifying that JetStream is available. Add the local URL in Settings; NATS
username/password or a standard `.creds` credentials file are optional. Credentials-file
contents remain on the native side and are loaded directly by the NATS client.
Set `EXCALIBUR_DEV_NATS_PORT` to use another development port. The former
`EXCALIWAILS_DEV_NATS_PORT` name remains supported for compatibility.
`task run` also starts or reuses this server while running the built application.

## Merlin assistant

Enable Merlin in Settings and configure the URL and model for your
OpenAI-compatible API. A floating Merlin button with a sparkle icon at the top of the canvas toggles Merlin's
expandable panel beside the canvas, so the panel never covers the drawing. The
URL may be a server root, a `/v1` base URL, or the full `/chat/completions`
endpoint. API keys are optional.

The chat harness advertises `get_canvas_file` for reading the current complete
`.excalidraw` JSON (including embedded files) and `apply_canvas_operations` for
granular add/update/delete element edits. The model must inspect the current
file first; edits are validated, limited to 100 operations, rejected if the
user switches canvases, and applied through Excalidraw's normal undo, save, and
collaboration paths. Canvas data is sent only when the model calls the read
tool. The harness supports iterative inspect/edit cycles for larger requests,
with a bounded tool budget and a final tool-free summary turn to prevent runaway
loops. The panel keeps chat local to the current app session.

For direct edit requests, the harness forces an inspect-then-edit tool sequence
instead of accepting a printed JSON “blueprint” as a successful canvas change.
Models that ignore the required tool are retried and ultimately return an
explicit error without claiming that the canvas was modified.

While a turn runs, the panel shows a single loader line naming the current step
("Reading the board", "Drawing", "Wrapping up") instead of the raw stream of
reasoning and tool output. The label switches before each step starts, so it
already says "Drawing" while the model is generating the edit rather than after
it lands. Reasoning stays available behind a collapsed "Thinking" disclosure
that toggles from anywhere on the card. Answers are capped to a short summary —
at most two sentences or three bullets — so the panel reports what changed
rather than repeating coordinates, IDs, or the operations that were sent.

The harness prompt also carries the diagram conventions Merlin follows when
asked to draw: a grid layout with 200-300 unit horizontal and 100-150 unit
vertical gaps (or a radial layout for mind maps), the `#a5d8ff` / `#b2f2bb` /
`#ffd43b` / `#ffc9c9` fill palette on `#1e1e1e` strokes, `fontFamily: 5` labels
bound to their containers, and per-type shape budgets. The budget is enforced,
not just advised: a diagram request whose operations exceed 24 element changes is
rejected by `apply_canvas_operations` with a message telling the model to redraw
a smaller version, so a request for fifteen classes becomes a readable subset
plus a list of what was left out.

Answers to questions get a structured form instead of a summary: a short
heading, real bullet lists, and emoji the model chooses for scanning. The panel
renders a small markdown subset (headings, bullets, numbered steps, bold, italic,
and inline code) itself, so the model never has to emit HTML and the chat needs
no markdown dependency.

## Morgan le Fayl

Morgan le Fayl is a two-pane file manager for the boards folders on this
computer and the NATS buckets in the sidebar. Toggle it with **⌘F** (View →
Morgan le Fayl) or the split-pane button in the sidebar. Each pane can show a
local boards folder or a bucket, and the panel is confined to those roots, so it
never reaches outside them.

In Settings → Boards, **Sidebar folders** controls how the sidebar opens folders
and buckets: **One at a time** (the default) opens only the active one, while
**Open freely** keeps several board lists open at once.

Drag a selection onto the other pane (or onto a folder row) to **move** it, or
hold **⌥/Alt** while dropping to **copy**. **Move over** and **Copy over** appear
in the panel header as soon as a pane has a selection and do the same without
dragging. The same actions are in the right-click menu, along with rename, new
folder, reveal, and trash. Drag and drop works across local and remote: a move
into a bucket copies the file and trashes the local source, while a move out of a
bucket deletes the remote object permanently (there is no Trash in a bucket, so
the right-click action is "Delete Permanently…" with a confirm dialog). Deleting
a local file moves it to the OS Trash, never removing it outright. Name
collisions prompt per-file Skip / Overwrite / Rename choices.

The folder button in a pane's header works inside that pane: in a local folder it
creates a subfolder there, and in a bucket it creates a new bucket on the same
server (a bucket is the only folder a NATS source has), then opens it. It only
asks for a name.

## Platform builds

Wails v3 can produce the desktop targets below. The macOS task uses Wails'
CGO-free backend so the universal app bundle can be cross-compiled:

```sh
task build:linux    # Linux AMD64
task build:macos    # macOS universal (Intel + Apple Silicon)
task build:windows  # Windows AMD64
```

Outputs are `bin/Excalibur`, `bin/Excalibur.app`, and
`bin/Excalibur.exe`, respectively. The GitHub Actions workflow also builds and
uploads each target on its native runner. Artifacts are unsigned; public
distribution still requires Apple Developer ID signing/notarization and Windows
code signing. A cross-compiled macOS bundle must be tested and signed on macOS.

## Releases

Push an annotated semantic-version tag to publish a GitHub Release:

```sh
git tag -a v0.1.0 -m "Excalibur v0.1.0"
git push origin v0.1.0
```

Release tags must have the exact form `vX.Y.Z`. The workflow uses the tag as the
native application version, builds all three desktop packages, generates
`SHA256SUMS`, and publishes the packages with automatically generated release
notes. The release is published only after every platform build succeeds.

To recover or publish an existing tag manually, run **Cross-platform build and
release** from the repository's Actions page and enter that tag in
`release_tag`. Manual releases always check out the existing tag itself; they do
not create tags from the branch selected in the workflow form.

NATS sources require a server with JetStream enabled.

## Copyright

Copyright © 2026 [github.com/laetho](https://github.com/laetho) and
[github.com/niklashaakonsen](https://github.com/niklashaakonsen).
