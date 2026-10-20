# Dashboard house rules

The operator dashboard follows one style: a dark data terminal, read in long
sessions, where colour belongs to data and every number can be trusted to the
unit. Tokens live in `src/index.css`; shared parts in `src/components/`.

## Colour

- Dark is the default theme; light is the opt-out. Both are token sets with
  the same names (`@theme` = light, `.dark` = dark). Never a raw hex in a
  component.
- Ground `#171719`, panels a step darker `#131217`, lines barely above the
  ground (`#232326`). Text in three tiers that all clear 4.5:1.
- One accent: ezllm's own sky (`#38bdf8` dark, `#0369a1` light). It goes on the
  main data series (tokens in) and on the selection line — nowhere else.
- Status colours (`ok`, `warn`, `bad`) only where they mean something: a
  failure count above zero, a status pill. Pills are pale pairs (tint +
  same-hue text), never saturated fills.
- No gradients, glows, glass or backdrop blur. No violet or indigo.

## Type

- Public Sans (bundled, works offline), 13px body, tabular figures everywhere.
- `font-mono` is the figure face (Public Sans, tabular). Real code, keys and
  IDs (`<code>`, `<pre>`, `<kbd>`, `.font-code`) keep a true monospace.
- Nothing under 11px. Labels and headers in sentence case — no tracked capitals.
- Screen title 24px, the screen's lead figure 30px, other figures 22px.

## Numbers

- **Never abbreviated.** `186,652`, not `186.8k`. `formatTokens` prints the
  exact integer with separators.
- **Money: two decimals**, separators, no space after the sign (`$1,204.50`,
  `$0.03`) — `fmtMoney`.
- **Compression figures are whole numbers**: tokens removed exactly, shares as
  whole percents (`34%`) — `pctWhole`.
- A missing value is a grey dash; a real zero prints as `0`.
- Figures that appear twice come from one value; shares name their base
  ("of prompt tokens").

## Selection

- Every selection — nav item, tab, segmented option — is the same mark: a 1px
  accent line exactly as wide as its label (`SelectionLine`). No filled pills.
- Toggles turn the semantic green when on, never the accent.
- One filled primary button per screen: the ink colour inverted.

## Motion (motion.dev)

The app uses `motion` (`motion/react`), pinned to an exact version at least two
weeks old. Motion is part of the product's feel, and it stays quick:

- Sections rise 12px and fade in, staggered ~60ms, once per visit
  (`Stagger`/`StaggerItem`, `Reveal`). A 15-second poll never replays them.
- A screen arrives with an 8px rise on navigation.
- The selection line slides between choices (shared `layoutId`) instead of
  blinking.
- Live rows enter from the top (`SlideRow`); large changing totals tween
  (`CountUp`); bars and charts draw in.
- Press feedback on every button.
- Transform and opacity only, 300–450ms, expo-out. `MotionConfig
  reducedMotion="user"` turns all of it into instant state changes for anyone
  who asks the OS for less motion.

## Layout

- A labelled sidebar (ten destinations need words); the screen's h1 shows at
  every width.
- Panels: 6px radius, one 1px line, no shadow at rest (dialogs and menus float).
- Cards that share a row end level; an empty block keeps its frame and says so
  in one sentence (dashed box). Loading shows the final layout with flat grey
  blocks at the values' sizes.
