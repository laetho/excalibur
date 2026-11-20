# AGENTS.md

## Project overview

Excalibur is a Wails v3 desktop wrapper for Excalidraw. It supports local
`.excalidraw` boards, remote boards stored in NATS JetStream Object Store, live
collaboration over NATS, and an optional AI assistant ("Dramallama") that can
read and edit the canvas through tool calls.

- **Backend:** Go (module `excalibur`, Go 1.26+), Wails v3, `nats.go`, `fsnotify`.
- **Frontend:** React 19 + TypeScript + Vite in `frontend/`, using `@excalidraw/excalidraw`.
- **Tooling:** [Task](https://taskfile.dev/) (`Taskfile.yml`), pnpm (`frontend/pnpm-lock.yaml`).

## Layout

| Path | Purpose |
| --- | --- |
| `main.go` | App entry point; embeds `frontend/dist`. |
| `app.go` | Wails service (`App`) exposed to the frontend. |
| `store.go`, `store_sources.go`, `models.go` | Board store, sources (local folders / NATS buckets), shared types. |
| `filemanager.go` | Morgan le Fayl: listing, thumbnails, and file transfer within the boards folders. |
| `nats.go` | NATS JetStream storage and collaboration (outbox, replay, snapshots, presence). |
| `ai.go` | Dramallama chat harness and canvas tools. |
| `version*.go` | Version info; `version_dev.go` / `version_production.go` are selected by the `production` build tag. |
| `cmd/devnats/` | Local JetStream-enabled NATS server for development. |
| `frontend/src/` | React UI (`Shell.tsx`, `Canvas.tsx`, `backend.ts`, `ipc.ts`, ...). |
| `frontend/src/FileManager.tsx` | The Morgan le Fayl two-pane panel (list/grid, drag & drop, dialogs). |
| `frontend/src/NewBucketDialog.tsx` | Creates a bucket on a NATS server (sidebar and Morgan le Fayl). |
| `frontend/bindings/` | Generated Wails bindings. Do not edit by hand. |
| `build/` | Wails platform build config and Taskfiles. `build/config.yml` holds the app version. |
| `docs/nats-collaboration.md` | Collaboration architecture. Read before changing `nats.go`. |

## Commands

```sh
task dev           # Wails dev mode plus local NATS on nats://127.0.0.1:42222
task dev:nats      # Only the local NATS server
task test          # Builds the frontend, then runs go test ./...
task build         # Build for the host OS
task build:linux | build:macos | build:windows

cd frontend && pnpm lint    # oxlint
cd frontend && pnpm build   # tsc -b && vite build
```

`main.go` embeds `frontend/dist`, so build the frontend before running
`go test ./...` on a clean checkout. `task test` does this for you.

## Conventions

- Everything in the repo is in English: code, comments, commits, and docs.
- Go: format with `gofmt`. Keep tests in `*_test.go` next to the code they test.
- Frontend: use pnpm, not npm or yarn. Follow the existing style in `frontend/src`.
- After changing exported `App` methods or models, regenerate the bindings
  (this happens during the frontend build) and do not hand-edit `frontend/bindings/`.
- Keep the README and `docs/nats-collaboration.md` up to date when behavior changes.
- Do not commit build output (`bin/`, `frontend/dist/`, `.task/`, `.dev/`).

## Verification

Before committing, run:

```sh
task test
cd frontend && pnpm lint
```

## Git workflow

- **Commit and push directly to `master`. Do not create feature branches or pull requests.**
- Pull before starting: `git checkout master && git pull`.
- Use Conventional Commits (`feat:`, `fix:`, `chore:`, `refactor:`, `docs:`, `test:`, `style:`), written in the imperative and kept short.
- Push with `git push origin master` when the work is done and verified.
- CI (`.github/workflows`) builds all platforms on every push.

## Releases

Push an annotated `vX.Y.Z` tag (`git tag -a v0.1.0 -m "Excalibur v0.1.0"` and
`git push origin v0.1.0`). The workflow stamps the version, builds every
platform, and publishes a GitHub Release. Only tag when asked.
