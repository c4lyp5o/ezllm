// Provider presets: the Add provider list shows the current names, the old
// ones are gone, and picking a bridge preset seeds a clean provider name with
// no "(bridge)" suffix (one per server, so there is nothing to invent). Other
// presets leave the name to you. Runs against the built web/dist
// with fixtures (see mockdash.mjs).
import { start, check, finish } from "./mockdash.mjs";

const wanted = [
  "Claude Pro/Max (bridge)", "ChatGPT Plus/Pro (bridge)", "Google Antigravity (bridge)",
  "GitHub Copilot (bridge)", "Claude.ai (web)", "Google Gemini (web)",
];
const gone = ["Claude.ai (web session)", "Google Gemini (web cookies)", "GitHub Copilot (CLI)"];

const t = await start();
const errors = [];
const page = await t.page(null);
page.on("pageerror", (e) => errors.push(e.message));
const openAdd = async () => {
  await page.goto(t.base + "/providers");
  await page.getByRole("button", { name: /add provider/i }).first().click();
  await page.locator("#ap-kind").waitFor();
};
try {
  await openAdd();
  const options = (await page.locator("#ap-kind option").allTextContents()).map((s) => s.trim());
  check("every current preset is listed", wanted.every((w) => options.includes(w)), wanted.filter((w) => !options.includes(w)));
  check("the old preset names are gone", gone.every((g) => !options.includes(g)), gone.filter((g) => options.includes(g)));
  // Seeding only fires on a blank name, so each preset gets a fresh modal.
  const seeded = {};
  for (const label of wanted) {
    await openAdd();
    await page.locator("#ap-kind").selectOption({ label });
    await page.waitForTimeout(150);
    seeded[label] = await page.locator("#ap-name").inputValue();
  }
  const bridges = wanted.filter((w) => w.endsWith("(bridge)"));
  check("a bridge preset seeds a clean name", bridges.every((b) => seeded[b] === b.replace(/ \(bridge\)$/, "")), seeded);
  check("a web preset leaves the name blank", wanted.filter((w) => !bridges.includes(w)).every((w) => seeded[w] === ""), seeded);
  check("no page errors", errors.length === 0, errors);
} catch (e) {
  check("driver ran to completion", false, String(e));
} finally {
  await t.close();
}
finish();
