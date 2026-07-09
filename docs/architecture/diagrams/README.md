# Architecture diagrams

Two renderings of the same architecture, for two different jobs.

| Artifact | Job | Renders on GitHub | Icons |
|---|---|---|---|
| [`aws-architecture.svg`](aws-architecture.svg) | Inline documentation | ✅ light **and** dark | Custom-drawn glyphs, approximate AWS palette |
| [`aws_architecture.py`](aws_architecture.py) → `aws-architecture.png` | Slides, exports, presentation | as a committed PNG | **Official AWS icon set** |

The SVG's glyphs are hand-drawn `<symbol>` definitions that *evoke* the AWS icon set — a chip for EC2, a bucket for S3, a λ for Lambda — rather than reproducing it. AWS's actual icons are a licensed asset pack, and embedding them would bloat the file and bind it to their terms. The PNG path exists precisely for when you need the real ones.

Both depict the architecture specified in [`docs/architecture/`](../). **Where a diagram and those documents disagree, the documents win.** A diagram is a view, not a source of truth.

## ⚠ These two must be kept in sync

This is the standing cost of having both. A change to the architecture means editing the SVG *and* the Python source, then regenerating the PNG. There is no mechanism enforcing this — nothing fails if they drift, they simply start lying at different rates.

If that cost stops being worth paying, **delete the PNG path and keep the SVG.** It is the one that renders inline where people actually read the docs.

## Regenerating the PNG

Requires Graphviz (the `dot` binary) plus the `diagrams` package.

```bash
brew install graphviz

python3 -m venv .venv
.venv/bin/pip install diagrams
.venv/bin/python docs/architecture/diagrams/aws_architecture.py
```

Writes `aws-architecture.png` into this directory. The Graphviz intermediate file it emits alongside is gitignored.

`diagrams` renders through Graphviz, so the layout is solver-chosen rather than hand-placed: expect it to differ from the SVG in arrangement while agreeing on content. That is the trade for getting the official icons without hand-placing 30 nodes.

## Editing the SVG

Hand-authored, no build step, no dependencies. Geometry is explicit, so it is easy to break by moving something into a neighbour.

Two checks worth running after any edit — both of these caught real defects during authoring:

```bash
# 1. XML well-formedness. (SVG comments cannot contain `--`.)
python3 -c "import xml.dom.minidom as m; m.parse('docs/architecture/diagrams/aws-architecture.svg')"

# 2. Actually look at it. macOS QuickLook rasterises SVG:
qlmanage -t -s 1440 -o /tmp docs/architecture/diagrams/aws-architecture.svg
open /tmp/aws-architecture.svg.png
```

The second one matters. Arrows routed by hand will happily pass straight through a component's card, and nothing but your eyes will tell you. QuickLook crops wide diagrams to a square — to inspect the right-hand edge, temporarily narrow the `viewBox`.

The SVG carries a `prefers-color-scheme: dark` block, so it inverts for dark-mode readers. Verify both themes, not just the one your machine is in.

## What the diagram is trying to say

If a reader takes three things from it:

1. **Spot is on the interruptible components, On-Demand is on the stateful ones.** The `SPOT` badges sit on n8n workers and Ollama; the OpenClaw Gateway is On-Demand precisely because it cannot be.
2. **The Gateway has zero inbound ingress.** Chat channels are outbound-initiated — the dashed arrow points *out*. Its remaining attack surface is semantic, not network ([08 — Security](../08-security.md)).
3. **Nothing reaches a model provider directly.** Bedrock and Ollama both sit behind the Model Gateway seam, which is what lets Bedrock act as Ollama's availability backstop ([ADR-0003](../../adr/0003-model-gateway-seam.md)).

Everything else on the canvas is supporting detail.
