import { useEffect, useRef, useState } from "react";

/** True for `ms` after `value` changes (not on first render). */
export function useFlash(value: string, ms = 600): boolean {
  const prev = useRef(value);
  const [on, setOn] = useState(false);
  useEffect(() => {
    if (prev.current === value) return;
    prev.current = value;
    setOn(true);
    const id = setTimeout(() => setOn(false), ms);
    return () => clearTimeout(id);
  }, [value, ms]);
  return on;
}
