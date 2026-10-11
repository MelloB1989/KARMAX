import { useEffect, useRef, useState } from "react";
import { useReducedMotion } from "motion/react";

export const TWEEN_MS = 400;

/** Eases `from`→`to` by t in 0..1. */
export const easeOut = (t: number): number => 1 - (1 - t) ** 3;

/** A number that counts to its new value over 400 ms; instant under reduced motion. */
export function useTween(value: number, ms = TWEEN_MS): number {
  const reduce = useReducedMotion();
  const [shown, setShown] = useState(value);
  const from = useRef(value);
  const cur = useRef(value);
  useEffect(() => {
    if (reduce || !Number.isFinite(value) || value === cur.current) {
      cur.current = value;
      setShown(value);
      return;
    }
    from.current = cur.current;
    const start = performance.now();
    let raf = 0;
    const loop = (now: number) => {
      const t = Math.min(1, (now - start) / ms);
      cur.current = from.current + (value - from.current) * easeOut(t);
      setShown(cur.current);
      if (t < 1) raf = requestAnimationFrame(loop);
    };
    raf = requestAnimationFrame(loop);
    return () => cancelAnimationFrame(raf);
  }, [value, ms, reduce]);
  return shown;
}

export function Num({ value, digits = 0 }: { value: number; digits?: number }) {
  return <>{useTween(value).toFixed(digits)}</>;
}
