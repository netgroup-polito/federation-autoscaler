# Architectural Diagrams

The `.mmd` files in this directory are the canonical, version-controlled
source for every architectural diagram referenced by [`design.md`](../design.md).
The `.png` files alongside them are best-effort renderings of those
sources; they may lag the `.mmd` until somebody regenerates them.

| Source           | Renders to                              | Covers                                                   |
|------------------|-----------------------------------------|----------------------------------------------------------|
| `architecture.mmd` | `architecture-diagram.png`            | System overview, three-binary split, asymmetric flow (§3) |
| `registration.mmd` | `registration-flow.png`               | Advertisement + heartbeat boot-up (§8.1)                  |
| `scale-up.mmd`     | `scale-up-execution-flow.png`         | CA scale-up → Reservation Peered → VirtualNode Ready (§8.2) |
| `scale-down.mmd`   | `scale-down-execution-flow.png`       | CA scale-down → Reservation Released (§8.3)               |

The `liqoctl` exec hops added in steps 8 / 9 (provider's `liqoctl generate
peering-user`, consumer's `liqoctl peer` / `liqoctl unpeer`) appear as
explicit participants in the sequence diagrams; the v3.0 PNGs predate
them.

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

## Viewing without re-rendering

GitHub renders Mermaid code fences natively, so the live `.mmd` source
will display correctly in any Markdown file that embeds it with a
` ```mermaid ` fence. For the canonical view, see the per-section
"Implemented in:" footers in [`../design.md`](../design.md), which point
at the Go packages each step in the flow lives in.
