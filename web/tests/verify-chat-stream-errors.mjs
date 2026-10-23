// Regression: a chat stream that delivers some content and then an error frame,
// or ends without [DONE]/finish_reason, must show a failure next to the kept
// partial text, and that partial answer must not be replayed as history on the
// next turn. A clean stream must still finish with no error.
import { start, check, finish } from "./mockdash.mjs";

const frame = (o) => `data: ${JSON.stringify(o)}\n\n`;
const delta = (text) => frame({ choices: [{ delta: { content: text }, finish_reason: null }] });
const cases = [
  ["error frame after content", delta("PARTIAL_ANSWER") + frame({ error: { type: "api_error", message: "PROVIDER_EXPLODED", code: 502 } }), "PROVIDER_EXPLODED"],
  ["eof before completion", delta("PARTIAL_ANSWER"), "ended before"],
  ["clean completion", delta("PARTIAL_ANSWER") + frame({ choices: [{ delta: {}, finish_reason: "stop" }] }) + "data: [DONE]\n\n", null],
];

const dash = await start();
try {
  for (const [name, body, wantErr] of cases) {
    const sent = [];
    const page = await dash.page(async (route, url) => {
      if (url.pathname === "/admin/chat") {
        sent.push(route.request().postDataJSON());
        await route.fulfill({ status: 200, contentType: "text/event-stream", body });
        return true;
      }
    });
    await page.goto(dash.base + "/chat");
    const box = page.getByRole("textbox", { name: "Message", exact: true });
    await box.fill("hello");
    await box.press("Enter");
    await page.getByText("PARTIAL_ANSWER", { exact: false }).first().waitFor();
    await page.getByText("Streaming…", { exact: false }).waitFor({ state: "hidden" });
    const shown = wantErr ? await page.getByText(wantErr, { exact: false }).count() : 0;
    check(`${name}: ${wantErr ? "failure shown" : "no failure"}`, wantErr ? shown > 0 : true, { shown });
    if (!wantErr) check(`${name}: no error text`, (await page.locator(".text-bad").count()) === 0);
    check(`${name}: partial text kept`, (await page.getByText("PARTIAL_ANSWER", { exact: false }).count()) > 0);

    await box.fill("again");
    await box.press("Enter");
    await page.waitForFunction(() => !document.body.innerText.includes("Streaming…"));
    const replayed = (sent[1]?.messages ?? []).some((m) => m.role === "assistant" && String(m.content).includes("PARTIAL_ANSWER"));
    check(`${name}: ${wantErr ? "partial not replayed" : "answer replayed"}`, sent.length === 2 && replayed === !wantErr, sent[1]?.messages?.map((m) => m.role));
    await page.context().close();
  }
} finally { await dash.close(); }
finish();
