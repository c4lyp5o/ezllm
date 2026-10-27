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

### The exceptions, and why they exist

Two animations are deliberately NOT transform-and-opacity, and both earn it:

- **Height animates** (`Collapse`) when a panel opens or closes — a provider
  account's details, a service's mode-dependent block, a combo's strategy
  fields. A fade alone would leave the page snapping to a new shape under the
  operator's cursor, which reads as a jump. Height is the one property that
  makes the change legible, and the block is short-lived and user-initiated,
  so the reflow cost is not on a repaint path.
- **Layout animates** (`ReorderItem`) when a combo's hops or a profile's
  compression stages are reordered. Order in both is meaning — hop order is
  routing priority, stage order is the execution pipeline — so seeing the row
  travel to its new position is the whole point. This is the one animation in
  the app that carries information rather than decoration.

### Rules the motion layer enforces

- **A `StaggerItem` needs a `Stagger` parent.** Variants resolve by walking
  up to the nearest orchestrator; an item with no parent resolves no variant
  and sits at its final state from the first paint — it looks fine and animates
  nothing.
- **Never put `initial={false}` on a page-level `AnimatePresence`.** It
  suppresses the initial animation for the entire descendant subtree, not just
  the direct child.
- **Table rows fade; they do not translate.** A transform on a `<tr>` does not
  participate in the table's own row sizing, so the row slides while its cells
  stay put and it detaches from its own columns (`staggerRow`).
- **`StaggerItem` carries `min-w-0`.** It is a grid or flex child, and those
  default to `min-width:auto`; without it the wrapper's min-content width —
  set by the longest unbreakable token inside — becomes the track's minimum and
  widens the page past the viewport.
- **A panel that is a toggle animates its HEIGHT, not its opacity**, and the
  `AnimatePresence` must directly own the animated child or the exit is dead.

## Layout

- A labelled sidebar (ten destinations need words); the screen's h1 shows at
  every width.
- Panels: 6px radius, one 1px line, no shadow at rest (dialogs and menus float).
- Cards that share a row end level; an empty block keeps its frame and says so
  in one sentence (dashed box). Loading shows the final layout with flat grey
  blocks at the values' sizes.

## The scene (Neighbourhood)

The Neighbourhood page is the one place the no-gradients, no-glows rule is
suspended: it is a toy, and a glow is the whole point. The toy is an isometric
city: one building per provider account, streets between them, and traffic that
runs whether or not anyone is watching. The exception is fenced and carries
rules of its own:

- **Scoped.** Every scene colour is a `--nb-*` token under `.nbhd` in
  `src/index.css` (light = a day scene, dark = a night one). No other data-page
  component may use a gradient or glow, and no data screen embeds the scene. The
  pre-auth login backdrop is described separately below.
- **Generated, but stable.** The street grid comes from `planCity` in
  `src/utils/city.ts`: blocks, setbacks, footprints, roof plant, trees and parks
  are derived from a seed plus a hash of the provider id. A building therefore
  never moves when traffic changes — only its height, light and weather do.
  "New city" re-rolls the seed and regrows the same providers in a new
  arrangement; nothing else about the data changes.
- **Activity indicators are data.** Height and window light are tokens per
  minute over the last five minutes (log scale, full glow = the busiest
  provider, never less than 2,000 a minute). Rain = a quarter or more of recent
  calls failed. Boarded windows = the account or all of its keys are disabled.
  Street traffic thins and thickens with the live request rate of the last
  minute. Roof plant, antennas, trees, parks, lamps, people, umbrellas, the
  plane overhead and the weather itself are scenery, not additional metrics.
  Missing usage is unavailable, never invented zero activity.
- **Weather is occasional and controllable.** A shower drifts in on the city's
  seeded schedule every minute or two and clears again — it is scenery, so it
  may be commanded with the Auto sky / Rain / Clear control and never encodes a
  figure. What it changes is atmosphere only: streaks over the whole scene, a
  sheen on the streets, thicker clouds, umbrellas on the pavements.
- **A request is an event.** A committed ledger row on `/admin/stream` flares
  that provider's building — the light climbs the facade from the pavement — and
  sends a car to its door. This is the one deliberate reversal of the old rule
  that motion must never replay a request: the feed is a real per-request
  source, so every flare is a call that actually happened, never a scheduled
  imitation timed to look alive. Quiet gateways still breathe on the ambient
  traffic; a call is an exception you can see.
- **Never colour alone.** The table under the scene lists every provider with
  exact, unabbreviated figures and a trend arrow for what moved since the last
  update (also named in words in the inspector), and each building is a real
  `<button>` with an `aria-label` and a `<title>`. Buildings and table buttons
  support focus, Enter/Space and touch selection; the inspector exposes the
  selected provider's exact values, its key count and its per-key split outside
  the illustration.
- **Cheap, and bounded.** Canvas 2D — never WebGL. One affine matrix per
  building face draws the window grid as `fillRect`, and one depth-sorted pass
  gives correct occlusion for traffic. Device pixel ratio is capped at 2, small
  screens budget 30 fps, the loop stops when the tab is hidden, pause stops the
  loop without stopping usage refresh (a paused scene still repaints on a data
  change), and reduced motion renders one still frame with the same figures on
  it. Usage is polled every 10 s; the SSE feed costs nothing.
- **Contained.** The stage sizes itself to its container (`clamp`), fits the
  whole city by construction (bounds include roof antennas), and never widens
  the page. Loading, empty, failed and partial reads stay actionable;
  previously available data is retained on a refresh failure. Landscape and
  portrait layouts show the same real provider IDs.

## Login railway

The sign-in/setup backdrop is a separate canvas exception to the flat dashboard.
It restores the pre-1.3 glowing sky/violet rails and busier desktop traffic, while
retaining bounded phone resolution and a 30 fps phone budget. Paint the initial
scene before frame limiting, initialize the elapsed-time clock independently of
whether a frame is drawn, and clamp resume deltas. Packets interpolate each rail
segment within its own 0–1 interval. Pause the loop while hidden and render a
still picture for OS/browser reduced motion. It requests no private dashboard
API data before authentication.

Browser regressions exercise the built assets in `tests/verify-railway.mjs`,
`tests/verify-neighbourhood.mjs`, and `tests/verify-neighbourhood-polish.mjs`;
`tests/shot-city.mjs` screenshots the city and measures its frame rate on a
21-provider fixture.
