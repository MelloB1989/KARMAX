// An agent's real terminal: watched through the shared stream, or held
// through a control socket. The xterm keeps the agent's own size (cols×rows)
// and is scaled to fit its box, so nothing ever resizes the agent's window.
import { useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { WebglAddon } from "@xterm/addon-webgl";
import "@xterm/xterm/css/xterm.css";
import type { TermFrame } from "../model";
import { subscribeTerm } from "./stream";
import { applyControlFrame, openControl } from "./control";
import { setControlHeld } from "../keys";

export interface LiveTerminalProps {
  agent: string;
  cols: number;
  rows: number;
  control?: boolean;
  onRelease?: () => void;
  onFrame?: (f: TermFrame) => void;
  fit?: "width" | "contain";
  fontSize?: number;
}

const theme = {
  background: "#03060A",
  foreground: "#C9D6E0",
  cursor: "#D6E2EC",
  selectionBackground: "#1B2A38",
};

export function LiveTerminal({ agent, cols, rows, control = false, onRelease, onFrame, fit = "contain", fontSize = 14 }: LiveTerminalProps) {
  const box = useRef<HTMLDivElement>(null);
  const host = useRef<HTMLDivElement>(null);
  const [scale, setScale] = useState(1);
  const [note, setNote] = useState<string | null>("waiting for terminal…");
  const frameRef = useRef(onFrame);
  frameRef.current = onFrame;
  const releaseRef = useRef(onRelease);
  releaseRef.current = onRelease;

  useEffect(() => {
    if (!host.current) return;
    setNote("waiting for terminal…");
    const term = new Terminal({
      cols,
      rows,
      fontSize,
      scrollback: 0,
      fontFamily: "'IBM Plex Mono', monospace",
      theme,
      disableStdin: !control,
      cursorBlink: control,
      allowProposedApi: true,
    });
    term.open(host.current);
    try {
      term.loadAddon(new WebglAddon());
    } catch {
      // DOM renderer: slower, still correct
    }
    let stop: () => void;
    if (control) {
      const c = openControl(agent);
      c.onData((b) => {
        setNote(null);
        term.write(b);
      });
      c.onFrame((f) => {
        frameRef.current?.(f);
        if (f.type === "reconnecting") setNote("reconnecting…");
        applyControlFrame(f, () => c.close());
        if (f.type === "released") releaseRef.current?.();
      });
      const input = term.onData((d) => c.send(d));
      term.focus();
      stop = () => {
        input.dispose();
        setControlHeld(false);
        c.close();
      };
    } else {
      stop = subscribeTerm(
        agent,
        (b) => {
          setNote(null);
          term.write(b);
        },
        (f) => {
          frameRef.current?.(f);
          if (f.type === "reconnecting") setNote("reconnecting…");
        },
      );
    }
    return () => {
      stop();
      term.dispose();
    };
  }, [agent, cols, rows, control, fontSize]);

  useEffect(() => {
    const b = box.current;
    const h = host.current;
    if (!b || !h) return;
    const measure = () => {
      const w = h.scrollWidth || 1;
      const ht = h.scrollHeight || 1;
      const sx = b.clientWidth / w;
      setScale(fit === "width" ? sx : Math.min(sx, b.clientHeight / ht));
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(b);
    return () => ro.disconnect();
  }, [fit]);

  return (
    <div ref={box} style={{ width: "100%", height: "100%", overflow: "hidden", position: "relative" }}>
      {note && <div className="term-note">{note}</div>}
      <div ref={host} style={{ transform: `scale(${scale})`, transformOrigin: "top left", width: "max-content" }} />
    </div>
  );
}
