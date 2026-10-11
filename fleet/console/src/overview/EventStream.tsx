import type { FleetEvent } from "../model";

export function EventStream({ events }: { events: FleetEvent[] | null }) {
  return (
    <ul className="events">
      {(events ?? []).slice(0, 60).map((e) => (
        <li key={e.id}>
          <span className="dim">{new Date(e.at).toLocaleTimeString("en-GB")}</span> <span className="ev-agent">{e.agent}</span>{" "}
          <span className="ev-kind">{e.kind}</span>
        </li>
      ))}
    </ul>
  );
}
