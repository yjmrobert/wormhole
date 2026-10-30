# Design

<!-- impeccable:design-schema 1 -->

The visual system of the Wormhole documentation site (`tools/docsite`).
It records the built world so later changes stay on it. The product itself
is a CLI and daemon; this file covers the site.

## World

A 1984 night. The home page opens on a neon horizon: a wireframe floor in
perspective, magenta lines running toward the reader and cyan lines across,
bent into a well under the wormhole, which stands on the horizon as a
striped magenta disc inside a pulsing cyan ring. The wordmark is a neon
sign above it. Two terminal cards on the floor show the ask and the inbox.
Everything below the horizon is a calm document: deep navy, cyan links,
magenta for the one thing in motion, terminal-style code blocks.

Dark is the native state. Light is the same site with the lights on
(lavender-white paper, navy ink, cyan and magenta darkened for contrast),
and the hero stays a night window inside it.

## Tokens

Defined on `:root` in `assets/style.css`; light values on
`[data-theme="light"]` and under `prefers-color-scheme: light` when no
theme is set. The machine copy lives in `.impeccable/design.json`.

| Role | Dark | Light |
| --- | --- | --- |
| `--navy` page ground | `oklch(11% 0.045 285)` | `oklch(97% 0.012 300)` |
| `--navy-2` panels, code | `oklch(15% 0.05 288)` | `oklch(99% 0.005 300)` |
| `--navy-3` cells, chips | `oklch(19.5% 0.055 290)` | `oklch(93.5% 0.02 300)` |
| `--ink` text | `oklch(96% 0.01 300)` | `oklch(18% 0.05 290)` |
| `--ink-2` secondary text | `oklch(78% 0.03 300)` | `oklch(40% 0.05 290)` |
| `--ink-3` labels | `oklch(64% 0.04 300)` | `oklch(50% 0.05 290)` |
| `--line` rules | `oklch(96% 0.01 300 / .14)` | `oklch(18% 0.05 290 / .16)` |
| `--cyan` light, strokes | `oklch(86% 0.14 200)` | same |
| `--cyan-text` links, current page | `oklch(86% 0.14 200)` | `oklch(46% 0.12 210)` |
| `--magenta` the thing in motion | `oklch(68% 0.27 340)` | same |
| `--magenta-text` numbers, markers | `oklch(77% 0.22 345)` | `oklch(50% 0.24 340)` |
| `--violet` the bus, glows | `oklch(56% 0.25 300)` | same |
| `--pink` the envelope | `oklch(78% 0.2 350)` | same |

Color strategy: two lights on one night. Cyan is for reading and finding
(links, the current page, inline code, focus). Magenta is for the one thing
in motion (the envelope, numbers, the reply lane, the caret). Violet only
glows. Body and label text meet 4.5:1 in both themes; the hero keeps its
own fixed night colours in both.

## Type

- Wordmark and home title: Monoton, the neon sign, uppercase, +0.06em,
  cyan with a cyan glow. Used nowhere else.
- Titles and labels: Chakra Petch 700/600, uppercase, +0.04 to +0.2em.
- Reading: Exo 2 400/500/600 at 17px, line-height 1.6, measure 68ch,
  `text-wrap: pretty` on paragraphs and `balance` on headings.
- Code: JetBrains Mono 400/600 at 0.86em inline and 0.88rem in blocks.

Scale: home title `clamp(2.4rem, 1rem + 5.4vw, 5rem)`; page title
`clamp(2rem, 1.1rem + 2.4vw, 3.1rem)`; h2 1.35rem caps with a magenta to
cyan rule beneath; h3 1.18rem in cyan; labels 0.66 to 0.82rem.

Fonts are self-hosted from `assets/fonts/` (latin and latin-ext subsets,
OFL licences alongside). No hosted font stylesheet.

## Spacing and layout

4px base. Page grid: rail 232px, column `minmax(0, 1fr)` with prose at
68ch, contents 216px, gutter `clamp(16px, 4vw, 56px)`, max 1360px. The
hero spans the grid and bleeds to the viewport edges at 640px tall (480px
under 840px). Sections: 3.2rem above an h2, 1rem below. Contents and rail
are sticky under the 60px masthead.

Breakpoints: under 1180px the contents column drops; under 840px the rail
becomes a row of pills, the terminal cards hide, the hero shrinks; under
480px the wordmark keeps only its mark and title-block rows stack.

## Components

- Masthead: frosted navy, wordmark (cyan and magenta mark plus WORMHOLE in
  Chakra Petch), pill search, release tag chip, GitHub link, theme switch.
- Hero (home only): sky gradient, sun, ring, mesh, haze, floor fade, two
  terminal cards, the title. The mesh is emitted by `hero()` in `main.go`.
- Rail: "Index", numbered pages, the current one on a cyan panel.
- Lede, primary action as a cyan pill, secondary as a magenta-underlined link.
- Figure 1 (home only): the round-trip schematic in two layouts; cyan ask
  lanes, magenta reply lanes, violet bus, pink envelope with a glow.
- Prose: h2 with the gradient rule; inline code as cyan chips; code blocks
  as terminal windows with three magenta dots and a copy control on hover;
  tables with a navy header in cyan caps; blockquotes with a magenta rule.
- Pager: previous and next with magenta labels.
- Title block: a readout panel, right-aligned, 560px max; numbers in cyan mono.
- Contents ("On this page"): left-ruled list; the active heading in cyan.
- Reading pages: a faint static grid floor fixed at the foot of the viewport.
- Icons: authored single-stroke SVG at 1.6px stroke. No glyph or emoji icons.

Browser surfaces are themed: selection (magenta on navy), caret (magenta),
focus ring (2px cyan, offset 3px), thin scrollbars, underline offset 0.2em,
tabular numerals in tables and the title block.

## Motion

Three movements, all ambient, all stoppable:

1. The hero ring breathes over 7 s and the orb spirals into the well on an
   11 s motion path. The mesh never changes.
2. Figure 1 plays the round trip as one looped 16 s story (`--loop`),
   unchanged from the first design: envelope, bus pulse, cards, receipt,
   a four-step caption strip with ticks.
3. Three star layers drift upward over 140 to 260 s, dark theme only.

Everything else is 150 ms state feedback with `cubic-bezier(0.16, 1, 0.3, 1)`.
Under `prefers-reduced-motion` the orb is hidden, the ring and stars hold,
and the figure shows its finished state. Nothing is computed at runtime.

## Voice

Labels are plain: index, on this page, previous, next, page, rev, issued.
Controls name their action ("Copy to clipboard", "Switch to light theme").
Placeholder people are alice and bob.
