import { startReplayServer } from "./replay-server.mjs";

export default async function globalSetup() {
  const s = await startReplayServer();
  process.env.E2E_URL = s.url;
  return () => s.close();
}
