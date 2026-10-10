# docs/figures - diagrams for this repository

Figures are committed as source, not as screenshots. The `.html` file is the source of
truth and is self-contained: HTML plus inline SVG, no build step, and no external assets
beyond the web fonts. The `.png` next to it is the delivery copy used in the README and
in chat.

| What | File | When |
|---|---|---|
| Source (required) | `<topic>.html` | always - committed, self-contained, source of truth |
| Delivery copy | `<topic>.png` | when it is embedded in docs or sent to chat (2x, paper background) |
| Vector | `<topic>.svg` | only when a vector editor needs it |
| Gallery | `index.html` | once there are three or more figures |

`<topic>` is kebab-case and names the subject (`player-join-flow`, `nakama-module-map`).
No dates and no versions in the name - provenance is `git log`.

## Choose the level before the type

Context (where the system sits), then container (what deploys, over which protocol),
then component (what is inside one service), and only then code. Runtime (sequence, state
machine) and change (architecture delta) are separate axes. Context and container cover
most teams. One figure answers one question.

## Rules for a figure that goes into documentation

1. A full lead-in sentence before the figure - ending in `:` when it follows immediately,
   in `.` when a paragraph sits between.
2. A numbered caption: `**Figure 1.** Short description.` Numbered per document, not per file.
3. Alt text on every figure. A complex figure gets one extra sentence in prose; a
   decorative one gets `alt=""`.
4. `paper` background, never transparent, for a figure that goes into documentation.
5. Prefer SVG. PNG is the delivery copy. No animated GIFs.
6. Never ship a picture of text, of code or of a terminal - the text is real text.
7. Do not centre figures, do not wrap an `<img>` in a `<p>`, and stay consistent across a
   document set.
8. No secrets and no personal data in a figure. If something has to be hidden, cover it
   with a fully opaque block; blur is not redaction.

## Living with the code

A figure sits next to the document it serves and is committed with the same change. The
`.html` source is kept so that the figure can be regenerated. Do not redraw a full schema
or API surface - draw the part the decision touches. When the design changes, an
architecture delta beats redrawing the original.

## Required metadata

Every figure declares what it documents, inside the file, invisible when rendered:

```html
<!-- diagram-meta {"topic":"player-join-flow","level":"runtime","type":"sequence","question":"What happens when a client joins?","sources":["src/net/*.go","proto/*.proto"],"updated":"YYYY-MM-DD"} -->
```

`sources` is a glob over the code that actually decides the content of the figure, not
the whole repository. A figure whose sources changed after it was drawn is stale, and
fixing it belongs in the same change as the code.

## Skin

Figures use the house default editorial skin - paper `#f5f5f5`, ink `#2d3142`, muted
`#4f5d75`, accent `#eb6c36`. A repository can override it with a `.diagram-design`
marker at its root. The PNG is exported with headless Chromium locally; because the PNG
is committed, a reader needs no tooling at all.
