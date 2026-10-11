import type { Snapshot } from "../model";
import type { Conn } from "../state/store";
import { ageOf, agoText, fmtAge, isKnown } from "../state/derive";
import { sourceStale } from "../state/cadence";
import { gpuName } from "../state/hooks";

const gib = (b: number) => (b / 1073741824).toFixed(1);

function Vital({ k, v, dim, title, cls }: { k: string; v: string; dim?: boolean; title?: string; cls?: string }) {
  return (
    <div className={`vital${cls ? " " + cls : ""}`} title={title}>
      <span className="vital-k">{k}</span>
      <span className={dim ? "dim" : ""}>{v}</span>
    </div>
  );
}

export function Header({ snapshot: s, conn, now, gpu }: { snapshot: Snapshot | null; conn: Conn; now: number; gpu: string }) {
  const h = s?.host;
  const hostStale = s ? sourceStale(s, "host", now) : true;
  const gitStale = s ? sourceStale(s, "git", now) : true;
  const main = s?.git.repos?.[0]?.main;
  const aged = (at: string) => agoText(at, now);
  const up = h ? h.name.toUpperCase() : "—";
  const d = new Date(now);
  return (
    <header className="header">
      <div className="brand">
        <span className="brand-name">FLEET CONSOLE</span>
        <span className="brand-sub">OPERATIONS · {up}</span>
      </div>
      <div className="grow" />
      {h && (
        <>
          <Vital k={`${up} UPTIME`} v={hostStale ? aged(h.at) : fmtAge(h.uptime_s)} dim={hostStale} />
          <Vital k="LOAD 1·5·15" v={hostStale ? aged(h.at) : h.load.map((l) => l.toFixed(2)).join(" ")} dim={hostStale} />
          <Vital k="MEMORY" v={hostStale ? aged(h.at) : `${gib(h.mem_used)} / ${gib(h.mem_total)} GiB`} dim={hostStale} />
          <Vital k="CORES · GPU" v={`${h.cpus} · ${gpuName(gpu)}`} title={gpu} cls="vital-gpu" />
        </>
      )}
      {main && isKnown(main.at) && <Vital k="MAIN" v={gitStale ? aged(main.at) : `${main.sha.slice(0, 7)} · ${fmtAge(ageOf(main.at, now))} ago`} dim={gitStale} />}
      <div className={`conn conn-${conn}`}>{conn}</div>
      <div className="clock">
        <span className="clock-time">{d.toLocaleTimeString("en-GB")}</span>
        <span className="vital-k">{d.toLocaleDateString("en-GB", { weekday: "short", day: "numeric", month: "short" }).toUpperCase()}</span>
      </div>
    </header>
  );
}
