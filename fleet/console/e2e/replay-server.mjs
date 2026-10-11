// TEST ONLY. Serves the built panel (../internal/console/dist) and replays dev/ fixtures over the
// same routes fleet-console serves, so the production bundle is exercised end to end. Never shipped,
// never imported by src/. Binds 127.0.0.1 on a free port.
import http from "node:http";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { WebSocketServer } from "ws";

const here = path.dirname(fileURLToPath(import.meta.url));
const DIST = path.resolve(here, "../../internal/console/dist");
const DEV = path.resolve(here, "../dev");
export const TOKEN = "e2e-token";
const COOKIE = "fleet_session=e2e";

const TYPES = {
  ".html": "text/html; charset=utf-8", ".js": "text/javascript", ".css": "text/css", ".json": "application/json",
  ".woff2": "font/woff2", ".woff": "font/woff", ".svg": "image/svg+xml", ".png": "image/png", ".ico": "image/x-icon",
};
const ISO = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/;

/** The fixture with every timestamp shifted so the snapshot's own `at` is now: nothing reads stale. */
export function freshSnapshot(base, nowMs = Date.now()) {
  const delta = nowMs - Date.parse(base.at);
  const shift = (v) => {
    if (typeof v === "string" && ISO.test(v)) return new Date(Date.parse(v) + delta).toISOString();
    if (Array.isArray(v)) return v.map(shift);
    if (v && typeof v === "object") return Object.fromEntries(Object.entries(v).map(([k, x]) => [k, shift(x)]));
    return v;
  };
  return shift(base);
}

const authed = (req) => (req.headers.cookie ?? "").includes(COOKIE);

export function startReplayServer() {
  const base = JSON.parse(fs.readFileSync(path.join(DEV, "snapshot.json"), "utf8"));
  const server = http.createServer((req, res) => {
    const url = new URL(req.url, "http://x");
    if (url.pathname === "/login") {
      if (url.searchParams.get("k") !== TOKEN) {
        res.writeHead(403).end("bad token");
        return;
      }
      res.writeHead(302, { "Set-Cookie": `${COOKIE}; HttpOnly; Path=/`, Location: "/" }).end();
      return;
    }
    if (!authed(req)) {
      res.writeHead(401).end("login first");
      return;
    }
    if (url.pathname.startsWith("/v1/")) {
      res.writeHead(404).end("not replayed");
      return;
    }
    let file = path.join(DIST, url.pathname === "/" ? "index.html" : url.pathname);
    if (!file.startsWith(DIST) || !fs.existsSync(file) || fs.statSync(file).isDirectory()) {
      res.writeHead(404).end("not found");
      return;
    }
    res.writeHead(200, { "Content-Type": TYPES[path.extname(file)] ?? "application/octet-stream" });
    fs.createReadStream(file).pipe(res);
  });

  const wss = new WebSocketServer({ noServer: true });
  server.on("upgrade", (req, socket, head) => {
    const url = new URL(req.url, "http://x");
    if (!authed(req)) {
      socket.write("HTTP/1.1 401 Unauthorized\r\n\r\n");
      socket.destroy();
      return;
    }
    wss.handleUpgrade(req, socket, head, (ws) => {
      if (url.pathname === "/v1/watch") return watch(ws, base);
      const m = url.pathname.match(/^\/v1\/term\/([^/]+)$/);
      if (m) return term(ws, decodeURIComponent(m[1]), url.searchParams.get("control") === "1");
      ws.close();
    });
  });

  return new Promise((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      const port = server.address().port;
      resolve({
        url: `http://127.0.0.1:${port}`,
        close: () =>
          new Promise((r) => {
            wss.clients.forEach((c) => c.terminate());
            server.close(() => r());
          }),
      });
    });
  });
}

// A snapshot at once and a fresh one every second, so no source ages while a test runs.
function watch(ws, base) {
  let seq = 1;
  const send = () => ws.readyState === 1 && ws.send(JSON.stringify({ type: "snapshot", seq: seq++, snapshot: freshSnapshot(base) }));
  send();
  const t = setInterval(send, 1000);
  ws.on("close", () => clearInterval(t));
}

function term(ws, agent, control) {
  const file = path.join(DEV, `term-${agent}.ans`);
  if (!fs.existsSync(file)) {
    ws.send(JSON.stringify({ type: "error", agent, error: "no such agent" }));
    ws.close();
    return;
  }
  ws.send(JSON.stringify({ type: "hello", agent, mode: control ? "control" : "watch", cols: 220, rows: 50 }));
  ws.send(Buffer.from(fs.readFileSync(file, "utf8").replace(/\n/g, "\r\n")), { binary: true });
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const s = await startReplayServer();
  console.log(`replay server on ${s.url}/login?k=${TOKEN}`);
}
