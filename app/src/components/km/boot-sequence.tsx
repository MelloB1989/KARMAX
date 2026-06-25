import { useEffect, useState } from 'react';
import { useReducedMotion } from 'react-native-reanimated';

import { View } from '@/tw';

import { LogLine } from './log-line';

const LINES = ['booting karmax…', 'linking daemon ✓', 'memory online ✓', 'awaiting input.'];

// Prints the boot lines one at a time on first render (the one orchestrated
// motion moment). Renders instantly when reduce-motion is on.
export function BootSequence() {
  const reduced = useReducedMotion();
  const [shown, setShown] = useState(reduced ? LINES.length : 0);

  useEffect(() => {
    if (reduced) return;
    let i = 0;
    const id = setInterval(() => {
      i += 1;
      setShown(i);
      if (i >= LINES.length) clearInterval(id);
    }, 360);
    return () => clearInterval(id);
  }, [reduced]);

  return (
    <View className="gap-1.5">
      {LINES.slice(0, shown).map((line, i) => (
        <LogLine key={i} role="sys" content={line} />
      ))}
    </View>
  );
}
