# Spot Show Static-First Viewer Upgrade

**Status:** Complete

## Implementation Checklist

- [x] Add structural validation and safe local-asset collection to the Show build pipeline.
- [x] Replace direct sandbox inspection with a message-based HTML iframe bridge.
- [x] Add a system/light/dark theme contract shared by the page, HTML blocks, and Mermaid.
- [x] Add card permalinks plus accessible fullscreen Mermaid and image views.
- [x] Improve code, terminal, diff, and Markdown rendering without changing existing documents.
- [x] Add the `trace` block, update agent-facing docs, and complete browser/CLI regression coverage.

## Overview

Spot Show turns one JSON document into a static report site. That is its useful
distinction from Sideshow: it is a portable deployment artifact with a stable
Spot URL, gallery preview, access policy, and no stateful report service.

Keep that model and adopt the strongest viewer and authoring ideas from
Sideshow: a real sandbox bridge, coherent theme tokens, fullscreen visual
surfaces, richer native rendering, local asset handling, stable anchors, and a
trace timeline. Do not add sessions, comments, revisions, per-block APIs, or an
MCP server in this effort.

The current renderer is emitted inline by `show_build` in `cli/spot`. The same
CLI is checked in at `sdk/spot` and then copied into Go's embedded static assets
by `just generate`. All changes must keep those copies synchronized.

## Settled Design Decisions

- Preserve `show.json -> static folder -> Spot deploy` as the complete runtime
  model. A deployed Show does not require database, realtime, AI, or other Spot
  SDK capabilities.
- Keep every existing valid Show document valid and visually recognizable.
  New fields and the `trace` kind are optional.
- Keep the generated report as plain HTML, CSS, and browser JavaScript. Add no
  new Go server API and no frontend framework.
- Continue treating HTML blocks as untrusted opaque-origin iframes. Never add
  `allow-same-origin` to make resizing easier.
- Use message passing for iframe height and theme changes. The parent accepts a
  message only when `event.source` is a registered block iframe and validates
  and clamps every value.
- Default the viewer to system color mode and offer explicit system, light, and
  dark choices persisted in `localStorage`.
- Use stable card fragment links. An explicit card `id` wins; otherwise derive a
  deterministic slug from its title and index, resolving duplicates predictably.
- Bundle local files referenced by `image` blocks. Remote HTTP(S), root-relative,
  and `data:` sources remain unchanged. Markdown-local-image discovery and
  arbitrary HTML dependency crawling are out of scope.
- Load syntax highlighting progressively only when code declares a language.
  Pin the browser dependency to an exact version and preserve a readable plain
  text fallback when it cannot load. Do not make basic report rendering depend
  on the CDN.
- Keep Mermaid's strict security mode, pin its version, and preserve source text
  in an error fallback.
- Do not add comments, retained revisions, sessions, sidebars, per-block CRUD,
  or an MCP transport in this plan.

## Goals

- Make HTML blocks size reliably as fonts, images, and interactive content
  change after load.
- Make every built-in block legible in system, light, and dark modes.
- Let users expand diagrams and images without losing their place in a report.
- Make individual cards addressable and shareable.
- Let a Show deploy screenshots and other local images without manual staging.
- Improve code-review and validation reports with line numbers, ANSI terminal
  output, better diffs, and a trace timeline.
- Catch malformed documents and missing local assets before deployment.
- Keep the POSIX shell CLI, installed single-file workflow, static deployment,
  gallery screenshots, and Cloudflare publishing behavior intact.

## Non-Goals

- Reimplementing the Sideshow workspace/session viewer.
- Browser-to-agent comments or anchored feedback.
- Persisting Show revisions or comparing historical deployments.
- Editing individual cards or blocks through server APIs.
- Adding a JavaScript build tool or framework to the Spot server.
- Crawling assets referenced from arbitrary Markdown or HTML.
- Turning a Show into a general-purpose Spot application.
- Redesigning the Spot gallery or site access-control model.

## Architecture and Data Flow

The build path remains:

```text
show.json
   |
   +-- validate document and local references
   +-- copy allowed local image assets, preserving relative paths
   +-- emit show.json + _spot.json + index.html
   |
   +-- deploy the generated directory
       +-- capture _screenshot.png
       +-- refresh open tabs through /spot-live.js
```

The generated page owns card layout, Markdown, JSON, diff, terminal, code,
image, trace, and Mermaid rendering. HTML remains isolated in sandboxed
`srcdoc` frames. Each HTML frame receives a small trusted wrapper around the
user fragment:

- Spot Show CSS custom properties and base typography;
- a resize reporter based on `ResizeObserver`, load, font readiness, and bounded
  delayed measurements;
