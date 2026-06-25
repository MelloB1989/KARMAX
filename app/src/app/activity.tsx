import type { ReactNode } from 'react';
import { ActivityIndicator } from 'react-native';

import { KM } from '@/components/km/colors';
import { formatLogTime } from '@/components/km/log-line';
import type { Job } from '@/lib/api';
import { useActivity, useDeviceActions, useRunJob } from '@/lib/hooks';
import { Pressable, ScrollView, Text, View } from '@/tw';
import { useConnection } from '@/stores/connection';

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <View className="gap-2">
      <Text className="font-mono text-[11px] uppercase text-km-muted">{title}</Text>
      {children}
    </View>
  );
}

function JobRow({ j }: { j: Job }) {
  const run = useRunJob();
  return (
    <View
      className="flex-row items-center gap-3 rounded-md border border-km-line bg-km-panel px-3 py-3"
      style={{ borderCurve: 'continuous' }}>
      <View className="flex-1 gap-0.5">
        <Text className="font-mono-medium text-[13px] text-km-text">{j.name}</Text>
        <Text className="font-mono text-[11px] text-km-muted">
          {`${j.cron}${j.next_run ? `  ·  next ${formatLogTime(j.next_run)}` : ''}  ·  ${j.run_count}×`}
        </Text>
      </View>
      <Pressable
        onPress={() => run.mutate(j.id)}
        disabled={run.isPending}
        className="rounded-md border border-km-line px-3 py-2"
        style={{ borderCurve: 'continuous' }}>
        <Text className="font-mono-medium text-[12px] text-km-amber">run</Text>
      </Pressable>
    </View>
  );
}

export default function ActivityScreen() {
  const connected = useConnection((s) => s.status === 'connected' && !!s.token);
  const { data, isLoading } = useActivity();
  const scheduled = useDeviceActions('');

  return (
    <ScrollView
      className="flex-1 bg-km-ink"
      contentContainerStyle={{ padding: 16, gap: 20 }}
      contentInsetAdjustmentBehavior="automatic">
      <View className="gap-1">
        <Text className="font-display text-2xl text-km-amber">activity</Text>
        <Text className="font-mono text-xs text-km-muted">what karmax is running.</Text>
      </View>

      {!connected ? (
        <Text className="font-mono text-xs text-km-muted">not connected — open config.</Text>
      ) : isLoading || !data ? (
        <ActivityIndicator color={KM.muted} style={{ marginTop: 20 }} />
      ) : (
        <>
          <Section title={`loops & jobs (${data.jobs.length})`}>
            {data.jobs.length === 0 ? (
              <Text className="font-mono text-xs text-km-muted">none</Text>
            ) : (
              data.jobs.map((j) => <JobRow key={j.id} j={j} />)
            )}
          </Section>

          <Section title={`coding sessions (${data.coding_sessions.length})`}>
            {data.coding_sessions.length === 0 ? (
              <Text className="font-mono text-xs text-km-muted">none</Text>
            ) : (
              data.coding_sessions.slice(0, 10).map((c) => (
                <View key={c.id} className="flex-row justify-between gap-2 border-b border-km-line pb-2">
                  <Text className="flex-1 font-mono text-[12px] text-km-text" numberOfLines={1}>
                    {`${c.tool}: ${c.description}`}
                  </Text>
                  <Text
                    className="font-mono text-[11px]"
                    style={{ color: c.status === 'completed' ? KM.green : c.status === 'failed' ? KM.red : KM.amber }}>
                    {c.status}
                  </Text>
                </View>
              ))
            )}
          </Section>

          <Section title="scheduled (calendar / reminders)">
            {(scheduled.data ?? []).length === 0 ? (
              <Text className="font-mono text-xs text-km-muted">nothing scheduled yet</Text>
            ) : (
              (scheduled.data ?? []).slice(0, 10).map((a) => {
                const title = (a.payload?.title as string | undefined) ?? a.kind;
                return (
                  <View key={a.id} className="flex-row justify-between gap-2 border-b border-km-line pb-2">
                    <Text className="flex-1 font-mono text-[12px] text-km-text" numberOfLines={1}>
                      {`${a.kind === 'reminder' ? '✓' : '📅'} ${title}`}
                    </Text>
                    <Text
                      className="font-mono text-[11px]"
                      style={{ color: a.status === 'done' ? KM.green : a.status === 'failed' ? KM.red : KM.amber }}>
                      {a.status}
                    </Text>
                  </View>
                );
              })
            )}
          </Section>

          <Section title={`webhooks (${data.webhooks.length})`}>
            {data.webhooks.length === 0 ? (
              <Text className="font-mono text-xs text-km-muted">none yet</Text>
            ) : (
              data.webhooks.map((wh) => (
                <Text key={wh.id} className="font-mono text-[12px] text-km-muted">
                  {`${wh.method} ${wh.route} · ${formatLogTime(wh.received_at)}`}
                </Text>
              ))
            )}
          </Section>

          <Section title="event stream">
            {data.events.length === 0 ? (
              <Text className="font-mono text-xs text-km-muted">quiet</Text>
            ) : (
              data.events.map((e) => (
                <Text key={e.id} className="font-mono text-[12px] leading-5">
                  <Text className="text-km-muted">{`[${formatLogTime(e.created_at)}] `}</Text>
                  <Text className="text-km-cyan">{e.kind}</Text>
                </Text>
              ))
            )}
          </Section>
        </>
      )}
    </ScrollView>
  );
}
