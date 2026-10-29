# Design

<!-- impeccable:design-schema 1 -->

The visual system of the Wormhole documentation site (`tools/docsite`).
It records the built world so later changes stay on it. The product itself
is a CLI and daemon; this file covers the site.

## World

Every page is a numbered engineering drawing sheet. The frame carries the
identity: a drafting-paper ground with a blueprint grid, a sheet border
with zone rulers, a drawing index in the left rail, drawing titles set in
condensed capitals, and a title block at the bottom right of each sheet
(project, title, sheet n of m, revision = release tag, source, issue date).
The reading column inside the frame is a calm document: a reading face at
a real measure, standard code blocks, tables and lists.

Light is the default: drafting paper under daylight at a desk. Dark is the
cyanotype negative: pale lines on Prussian blue, with a sparse star field
drifting behind the sheet.

## Tokens

Defined on `:root` in `assets/style.css`; dark values on `[data-theme="dark"]`
and under `prefers-color-scheme: dark` when no theme is set. The machine
copy lives in `.impeccable/design.json`.

| Role | Light | Dark |
| --- | --- | --- |
| `--paper` sheet ground | `oklch(96.5% 0.012 85)` | `oklch(24% 0.07 255)` |
| `--paper-2` code and cells | `oklch(93.8% 0.016 85)` | `oklch(29% 0.075 255)` |
| `--desk` page ground | `oklch(90% 0.02 80)` | `oklch(14% 0.035 262)` |
| `--ink` text | `oklch(26% 0.04 255)` | `oklch(94% 0.02 240)` |
| `--ink-2` secondary text | `oklch(43% 0.04 255)` | `oklch(80% 0.045 238)` |
| `--ink-3` labels, numbers | `oklch(47% 0.045 255)` | `oklch(70% 0.05 238)` |
| `--line` rules and borders | `oklch(52% 0.09 250 / .55)` | `oklch(82% 0.07 230 / .5)` |
| `--grid-minor` / `--grid-major` | blue at 8.5% / 19% | pale blue at 9% / 20% |
| `--blue` links, drawn strokes | `oklch(45% 0.16 255)` | `oklch(82% 0.11 220)` |
| `--red` the thread in motion | `oklch(56% 0.19 35)` | `oklch(74% 0.17 40)` |
| `--red-text` red used as text | `oklch(45% 0.18 35)` | `oklch(80% 0.15 45)` |
| `--star` | unused | `oklch(95% 0.02 230)` |

Color strategy: restrained. Neutrals are tinted from the world (warm paper,
navy ink); blue does the drawing and the linking; red is reserved for the
one thing in motion (the envelope, the active marker, the current sheet
number, focus rings). Secondary text on colored ground is tinted from the
hue, never gray. Body and label text meet 4.5:1 in both themes.

Grid: minor 24px, major 120px, drawn with layered linear gradients on the
sheet. Zone rulers are 8px strips with a tick every 120px.

## Type

- Display and labels: Barlow Condensed 600 (700 for the wordmark), uppercase,
  +0.02em on the sheet title, +0.05 to +0.14em on smaller labels.
- Reading: Barlow 400/500/600 at 17px, line-height 1.55, measure 70ch,
  `text-wrap: pretty` on paragraphs and `balance` on headings.
- Code: JetBrains Mono 400/600 at 0.86em inline and 0.9rem in blocks.
  Monospace is for code, subjects, and the title block's numbers only.

Scale: sheet title `clamp(2.6rem, 1.5rem + 3.6vw, 4.6rem)`; h2 1.6rem with a
rule beneath; h3 1.2rem; labels 0.74 to 0.95rem.

Fonts are self-hosted from `assets/fonts/` (latin and latin-ext subsets,
OFL licenses alongside). No hosted font stylesheet.

## Spacing and layout

4px base. Sheet grid: rail 232px, column `minmax(0, 1fr)` with prose at
70ch, contents 220px, gutter `clamp(16px, 4vw, 48px)`. The home sheet lets
the column span the sheet so the schematic has room; prose stays at
measure. Sections: 3rem above an h2, 1rem below. Contents and rail are
sticky under the 56px masthead.

Breakpoints: under 1180px the contents column drops; under 840px the rail
becomes a horizontal strip of sheet tabs, the sheet loses its side border,
and the title block stretches; under 480px the wordmark keeps only its
mark and title-block rows stack.

## Components

- Masthead: wordmark (drawn mark plus WORMHOLE), search field, release
  tag chip (monospace), GitHub link, theme switch.
- Rail: "Drawing index", ordered list with two-digit sheet numbers; the
  current sheet's number is red.
- Sheet title, lede (home only), primary action as a drawn button (ink
  fill, condensed caps, arrow), secondary action as an underlined link.
- Schematic (home only): inline SVG, three stations, ask lane above, reply
  lane below, leader lines to callouts, an envelope on the thread. Strokes
  carry `pathLength="1"` so one dasharray serves the draw-on.
- Prose: h2 as drawing title with rule; inline code in a bordered chip;
  code blocks with a copy control that appears on hover; tables ruled with
  condensed-caps headers inside a horizontal scroll wrapper; blockquotes
  with a 1px rule.
- Pager: previous and next sheet with small condensed labels.
- Title block: a ruled table, right-aligned, 540px max.
- Contents ("On this sheet"): left-ruled list; the active heading carries
  a red rule.
- Search results: a panel under the field with sheet number, title,
  matched heading and a snippet with `<mark>`.
- Icons: authored single-stroke SVG at 1.6px stroke (search, sun, moon,
  copy, the wordmark). No glyph or emoji icons.

Browser surfaces are themed: selection (blue on paper), caret (red), focus
ring (2px red, offset 3px), thin scrollbars in the line color, underline
offset 0.18em, tabular numerals in tables and the title block.

## Motion

Exactly three movements:

1. The authored moment: the schematic draws itself on arrival (1.1s,
   `cubic-bezier(0.16, 1, 0.3, 1)`, staggered: stations, lanes, leaders,
   then labels fade). Once.
2. The scroll-bound element: the envelope travels the ask lane and returns
   on the reply lane over the first 1400px of scroll (`animation-timeline:
   scroll(root)`); without scroll timelines it loops over 18s.
3. The ambient layer: three star layers drift upward over 120 to 240s, dark
   theme only.

Everything else is 150ms state feedback with the same ease-out. Under
`prefers-reduced-motion` the drawing renders complete, the envelope rests
at the bus, and the stars hold still.

## Voice

Labels use the drawing's own vocabulary: sheet, drawing index, title
block, rev, issued, on this sheet. Controls name their action ("Copy to
clipboard", "Switch to dark theme"). Placeholder people are alice and bob.