- a listener for validated theme-token messages from the parent;
- a message type marker and per-frame nonce so unrelated page messages cannot
  masquerade as bridge traffic.

The parent registers each iframe's `contentWindow`, sends the current resolved
theme after load and on mode changes, and applies reported heights with a
minimum and maximum bound. It must remove registrations and listeners when a
card is replaced or the page unloads. A single Show currently renders once, but
the lifecycle should not leak if live reload behavior changes later.

Mermaid blocks remain parent-rendered. Store each block's source and target so a
theme change can render it again with a fresh unique diagram ID. HTML frames
receive updated tokens without being recreated, preserving their interactive
state.

## Backward-Compatible Schema Additions

### Cards

```json
{
  "id": "validation",
  "title": "Validation",
  "blocks": []
}
```

- `id`: optional string matching `[A-Za-z0-9][A-Za-z0-9_-]{0,63}`.
- Duplicate explicit IDs are validation errors.
- Missing IDs are derived at render time and do not rewrite `show.json`.
- The rendered DOM ID is `card-<id>` and the canonical link is `#card-<id>`.

### Code

```json
{
  "kind": "code",
  "title": "handlers.go",
  "language": "go",
  "line_start": 80,
  "body": "func handle() {}"
}
```

- `line_start`: optional positive integer; defaults to `1`.
- Render line numbers without making them part of copied source text.
- Highlight known languages when the pinned highlighter is available; otherwise
  use escaped plain text.

### Diff

```json
{
  "kind": "diff",
  "layout": "split",
  "body": "diff --git ..."
}
```

- `layout`: optional `unified` or `split`; defaults to `unified`.
- Split mode may fall back to unified for an unparseable patch and must show a
  non-blocking explanation rather than dropping content.

### Terminal

```json
{
  "kind": "terminal",
  "title": "Tests",
  "cols": 100,
  "body": "\u001b[32mok\u001b[0m"
}
```

- `cols`: optional positive integer with a documented, validated upper bound.
- Interpret ANSI SGR styling only. Escape all text and ignore cursor-addressing
  or unsupported control sequences.

### Trace

```json
{
  "kind": "trace",
  "title": "Agent run",
  "steps": [
    {
      "label": "Inspect renderer",
      "kind": "tool",
      "status": "done",
      "detail": "Read cli/spot",
      "ts": "10:42:03"
    }
  ]
}
```

- `steps`: required non-empty array for `trace`.
- Every step requires `label`.
- `kind`, `detail`, and `ts` are optional strings.
- `status` is optional and limited to `pending`, `running`, `done`, and `error`.
- Details are collapsed initially; the active/error state remains visually
  scannable without expansion.

### HTML Theme Contract

HTML fragments may use these stable variables:

```css
--spot-bg
--spot-panel
--spot-panel-muted
--spot-line
--spot-text
--spot-muted
--spot-accent
--spot-accent-secondary
--spot-good
--spot-warn
--spot-bad
--spot-font-sans
--spot-font-mono
```

The wrapper sets `color-scheme`, body background/text defaults, box sizing, and
font variables. User styles may override the defaults. Existing HTML fragments
that provide all their own CSS continue to render as before.

## Validation and Error Behavior

Add `spot show validate [show.json]`. `show build`, `show deploy`, and each watch
iteration run the same validator before replacing output or deploying.

Validation errors use JSON paths and actionable messages:

```text
show.json: cards[1].blocks[0].src: local file not found: screenshots/result.png
```

Errors include:

- invalid JSON or a non-object top level;
- missing or non-string `title`;
- missing or non-array `cards`/`blocks`;
- unsupported block kinds;
- missing or incorrectly typed kind-specific fields;
- invalid/duplicate explicit card IDs;
- invalid `line_start`, `cols`, `layout`, trace status, or trace step shape;
- local image paths that are missing, not regular files, or escape the Show
  document directory after symlink resolution.

Unknown optional fields produce warnings, not errors, so future producers and
older CLIs can coexist. Remote asset availability is not checked at build time.

Implement validation and asset discovery in the existing embedded Python path.
Show validation and asset-aware builds require `python3`; fail with a concise
installation message when it is unavailable rather than silently producing an
incomplete deployment. `show init` and unrelated Spot CLI commands remain
usable without Python.

The build must finish validation and asset discovery before removing or
replacing its output directory. A failed watch iteration leaves the last good
deployment live and continues watching after printing the validation error.

## Implementation Steps

### Task 1: Validator and safe asset collection

**Files:**

- Modify: `cli/spot`
- Modify: `sdk/spot`
- Modify: `scripts/cli-smoke.sh`
- Modify: `sdk/spot-show-schema.md`

