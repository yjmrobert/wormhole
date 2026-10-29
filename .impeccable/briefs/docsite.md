# Surface brief: documentation site (tools/docsite → GitHub Pages)

Scope: the whole public documentation site, every page rendered from docs/.
Visitor mode: Read. Code-led build (Read surface, no image generation).

Audience and job: engineers evaluating or operating Wormhole; they arrive
from the README or a release and need either the picture (how it works,
is it safe) or one answer (a flag, a key, a behavior).

Proof and content: the real docs under docs/, the real release tag, the
real CLI table. Nothing invented: no adopters, no benchmarks, no demo.

Constraints: static, subpath-hosted, no third-party requests, self-hosted
fonts, WCAG AA both themes, reduced-motion path, Go-only toolchain.

Unresolved: none blocking. Search is client-side over a generated index.

## Direction contract

THESIS: every page is a numbered engineering drawing of Wormhole, read
on drafting paper; the frame (sheet border, title block, drawing index,
section openers set as drawing titles) carries the world and the reading
column stays a calm document. It refuses the docs-site default of a white
page with a purple sidebar and a search box in a pill.

OWN-WORLD: light theme is warm drafting paper (oklch ~96% chroma .012 hue
80) with a faint blueprint-blue minor/major grid, navy ink text (oklch 27%
.04 255), blueprint blue (oklch 52% .16 255) for links and the drawn
schematic, red-lead (oklch 62% .19 35) for the one thing in motion, the
envelope on the thread. Dark theme is the cyanotype negative: Prussian
blue ground (oklch 23% .07 255) fading to near-black space at the edges,
pale cyan-white lines and text, a sparse star field drifting behind the
sheet. Type: Barlow Condensed 600 uppercase with +0.04em tracking for
drawing titles and the title block, Barlow 400/500 for reading, JetBrains
Mono for code only. Icons are authored single-stroke SVG. Sheet border
with zone ticks on desktop; title block bottom-right of every sheet
(title, sheet n of m, rev = release tag, project).

STORY: the visitor lands on sheet 01 and sees the round trip drawn in
front of them, understands in one viewport that a question leaves one
machine and an answer comes back through a bus, reads that nothing runs
on the other side until a person opens it, and goes to Install.

FIRST VIEWPORT (home, 1440 wide): masthead strip across the top (wordmark
left: a drawn wormhole mark plus WORMHOLE in condensed caps; search in the
middle; release tag, GitHub link and theme switch right). Below it the
sheet: left rail “DRAWING INDEX” listing the sheets; the column opens with
the drawing title WORMHOLE, one-line thesis beneath it in reading type,
then figure 1 across the column: three stations left to right (Alice’s
machine, the bus as a cylinder, Bob’s machine) with session cards inside
the screens, the ask lane above and the reply lane below carrying the
subjects (“wh.inbox.bob.alice”, “wh.inbox.alice.bob”) and the words ask
and reply, a dashed receipt return between the lanes, the red-lead
envelope on the thread, and a four-step caption strip under a rule with
step ticks at its right. The primary action, “Install”, sits as a drawn
button under the thesis line; “Read the design” beside it as a link.

FORM: Drawing sheet, first on the ordered list; seed: none (unavailable);
the roll could not run in this environment and the world was pinned by the
maintainer.

Signature interaction (revised at the maintainer's request after the
first review): figure 1 is a looped 16 s story of the round trip, with
session cards appearing on each machine, a bus pulse, a receipt, and a
four-step caption strip that advances with the phases. Reduced motion
shows the finished state. The draw-on and the scroll-bound envelope are
retired in its favour.

Motion grammar: one authored loop (figure 1), one ambient layer (star
drift, dark theme only). Everything else is 150–300 ms state feedback
with ease-out.

Cross-surface reach: the sheet frame, title block, index rail, drawing
titles and code styling repeat on every page; the schematic is unique to
sheet 01, smaller drawings may open other sheets later.

Recorded adaptations: link and label text uses darker blue (45%) and red
(45%) than the contract's stroke values so body-size text meets WCAG AA on
paper; the drawn strokes (stations, lanes, envelope) keep the contract's
52% blue and 62% red via --blue-stroke and --red-stroke. The sheet sits on
a darker desk so the paper reads as a sheet.

Honest risk: the heaviest of the three directions; scroll-driven motion
needs the fallback; the title block must not become a kicker.

FINISH: unreviewed and undocumented is unfinished; this build ends with
the finish review, the verdict, DESIGN.md, and every shipping raster
carrying its provenance.
