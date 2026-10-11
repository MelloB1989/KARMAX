import { test, expect, type Page } from "@playwright/test";
import fs from "node:fs";

// where screenshots go: E2E_SHOTS, else e2e/screens (ignored by git)
const SHOTS = process.env.E2E_SHOTS ?? "e2e/screens";
const BG = [0x0a, 0x10, 0x18];

const url = (p = "") => `${process.env.E2E_URL}${p}`;
const shot = async (page: Page, name: string) => {
  fs.mkdirSync(SHOTS, { recursive: true });
  await page.screenshot({ path: `${SHOTS}/e2e-${name}.png` });
};

let errors: string[] = [];

test.beforeEach(async ({ page }) => {
  errors = [];
  page.on("pageerror", (e) => errors.push(`pageerror: ${e.message}`));
  page.on("console", (m) => m.type() === "error" && errors.push(`console.error: ${m.text()}`));
  // record every canvas the app creates, so desk screens can be read without a hook in the app
  await page.addInitScript(() => {
    const w = window as unknown as { __canvases: HTMLCanvasElement[] };
    w.__canvases = [];
    const make = document.createElement.bind(document);
    document.createElement = ((tag: string, o?: ElementCreationOptions) => {
      const el = make(tag, o);
      if (tag.toLowerCase() === "canvas") w.__canvases.push(el as HTMLCanvasElement);
      return el;
    }) as typeof document.createElement;
  });
});

test.afterEach(() => {
  expect(errors, "no console errors").toEqual([]);
});

async function open(page: Page, opts: { noWebgl?: boolean } = {}) {
  if (opts.noWebgl) {
    // xterm falls back to its DOM renderer, whose rows can be read as text
    await page.addInitScript(() => {
      const get = HTMLCanvasElement.prototype.getContext;
      HTMLCanvasElement.prototype.getContext = function (this: HTMLCanvasElement, kind: string, ...a: unknown[]) {
        if (kind === "webgl" || kind === "webgl2") return null;
        return (get as (...x: unknown[]) => unknown).call(this, kind, ...a);
      } as typeof get;
    });
  }
  await page.goto(url("/login?k=e2e-token"));
}

test("the boot log is gone within 3 s", async ({ page }) => {
  await open(page);
  await expect(page.locator(".boot")).toHaveCount(0, { timeout: 3000 });
  await shot(page, "boot");
});

test("the overview lists 4 agents with phase chips", async ({ page }) => {
  await open(page);
  const cards = page.locator(".card");
  await expect(cards).toHaveCount(4);
  const phases = await cards.evaluateAll((els) => els.map((e) => [e.getAttribute("data-phase"), e.querySelector(".card-phase")?.textContent?.trim()]));
  for (const [phase, chip] of phases) {
    expect(phase).toBeTruthy();
    expect(chip, `chip for ${phase}`).toBeTruthy();
  }
  expect(phases.map((p) => p[0])).toEqual(["standby", "standby", "standby", "disabled"]);
  await shot(page, "overview");
});

test("the centre terminal has non-empty rows", async ({ page }) => {
  await open(page, { noWebgl: true });
  const rows = page.locator(".center-term .xterm-rows > div");
  await expect.poll(async () => (await rows.evaluateAll((els) => els.filter((e) => e.textContent?.trim()).length)), { timeout: 10_000 }).toBeGreaterThan(3);
  await shot(page, "terminal");
});

test("F enters Focus and Esc leaves", async ({ page }) => {
  await open(page);
  await expect(page.locator(".card")).toHaveCount(4);
  const left = page.locator(".collapse-left");
  const width = async () => (await left.boundingBox())?.width ?? -1;
  await expect.poll(width).toBeGreaterThan(300);
  await page.keyboard.press("f");
  await expect.poll(width, { timeout: 5000 }).toBeLessThan(5);
  await shot(page, "focus");
  await page.keyboard.press("Escape");
  await expect.poll(width, { timeout: 5000 }).toBeGreaterThan(300);
});

test("Enter takes the keyboard and Ctrl+] gives it back", async ({ page }) => {
  await open(page);
  await expect(page.locator(".card")).toHaveCount(4);
  await page.keyboard.press("Enter");
  const bar = page.getByText("YOU HAVE THE KEYBOARD");
  await expect(bar).toBeVisible({ timeout: 5000 });
  await shot(page, "control");
  await page.keyboard.press("Control+]");
  await expect(bar).toHaveCount(0, { timeout: 5000 });
});

test("O opens the office and every desk screen has pixels", async ({ page }) => {
  await open(page);
  await expect(page.locator(".card")).toHaveCount(4);
  await page.keyboard.press("o");
  await expect(page.locator("canvas").first()).toBeVisible({ timeout: 10_000 });
  // 4 agents + the orchestrator: five 1760x800 desk canvases, each with something other than its background (an unpainted canvas is transparent and does not count)
  const lit = () =>
    page.evaluate((bg) => {
      const cs = (window as unknown as { __canvases: HTMLCanvasElement[] }).__canvases.filter((c) => c.width === 1760 && c.height === 800);
      return cs.map((c) => {
        const ctx = c.getContext("2d");
        if (!ctx) return -1; // not a desk screen: something else (xterm's) took this canvas for GL
        const d = ctx.getImageData(0, 0, c.width, c.height).data;
        let n = 0;
        for (let i = 0; i < d.length; i += 4) if (d[i + 3] > 0 && (d[i] !== bg[0] || d[i + 1] !== bg[1] || d[i + 2] !== bg[2])) n++;
        return n;
      });
    }, BG);
  await expect.poll(async () => (await lit()).filter((n) => n > 500).length, { timeout: 20_000 }).toBe(5);
  await page.waitForTimeout(1500); // let the scene settle for the picture
  await shot(page, "office");
});