- [x] Add `show validate [show.json]` to CLI usage and command dispatch.
- [x] Refactor the current Python metadata reader into one validator that emits
  normalized metadata and a machine-readable list of local image assets.
- [x] Validate the existing schema plus the additive fields defined above.
- [x] Resolve local image paths relative to the source document, reject symlink
  or `..` escapes, and copy regular files while preserving safe relative paths.
- [x] Leave HTTP(S), root-relative, and `data:` image sources untouched.
- [x] Validate before deleting an existing build directory.
- [x] Make watch failures non-destructive and retryable.
- [x] Add CLI smoke cases for valid legacy input, every new field, malformed
  structures, duplicate IDs, missing assets, path traversal, symlink escape,
  successful nested asset copying, and output preservation on failure.
- [x] Synchronize `cli/spot` to `sdk/spot`, run `just generate`, and verify no
  embedded-asset drift.

**Validation:**

```sh
sh -n cli/spot sdk/spot
./scripts/cli-smoke.sh
just check-generate
```

### Task 2: Sandboxed HTML bridge and theme contract

**Files:**

- Modify: `cli/spot`
- Modify: `sdk/spot`
- Modify: `scripts/cli-smoke.sh`
- Modify: `sdk/spot-show-schema.md`
- Modify: `sdk/spot-agent-howto.md`

- [x] Generate an HTML-frame wrapper containing Spot theme tokens and the
  trusted resize/theme bridge around the user fragment.
- [x] Register frames in the parent by `contentWindow`; validate bridge marker,
  nonce, message type, and payload before acting.
- [x] Report height after load, font readiness, image completion, resize
  observation, and bounded delayed settling passes.
- [x] Clamp parent-applied height and guard against rapid two-height feedback
  loops without clipping the larger settled content.
- [x] Add system/light/dark mode selection and persist the selected mode.
- [x] Update system-mode reports live when the OS preference changes.
- [x] Send resolved theme tokens to loaded HTML frames without recreating them.
- [x] Keep the iframe opaque-origin sandbox and document the HTML variable
  contract in both the schema and agent how-to.
- [x] Add regression fixtures for short, tall, asynchronously growing, image,
  and intentionally oscillating HTML content.

**Validation:**

```sh
./scripts/cli-smoke.sh
just check-generate
just test
```

Manually verify the iframe fixtures in Chromium in all three theme modes and
confirm no frame remains at the seed height or enters a resize loop.

### Task 3: Card permalinks and fullscreen visuals

**Files:**

- Modify: `cli/spot`
- Modify: `sdk/spot`
- Modify: `scripts/cli-smoke.sh`
- Modify: `sdk/spot-show-schema.md`

- [x] Render explicit or deterministic card IDs and update the URL fragment as
  a copied card link without disturbing normal scrolling.
- [x] Add a keyboard-accessible copy-link control to each card.
- [x] Scroll and focus the requested card on initial fragment navigation after
  async block heights settle.
- [x] Add fullscreen controls to Mermaid and image blocks using an accessible
  modal with focus trapping/restoration, Escape close, and backdrop close.
- [x] Preserve image alt/caption content and meaningful diagram titles in the
  fullscreen view.
- [x] Re-render Mermaid on resolved theme changes, pin Mermaid to an exact
  version, set strict security mode explicitly, and keep source/error fallback.
- [x] Add mobile layout and reduced-motion treatment for the modal and controls.
- [x] Add smoke fixtures for duplicate derived slugs, explicit IDs, fragments,
  fullscreen controls, and Mermaid error fallback.

**Validation:**

```sh
./scripts/cli-smoke.sh
just check-generate
just test
```

Manually verify mouse, keyboard, fragment reload, browser back/forward, and
light/dark Mermaid behavior in Chromium.

### Task 4: Rich Markdown, code, terminal, and diff rendering

**Files:**

- Modify: `cli/spot`
- Modify: `sdk/spot`
- Modify: `scripts/cli-smoke.sh`
- Modify: `sdk/spot-show-schema.md`
- Modify: `examples/spot-show/show.json`

- [x] Add reusable line-number rendering for `code`, including `line_start` and
  copyable source text.
- [x] Pin and lazily load the selected syntax highlighter only for declared
  languages; keep escaped plaintext fallback and show no blocking error.
- [x] Apply the same highlighting path to language-tagged Markdown fences.
- [x] Parse and render ANSI SGR terminal styles while stripping unsupported
  controls and preserving carriage-return progress output sensibly.
- [x] Add terminal chrome/title and validated `cols` sizing without forcing the
  report wider than its card.
- [x] Improve unified diff file/hunk structure and add optional split layout.
- [x] Fall back safely to unified/raw rendering for malformed patches.
- [x] Ensure every renderer uses text nodes or escaped output for user data.
- [x] Extend the demo Show to exercise highlighting, non-1 line starts, ANSI,
  unified/split diffs, and dark mode.
