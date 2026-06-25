import { useEffect } from 'react';
import Animated, {
  useAnimatedStyle,
  useReducedMotion,
  useSharedValue,
  withRepeat,
  withSequence,
  withTiming,
} from 'react-native-reanimated';

import { KM, MONO } from './colors';

// A blinking terminal block cursor. Static when reduce-motion is on.
export function Caret({ color = KM.amber, size = 13 }: { color?: string; size?: number }) {
  const reduced = useReducedMotion();
  const opacity = useSharedValue(1);

  useEffect(() => {
    if (reduced) return;
    opacity.value = withRepeat(
      withSequence(withTiming(1, { duration: 520 }), withTiming(0.12, { duration: 520 })),
      -1,
      true,
    );
  }, [reduced, opacity]);

  const animStyle = useAnimatedStyle(() => ({ opacity: opacity.value }));

  return (
    <Animated.Text style={[{ color, fontFamily: MONO, fontSize: size }, animStyle]}>▮</Animated.Text>
  );
}
