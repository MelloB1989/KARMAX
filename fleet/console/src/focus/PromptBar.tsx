import { useEffect, useRef, useState } from "react";
import { prompt } from "../api";

type Status = { s: "idle" } | { s: "pending" } | { s: "ok" } | { s: "error"; msg: string };

/** Types into `target`'s session as the operator, through the console. */
export function PromptBar({ target, label, onClose }: { target: string; label: string; onClose: () => void }) {
  const [text, setText] = useState("");
  const [st, setSt] = useState<Status>({ s: "idle" });
  const input = useRef<HTMLInputElement>(null);
  useEffect(() => input.current?.focus(), []);

  const send = async () => {
    if (!text.trim() || st.s === "pending") return;
    setSt({ s: "pending" });
    try {
      await prompt(target, text);
      setSt({ s: "ok" });
      setText("");
    } catch (e) {
      setSt({ s: "error", msg: e instanceof Error ? e.message : String(e) });
    }
  };

  return (
    <div className="prompt-bar">
      <span className="prompt-label">{label}</span>
      <input
        ref={input}
        value={text}
        placeholder="type, Enter to send, Esc to close"
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter") void send();
          if (e.key === "Escape") onClose();
        }}
      />
      <span className={`prompt-status ${st.s}`}>
        {st.s === "pending" ? "sending…" : st.s === "ok" ? "sent" : st.s === "error" ? st.msg : ""}
      </span>
    </div>
  );
}
