# Architectural Diagrams

The `.mmd` files in this directory are the canonical, version-controlled
source for every architectural diagram referenced by [`design.md`](../design.md).
The `.png` files alongside them are best-effort renderings of those
sources; they may lag the `.mmd` until somebody regenerates them.

| Source           | Renders to                              | Covers                                                   |
|------------------|-----------------------------------------|----------------------------------------------------------|
| `architecture.mmd` | `architecture-diagram.png`            | System overview, three-binary split, asymmetric flow (§3) |
| `registration.mmd` | `registration-flow.png`                 | Advertisement + heartbeat boot-up (§9.1)                  |
| `scale-up.mmd`     | `scale-up-execution-flow.png`         | CA scale-up, one Reservation per chunk, to VirtualNode Ready (§9.2) |
| `scale-down.mmd`   | `scale-down-execution-flow.png`       | CA scale-down to Reservation Released (§9.3)              |

`architecture-diagram-2.svg` is a hand-authored companion of the
architecture view used by the top-level `README.md`; it has no `.mmd`
source.

## Regenerating the PNGs

The Mermaid CLI (`mmdc`, shipped by `mermaid-cli`) renders any `.mmd`
file to PNG. From this directory:

```bash
npm install -g @mermaid-js/mermaid-cli   # one-time, if not already installed
mmdc -i architecture.mmd  -o architecture-diagram.png        --backgroundColor white
mmdc -i registration.mmd  -o registration-flow.png           --backgroundColor white
mmdc -i scale-up.mmd      -o scale-up-execution-flow.png     --backgroundColor white
mmdc -i scale-down.mmd    -o scale-down-execution-flow.png   --backgroundColor white
```

Commit both the updated `.mmd` and `.png` together so the rendering
stays in lock-step with the source.

Two rendering gotchas: mermaid treats a semicolon inside sequence-diagram
message or note text as a statement separator and fails to parse (use a
comma or period instead; an old semicolon is why `registration-flow.png`
went unrendered for months), and on hosts where Chrome refuses to start
without a sandbox, pass `-p` with a puppeteer config containing
`{"args": ["--no-sandbox"]}`.

## Viewing without re-rendering

GitHub renders Mermaid code fences natively, so the live `.mmd` source
will display correctly in any Markdown file that embeds it with a
` ```mermaid ` fence. For the canonical view, see the per-section
"Implemented in:" footers in [`../design.md`](../design.md), which point
at the Go packages each step in the flow lives in.
