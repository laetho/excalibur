<p align="center">
  <img src="excalibur.svg#gh-light-mode-only" alt="Excalibur logo" width="180">
  <img src="excalibur-dark.svg#gh-dark-mode-only" alt="Excalibur logo" width="180">
</p>

# Excalibur — The board of legend

A Wails v3 desktop wrapper for Excalidraw with local `.excalidraw` boards, NATS JetStream Object Store sources, and live collaboration over NATS subjects.

## NATS collaboration

Remote boards deliberately combine two NATS features: JetStream Object Store
provides the durable shared `.excalidraw` document, while Core NATS subjects
provide immediate scene and cursor updates to applications that currently have
the board open. Shared storage does not merge JSON, and NATS does not supply a
CRDT automatically; the current protocol sends complete scenes and therefore
has last-update-wins conflict behavior.

See [docs/nats-collaboration.md](docs/nats-collaboration.md) for the architecture,
message flow, consistency behavior, and how this could evolve into a genuine
operation-based CRDT.

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

## Dramallama AI assistant

Enable the canvas chat overlay in Settings and configure the URL and model for
your OpenAI-compatible Dramallama API. The URL may be a server root, a `/v1`
base URL, or the full `/chat/completions` endpoint. API keys are optional.

The chat harness advertises `get_canvas_file` for reading the current complete
`.excalidraw` JSON (including embedded files) and `apply_canvas_operations` for
granular add/update/delete element edits. The model must inspect the current
file first; edits are validated, limited to 100 operations, rejected if the
user switches canvases, and applied through Excalidraw's normal undo, save, and
collaboration paths. Canvas data is sent only when the model calls the read
tool. The harness supports iterative inspect/edit cycles for larger requests,
with a bounded tool budget and a final tool-free summary turn to prevent runaway
loops. The overlay keeps chat local to the current app session.

For direct edit requests, the harness forces an inspect-then-edit tool sequence
instead of accepting a printed JSON “blueprint” as a successful canvas change.
Models that ignore the required tool are retried and ultimately return an
explicit error without claiming that the canvas was modified.

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
