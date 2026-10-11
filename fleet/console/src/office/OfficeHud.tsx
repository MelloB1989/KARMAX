import { palette } from "../state/derive";

const hudBtn: React.CSSProperties = {
  all: "unset", cursor: "pointer", padding: "6px 12px", marginRight: 8, border: `1px solid ${palette.dim}`, letterSpacing: 2,
};

/** The selected desk's actions, as buttons: over a remote desktop Ctrl+] and Esc may never reach the page. */
export function OfficeHud({ error, control, canControl, onTake, onGiveBack, onLeave }: {
  error: string | null;
  control: boolean;
  canControl: boolean;
  onTake: () => void;
  onGiveBack: () => void;
  onLeave: () => void;
}) {
  return (
    <div style={{ position: "absolute", left: 16, bottom: 16, color: palette.dim, font: '12px "IBM Plex Mono", monospace', display: "flex", alignItems: "center" }}>
      {error && <span style={{ color: palette.alert, marginRight: 8 }}>{error}</span>}
      {control ? (
        <button type="button" style={{ ...hudBtn, background: palette.amber, color: "#07080C", border: 0 }} onClick={onGiveBack}>GIVE BACK · Ctrl+]</button>
      ) : (
        canControl && <button type="button" style={hudBtn} onClick={onTake}>TAKE CONTROL ⏎</button>
      )}
      {!control && <button type="button" style={hudBtn} onClick={onLeave}>LEAVE · Esc</button>}
    </div>
  );
}
