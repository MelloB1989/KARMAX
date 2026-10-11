import type { Snapshot } from "../model";
import { ageOf, fmtAge } from "../state/derive";
import { sourceStale } from "../state/cadence";
import { Bar } from "./Bar";
import { Num } from "../motion/tween";
import { EventStream } from "./EventStream";

export function RightRail({ snapshot: s, now }: { snapshot: Snapshot; now: number }) {
  const o = s.orchestrator;
  const stale = sourceStale(s, "fleetd", now);
  return (
    <aside className="rail">
      <section>
        <h3>ORCHESTRATOR</h3>
        {o ? (
          stale ? (
            <div className="dim">{o.name} · {fmtAge(ageOf(o.at, now))} ago</div>
          ) : (
            <>
              <div>{o.name}</div>
              <Bar label="5h" pct={o.five_hour_pct} />
              <Bar label="7d" pct={o.seven_day_pct} />
            </>
          )
        ) : (
          <div className="dim">none</div>
        )}
      </section>
      <section>
        <h3>OPEN TASKS</h3>
        {(s.tasks ?? []).length === 0 && <div className="dim">none</div>}
        {(s.tasks ?? []).map((t) => (
          <div key={t.id} className="task">
            <span className="ev-agent">{t.agent}</span> {t.id} <span className="dim">{fmtAge(ageOf(t.since, now))}</span>
          </div>
        ))}
      </section>
      <section>
        <h3>TODAY</h3>
        {stale ? (
          <div className="dim">fleetd {fmtAge(ageOf(s.sources.fleetd?.at ?? "", now))} ago</div>
        ) : (
          <div>
            <Num value={s.today.tasks_closed} /> tasks · $<Num value={s.today.spend_usd} digits={2} />
          </div>
        )}
      </section>
      <section className="grow">
        <h3>EVENTS</h3>
        <EventStream events={s.events} />
      </section>
    </aside>
  );
}
