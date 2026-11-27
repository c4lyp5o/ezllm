# Neighbourhood — design

Status: **shipped** (off development 7149fd9). Concept approved
by the owner on 2026-11-27. Code: `web/src/utils/city.ts` (the data mapping),
`web/src/components/CityScene.tsx` (the drawing), `web/src/pages/Neighbourhood.tsx`.

## Why

The first city was charming and well built, but it did not tell the truth at a
glance. Measured with eight providers of mixed traffic:

- **Height flattened traffic.** Glow and height were log-scaled
  (`log10(1 + tpm) / log10(1 + ref)`): 220 tokens/min drew at 44% of the height of
  198,000, which is 900 times the traffic. Six towers looked alike.
- **Failure was the faintest signal.** Rain over a stormy building did not read in a
  still frame; a provider at 100% errors looked like the others.
- **Placement was random.** A seed chose each building's block, so a short building
  (disabled, idle, failing) often stood behind a tall one, and its label sat on
  someone else's tower.
- **Labels were street-level text** that other buildings covered.
- **"Fully lit" was relative to the busiest provider**, so one provider was always
  fully lit and the page could not say whether the gateway was busy.

The table under the city carried every answer; the city was decoration.

## Locked decisions

### One encoding per question

| question | encoding | rule |
|---|---|---|
| how much traffic | building height | **linear** in tokens/min against a ceiling, with a ruler drawn on the scene |
| did calls work | window colour | lit share = successful calls, red share = failed calls, dark = no calls |
| is it failing now | red sign, roof beacon, rain | `calls ≥ 3` and `errors / calls ≥ 25%` (unchanged rule) |
| is it switched off | short, grey, boarded | the account or every key is disabled |
| what is happening right now | cars | one car per live request from the ledger feed; a failed call is a red car |

Nothing else on the scene carries data. Sky, lamps, trees and the moon are scenery
and appear only in Show mode.

### Scale

- **Busiest** (default): the ceiling is the busiest provider's tokens/min rounded up
  to a 1, 2, 2.5 or 5 step, never below 1,000. The ruler is labelled in those steps.
- **Fixed**: the operator sets a ceiling (e.g. 100,000 tokens/min) in the page, so a
  quiet day looks quiet. Stored per browser.
- A provider with any traffic is at least one floor tall, so it never vanishes; its
  sign carries the exact number.

### Layout follows state

Rows, front to back: **failing**, **serving**, **quiet** (and usage unavailable),
**switched off**. Within a row, by name, so a building does not slide sideways when
traffic changes; height already shows who is busiest. At most four buildings per row
on desktop, two on a phone. A building moves only when its state changes.
The random seed and "New city" are gone: two operators with the same providers see
the same city.

### Signs

Every building has a sign above its roof: the name, and one number — tokens/min,
"N% failing", "quiet" or "switched off". Signs are placed front row first and
nudged up until none overlap, so a sign is never covered. A failing sign is red.

### Two modes over the same numbers

- **Status** (default): flat background, full-size signs, no camera movement. Meant
  to be left open on a monitor.
- **Show**: night sky, stars, moon, lamps, slow camera drift, more traffic; signs
  shrink unless the building is failing, hovered or selected.

Failure looks the same in both modes. Reduced motion draws one still frame in
either mode; a paused scene draws nothing new.

### Page

One summary line answers "is anything wrong?" ("Groq is failing" / "Everything is
working"), then traffic, calls and failure share. The details card appears only
when a building is selected. Scene options (mode, scale, pause, full screen) sit in
one row. On a phone the table becomes cards.

## Not doing

- **3D models or WebGL.** Kenney's CC0 3D kits would need a 3D library and a GPU
  context for a page meant to stay open on a monitor. The scene stays procedural
  Canvas 2D. Kenney's isometric sprites remain an option for street furniture.
- **Per-key buildings.** Keys stay in the details card.
- **A log scale toggle.** The fixed ceiling answers the same need ("small providers
  disappear") without misreading ratios.