- [x] Add smoke cases for HTML-like payloads, unknown languages, ANSI edge
  cases, malformed diffs, and CDN failure fallback.

**Validation:**

```sh
./scripts/cli-smoke.sh
just check-generate
just test
```

### Task 5: Trace block

**Files:**

- Modify: `cli/spot`
- Modify: `sdk/spot`
- Modify: `scripts/cli-smoke.sh`
- Modify: `sdk/spot-show-schema.md`
- Modify: `sdk/spot-agent-howto.md`
- Modify: `examples/spot-show/show.json`

- [x] Add `trace` to validation, CLI help, schema, and agent block vocabulary.
- [x] Render steps as a semantic ordered timeline with status, kind, timestamp,
  and expandable detail.
- [x] Make running/error state distinguishable without relying on color alone.
- [x] Keep long details scrollable and copyable without expanding card width.
- [x] Add trace examples and malformed/empty/large trace test cases.
- [x] Verify compact cards and mobile layouts remain readable with traces.

**Validation:**

```sh
./scripts/cli-smoke.sh
just check-generate
just test
```

### Task 6: Documentation, security review, and final verification

**Files:**

- Modify: `README.md`
- Modify: `CHANGELOG.md`
- Modify: `sdk/spot-show-schema.md`
- Modify: `sdk/spot-agent-howto.md`
- Regenerate: `server/static_assets/sdk/*`

- [x] Document `show validate`, local asset rules, card IDs/links, theme modes,
  fullscreen controls, renderer fields, trace, and progressive CDN fallback.
- [x] Update the starter generated by `spot show init` without making it so large
  that it stops being a useful agent prompt.
- [x] Review iframe sandbox flags, message-source/nonce validation, URL handling,
  Mermaid configuration, external links, and generated HTML injection surfaces.
- [x] Confirm Show source contains no credentials or newly persisted browser
  data beyond the display-mode preference.
- [x] Confirm legacy Show documents build without edits and retain their block
  order and compact-card behavior.
- [x] Run the complete repository validation suite and deploy the example to a
  local stack for a final browser pass.

**Validation:**

```sh
just build
just test
just check-generate
just e2e
just deploy-show-demo
git diff --check
```

## Testing Strategy

- **CLI unit/smoke:** extend `scripts/cli-smoke.sh` for parsing, validation,
  asset paths, output safety, generated file contents, deploy, and watch.
- **Renderer fixtures:** keep small committed Show fixtures covering every block
  kind and failure fallback. Prefer fixture documents over fragile assertions on
  the full generated HTML string.
- **Browser regression:** use headless Chromium already required for gallery
  screenshots to load built fixtures and capture console errors, final DOM
  markers, iframe heights, and screenshots. If browser interaction cannot be
  made deterministic without a new dependency, keep focus/fullscreen checks as
  a documented manual gate rather than adding a frontend framework solely for
  tests.
- **Security:** cover path traversal/symlink escape, malicious strings in every
  renderer, forged bridge messages, extreme height values, unsafe link schemes,
  and malformed Mermaid/diff/ANSI input.
- **Compatibility:** build the current example and representative pre-upgrade
  documents before and after the change; additions must not require migration.

## Acceptance Criteria

- Existing valid Show files build and deploy without modification.
- Invalid structure or missing/escaping local image assets fail before output
  replacement or deployment.
- A local image referenced by a Show is present at the same safe relative path
  in the built and deployed site.
- Sandboxed HTML frames grow and shrink with their content without
  `allow-same-origin`, clipping, runaway resizing, or accepting unrelated page
  messages.
- System, light, and dark mode produce coherent page, HTML, Mermaid, code, diff,
  terminal, JSON, image, and trace output.
- Theme switching preserves interactive HTML-frame state.
- Cards have stable fragment URLs, copied links reopen the correct card, and
  Mermaid/image fullscreen controls are keyboard accessible.
- Code line numbering, ANSI SGR, unified/split diffs, and trace details render
  safely with graceful fallback on malformed input or unavailable highlighting.
- `cli/spot`, `sdk/spot`, and `server/static_assets/sdk/spot` are synchronized.
- `just build`, `just test`, `just check-generate`, and `just e2e` pass.

## Deferred Follow-Ups

- Comments, pinned feedback, and agent feedback polling through Spot DB/realtime.
- Retained deploy revisions and recent-update indicators.
- Per-card screenshot export or a fully self-contained single-HTML export.
- Local Markdown image discovery and explicit downloadable file blocks.
- Multiple named palettes beyond the initial report palette.
- Partial card/block editing commands or APIs.
