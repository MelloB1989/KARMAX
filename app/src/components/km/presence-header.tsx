import { Text, View } from '@/tw';
import { useConnection } from '@/stores/connection';

import { Caret } from './caret';
import { KM } from './colors';

export function PresenceHeader() {
  const status = useConnection((s) => s.status);
  const agent = useConnection((s) => s.agent);

  const label =
    status === 'connected'
      ? 'online'
      : status === 'connecting'
        ? 'connecting…'
        : status === 'error'
          ? 'offline'
          : '—';
  const dot = status === 'connected' ? KM.green : status === 'connecting' ? KM.amber : KM.muted;

  return (
    <View className="flex-row items-center justify-between border-b border-km-line px-4 py-3">
      <View className="flex-row items-baseline">
        <Text className="font-display text-base text-km-amber">karmax</Text>
        <Text className="font-mono text-base text-km-muted">{`@${agent ?? 'nexus'}`}</Text>
        <Caret color={KM.amber} size={15} />
      </View>
      <View className="flex-row items-center gap-2">
        <View style={{ width: 7, height: 7, borderRadius: 4, backgroundColor: dot }} />
        <Text className="font-mono text-xs text-km-muted">{label}</Text>
      </View>
    </View>
  );
}
