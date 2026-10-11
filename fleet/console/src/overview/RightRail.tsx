import type { Snapshot } from "../model";
import { ageOf, agoText, fmtAge } from "../state/derive";
import { sourceNote, sourceStale } from "../state/cadence";
import { Num } from "../motion/tween";
import { EventStream } from "./EventStream";
import { Rings } from "./Rings";

const when = (iso?: string) => (iso ? new Date(iso).toLocaleString("en-GB", { weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" }) : "—");

export function RightRail({ snapshot: s, now }: { snapshot: Snapshot; now: number }) {
  const o = s.orchestrator;
  const stale = sourceStale(s, "fleetd", now);
  const gitStale = sourceStale(s, "git", now);
  const commits = (s.git.repos ?? []).reduce((n, r) => n + r.commits_7d, 0);
  const tasks = s.tasks ?? [];
  const fleetdNote = sourceNote(s, "fleetd", now);
  const gitNote = sourceNote(s, "git", now);
  return (
    <aside className="rail">
      <section className="orch">
        {o ? (
          stale ? (
            <div className="dim" title={fleetdNote.title}>
              {o.name} · {agoText(o.at, now)}
            </div>
          ) : (
            <>
              <Rings five={o.five_hour_pct} seven={o.seven_day_pct} />
              <div className="orch-text">
                <span className="orch-name">
                  {o.name.toUpperCase()} <span className="dim">ORCHESTRATOR</span>
                </span>
                <span>
                  <span className="amber">5h</span> {o.five_hour_pct}% · resets {when(o.five_hour_resets)}
                </span>
                <span>
                  <span className="cyan">7d</span> {o.seven_day_pct}% · resets {when(o.seven_day_resets)}
                </span>
                <span className="dim">
                  {tasks.length === 0
                    ? "no open assignments"
                    : tasks.map((t) => `${t.agent} ${t.id} ${fmtAge(ageOf(t.since, now))}`).join(" · ")}
                </span>
              </div>
            </>
          )
        ) : (
          <div className="dim" title={fleetdNote.title}>orchestrator: no data from fleetd</div>
        )}
      </section>
      <div className="tiles">
        <div className="tile">
          <div className="tile-k">TASKS CLOSED</div>
          <div className="tile-v">{stale ? <span className="dim" title={fleetdNote.title}>{fleetdNote.text}</span> : <Num value={s.today.tasks_closed} />}</div>
          <div className="tile-sub dim">{tasks.length} open</div>
        </div>
        <div className="tile">
          <div className="tile-k">SPEND · TODAY</div>
          <div className="tile-v">{stale ? <span className="dim" title={fleetdNote.title}>{fleetdNote.text}</span> : <>$<Num value={s.today.spend_usd} digits={2} /></>}</div>
          <div className="tile-sub dim">since midnight</div>
        </div>
        <div className="tile">
          <div className="tile-k">COMMITS · 7D</div>
          <div className="tile-v">{gitStale ? <span className="dim" title={gitNote.title}>{gitNote.text}</span> : <Num value={commits} />}</div>
          <div className="tile-sub dim">on {(s.git.repos ?? []).length} repos</div>
        </div>
      </div>
      <section className="events-card">
        <h3>EVENT STREAM</h3>
        <EventStream events={s.events} />
      </section>
    </aside>
  );
}
