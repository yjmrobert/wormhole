# Design

<!-- impeccable:design-schema 1 -->

The visual system of the Wormhole documentation site (`tools/docsite`).
It records the built world so later changes stay on it. The product itself
is a CLI and daemon; this file covers the site.

## World

A 1984 night, seen through glass. Behind every page a wormhole runs in
WebGL (`assets/wormhole.js`): the camera falls down a bending tunnel whose
walls are violet smoke threaded with cyan and magenta filaments, rings
rush out past the viewer, warp streaks pass at the edges, and the throat
is a lensed window onto the far side, ringed by a thin cyan event horizon.
The synthwave night is in the shader too: a striped sun (violet to magenta
to gold, the stripes widening toward its foot) sits on the horizon at the
far end of the tunnel behind the smoke; below the horizon the floor is the
wireframe grid, magenta lines running toward the reader and cyan lines
across, scrolling toward the camera and sagging into the well under the
throat; faint CRT scanlines sit over the dark theme. The page itself is a
set of frosted panes floating over it:
the masthead strip, the index rail, the reading column, the contents
card. The wormhole shows through them softened and darkened; between
them, and in the home page's hero window, it shows clear. The wordmark is
a neon sign over the throat; two terminal cards float in the window and
show the ask and the inbox. Inside the column everything is a calm
document: cyan links, magenta for the one thing in motion, code blocks
and tables as slightly denser glass.

Dark is the native state. Light is the same glass with the lights on
(near-white panes, navy ink, cyan and magenta darkened for contrast), and
the wormhole behind it is the same scene printed in ink on paper: every
feature that adds light at night subtracts its complement on paper, so
cyan, magenta and violet stay saturated, white stars and the hot core fall
away, and the smoke prints at under half weight so the ground stays paper.

## Tokens

Defined on `:root` in `assets/style.css`; light values on
`[data-theme="light"]` and under `prefers-color-scheme: light` when no
theme is set. The machine copy lives in `.impeccable/design.json`.

The glass (alpha is what makes it glass; the hue stays navy or paper):

| Role | Dark | Light |
| --- | --- | --- |
| `--glass` panes | `oklch(13% 0.05 287 / .66)` | `oklch(98.5% 0.006 300 / .74)` |
| `--glass-2` code, tables, cards | `oklch(15% 0.05 288 / .74)` | `oklch(99.5% 0.004 300 / .82)` |
| `--glass-3` chips, hover, inputs | `oklch(22% 0.06 290 / .6)` | `oklch(94% 0.02 300 / .7)` |
| `--glass-edge` hairline | `oklch(96% 0.01 300 / .13)` | `oklch(18% 0.05 290 / .14)` |
| `--glass-hi` inner top highlight | `oklch(96% 0.01 300 / .09)` | `oklch(100% 0 0 / .9)` |
| `--blur` backdrop blur | 18px (14px under 840px) | same |

Every pane is `.pane`: that background, the hairline, the inner highlight,
a 135° sheen, `backdrop-filter: blur(var(--blur)) saturate(1.25)`. The
panes go nearly opaque without `backdrop-filter` support, under
`prefers-reduced-transparency`, and under `prefers-contrast: more`.

The wormhole's own palette is fixed in the shader (navy, violet, magenta,
cyan, white) and matches these tokens by eye; `u_light` crossfades it to
the ink-on-paper rendering when the theme switches.

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

- The wormhole: a fixed `<canvas class="wormhole">` under everything,
  drawn by `assets/wormhole.js` with one fragment shader and no library.
  The throat sits in the hero window on the home page and follows it as
  the page scrolls; on reading pages it keeps to the right, behind the
  contents card, away from the column. The pointer nudges it by a few
  percent. It renders at 0.6 to 0.7 of device pixels (the frost hides the
  difference) and steps down further if frames run long.
- Masthead: a glass strip with a magenta-to-cyan hairline along its foot,
  wordmark (cyan and magenta mark plus WORMHOLE in Chakra Petch), pill
  search, release tag chip, GitHub link, wormhole switch (an orbit icon,
  `aria-pressed`, cyan ring while on), theme switch. Under 480px the chip
  hides so the search keeps its width.
- Hero (home only): a clear window onto the throat, the neon title at 22%
  from the top, two glass terminal cards, and a glass kicker pill ("Ask
  across machines") with a cyan thread hanging from it. `hero()` in
  `main.go` emits only the cards.
- Rail: a glass card, "Index", numbered pages, the current one on a cyan panel.
- Lede, primary action as a cyan pill, secondary as a magenta-underlined link.
- Figure 1 (home only): the round-trip schematic in two layouts; cyan ask
  lanes, magenta reply lanes, violet bus, pink envelope with a glow.
- Prose: h2 with the gradient rule; inline code as cyan chips; code blocks
  as terminal windows with three magenta dots and a copy control on hover;
  tables with a navy header in cyan caps; blockquotes with a magenta rule.
- Pager: previous and next with magenta labels.
- Title block: a readout panel, right-aligned, 560px max; numbers in cyan mono.
- Contents ("On this page"): left-ruled list; the active heading in cyan.
- Icons: authored single-stroke SVG at 1.6px stroke. No glyph or emoji icons.

Browser surfaces are themed: selection (magenta on navy), caret (magenta),
focus ring (2px cyan, offset 3px), thin scrollbars, underline offset 0.2em,
tabular numerals in tables and the title block.

## Motion

Three movements, all ambient, all stoppable:

1. The wormhole. The camera rolls slowly and falls down the tunnel; the
   walls scroll, rings rush out, streaks pass, the grid floor runs toward
   the reader, the throat pulses in front of the sun, the stars twinkle.
   It has a calm level: 0 while the home hero window is in view, 1 on
   reading pages and once the hero has scrolled away. Calm halves the
   speed of the fall, thins the smoke, filaments and rings, removes the
   dust, all but removes the streaks, deepens the vignette and takes a
   fifth of the saturation out, so the column reads over a dark drift and
   the full show lives in the hero. It is the one thing computed at
   runtime, and it pays for itself: below device resolution, paused when
   the tab is hidden, stepping down resolution and noise octaves when a
   frame averages over 24 ms. The reader can turn it off: the orbit button
   in the masthead sets `data-wormhole="off"` on the root, remembered in
   `localStorage` like the theme and applied before first paint; the
   canvas hides, the star field returns, the panes stay glass over navy.
2. Figure 1 plays the round trip as one looped 16 s story (`--loop`),
   unchanged from the first design: envelope, bus pulse, cards, receipt,
   a four-step caption strip with ticks.
3. Without WebGL, three star layers drift upward over 140 to 260 s, dark
   theme only, under the same panes on plain navy.

Everything else is 150 ms state feedback with `cubic-bezier(0.16, 1, 0.3, 1)`.
Under `prefers-reduced-motion` the wormhole draws one still frame (and
redraws it on resize, scroll and theme change), the stars hold, and the
figure shows its finished state.

## Voice

Labels are plain: index, on this page, previous, next, page, rev, issued.
Controls name their action ("Copy to clipboard", "Switch to light theme").
Placeholder people are alice and bob.
