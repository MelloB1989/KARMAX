import { NativeTabs } from 'expo-router/unstable-native-tabs';

import { KM } from '@/components/km/colors';
import { useNotifications, useProposals } from '@/lib/hooks';

export default function AppTabs() {
  const { data: pending = [] } = useProposals('pending');
  const { data: notifData } = useNotifications();
  const count = pending.length + (notifData?.unread ?? 0);

  return (
    <NativeTabs
      backgroundColor={KM.ink}
      indicatorColor={KM.line}
      labelStyle={{ color: KM.muted, selected: { color: KM.amber } }}>
      <NativeTabs.Trigger name="index">
        <NativeTabs.Trigger.Label>chat</NativeTabs.Trigger.Label>
        <NativeTabs.Trigger.Icon sf="terminal" />
      </NativeTabs.Trigger>

      <NativeTabs.Trigger name="inbox">
        <NativeTabs.Trigger.Label>inbox</NativeTabs.Trigger.Label>
        <NativeTabs.Trigger.Icon sf="tray.full" />
        {count > 0 ? <NativeTabs.Trigger.Badge>{String(count)}</NativeTabs.Trigger.Badge> : null}
      </NativeTabs.Trigger>

      <NativeTabs.Trigger name="activity">
        <NativeTabs.Trigger.Label>activity</NativeTabs.Trigger.Label>
        <NativeTabs.Trigger.Icon sf="bolt.fill" />
      </NativeTabs.Trigger>

      <NativeTabs.Trigger name="memory">
        <NativeTabs.Trigger.Label>memory</NativeTabs.Trigger.Label>
        <NativeTabs.Trigger.Icon sf="brain" />
      </NativeTabs.Trigger>

      <NativeTabs.Trigger name="settings">
        <NativeTabs.Trigger.Label>config</NativeTabs.Trigger.Label>
        <NativeTabs.Trigger.Icon sf="slider.horizontal.3" />
      </NativeTabs.Trigger>
    </NativeTabs>
  );
}
