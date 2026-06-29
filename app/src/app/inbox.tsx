import { useLocalSearchParams } from 'expo-router';
import { useState } from 'react';
import { ActivityIndicator } from 'react-native';

import { KM } from '@/components/km/colors';
import type { Notification, Proposal } from '@/lib/api';
import {
  useDecideProposal,
  useMarkAllNotificationsRead,
  useMarkNotificationRead,
  useNotifications,
  useProposals,
} from '@/lib/hooks';
import { useSpeech } from '@/lib/speech';
import { Pressable, ScrollView, Text, TextInput, View } from '@/tw';
import { useConnection } from '@/stores/connection';

type Tab = 'approvals' | 'notifications';

function timeAgo(iso?: string): string {
  if (!iso) return '';
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return '';
  const s = Math.max(0, Math.floor((Date.now() - then) / 1000));
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

function StatusTag({ status }: { status: string }) {
  const color =
    status === 'executed'
      ? KM.green
      : status === 'failed' || status === 'rejected'
        ? KM.red
        : KM.amber;
  return (
    <Text className="font-mono text-[11px] uppercase" style={{ color }}>
      {status}
    </Text>
  );
}

function ProposalCard({ p }: { p: Proposal }) {
  const decide = useDecideProposal();
  const [action, setAction] = useState(p.action);
  const [note, setNote] = useState('');
  const busy = decide.isPending;

  return (
    <View
      className="gap-3 rounded-lg border border-km-line bg-km-panel p-4"
      style={{ borderCurve: 'continuous' }}>
      <View className="gap-1">
        <Text className="font-mono-bold text-[15px] text-km-amber">{p.title}</Text>
        {p.summary ? (
          <Text selectable className="font-mono text-xs leading-5 text-km-muted">
            {p.summary}
          </Text>
        ) : null}
      </View>

      <View className="gap-1">
        <Text className="font-mono text-[11px] uppercase text-km-muted">
          proposed action — edit if needed
        </Text>
        <TextInput
          className="rounded-md border border-km-line bg-km-ink px-3 py-2.5 font-mono text-[13px] leading-5 text-km-text"
          style={{ borderCurve: 'continuous' }}
          value={action}
          onChangeText={setAction}
          multiline
          editable={!busy}
        />
      </View>

      <View className="gap-1">
        <Text className="font-mono text-[11px] uppercase text-km-muted">
          feedback — sent to karmax with your decision
        </Text>
        <TextInput
          className="rounded-md border border-km-line bg-km-ink px-3 py-2.5 font-mono text-xs text-km-text"
          style={{ borderCurve: 'continuous' }}
          placeholder="e.g. why you're rejecting, or a tweak to make"
          placeholderTextColor={KM.muted}
          value={note}
          onChangeText={setNote}
          multiline
          editable={!busy}
        />
      </View>

      {decide.isError ? (
        <Text className="font-mono text-xs text-km-red">
          {`! ${(decide.error as Error)?.message ?? 'failed'}`}
        </Text>
      ) : null}

      <View className="flex-row gap-2">
        <Pressable
          disabled={busy}
          onPress={() => decide.mutate({ id: p.id, decision: 'reject', note: note.trim() || undefined })}
          className="flex-1 items-center rounded-md border border-km-line py-3"
          style={{ borderCurve: 'continuous' }}>
          <Text className="font-mono-medium text-[13px] text-km-red">
            {note.trim() ? 'reject with feedback' : 'reject'}
          </Text>
        </Pressable>
        <Pressable
          disabled={busy}
          onPress={() =>
            decide.mutate({
              id: p.id,
              decision: 'approve',
              edit: action !== p.action ? action : undefined,
              note: note.trim() || undefined,
            })
          }
          className="items-center rounded-md py-3"
          style={{ borderCurve: 'continuous', backgroundColor: KM.amber, flexGrow: 1.4, flexBasis: 0 }}>
          {busy ? (
            <ActivityIndicator color={KM.ink} />
          ) : (
            <Text className="font-mono-bold text-[13px]" style={{ color: KM.ink }}>
              approve &amp; run
            </Text>
          )}
        </Pressable>
      </View>
    </View>
  );
}

function DecidedRow({ p }: { p: Proposal }) {
  return (
    <View className="gap-1 border-b border-km-line pb-3">
      <View className="flex-row items-center justify-between gap-2">
        <Text className="flex-1 font-mono-medium text-[13px] text-km-text" numberOfLines={1}>
          {p.title}
        </Text>
        <StatusTag status={p.status} />
      </View>
      {p.note ? (
        <Text selectable className="font-mono text-[11px] leading-4 text-km-muted">
          {`your note: ${p.note}`}
        </Text>
      ) : null}
      {p.result ? (
        <Text selectable className="font-mono text-xs leading-5 text-km-muted">
          {p.result}
        </Text>
      ) : null}
    </View>
  );
}

function NotificationRow({ n, onPress }: { n: Notification; onPress: () => void }) {
  const speaking = useSpeech((s) => s.speakingId === `notif:${n.id}`);
  const toggle = useSpeech((s) => s.toggle);
  const title = n.title || (n.kind ? n.kind : 'update');
  return (
    <Pressable
      onPress={onPress}
      className="flex-row gap-2.5 rounded-lg border border-km-line bg-km-panel p-3.5"
      style={{ borderCurve: 'continuous', opacity: n.read ? 0.6 : 1 }}>
      <View
        style={{
          width: 7,
          height: 7,
          borderRadius: 4,
          marginTop: 5,
          backgroundColor: n.read ? 'transparent' : KM.amber,
        }}
      />
      <View className="flex-1 gap-1">
        <View className="flex-row items-center justify-between gap-2">
          <Text className="flex-1 font-mono-bold text-[13px] text-km-text" numberOfLines={1}>
            {title}
          </Text>
          <Pressable onPress={() => toggle(`notif:${n.id}`, `${title}. ${n.body}`)} hitSlop={10}>
            <Text className="font-mono text-[11px]" style={{ color: speaking ? KM.amber : KM.muted }}>
              {speaking ? '◼' : '🔊'}
            </Text>
          </Pressable>
          <Text className="font-mono text-[10px] text-km-muted">{timeAgo(n.created_at)}</Text>
        </View>
        <Text selectable className="font-mono text-xs leading-5 text-km-muted">
          {n.body}
        </Text>
      </View>
    </Pressable>
  );
}

function SegButton({
  active,
  label,
  badge,
  onPress,
}: {
  active: boolean;
  label: string;
  badge: number;
  onPress: () => void;
}) {
  return (
    <Pressable
      onPress={onPress}
      className="flex-1 flex-row items-center justify-center gap-1.5 rounded-md py-2"
      style={{
        borderCurve: 'continuous',
        backgroundColor: active ? KM.panel : 'transparent',
        borderWidth: 1,
        borderColor: active ? KM.line : 'transparent',
      }}>
      <Text className="font-mono-medium text-[12px]" style={{ color: active ? KM.amber : KM.muted }}>
        {label}
      </Text>
      {badge > 0 ? (
        <View style={{ borderRadius: 8, backgroundColor: KM.amber, paddingHorizontal: 5, paddingVertical: 1 }}>
          <Text className="font-mono-bold text-[10px]" style={{ color: KM.ink }}>
            {String(badge)}
          </Text>
        </View>
      ) : null}
    </Pressable>
  );
}

function Tabs({
  tab,
  setTab,
  pending,
  unread,
}: {
  tab: Tab;
  setTab: (t: Tab) => void;
  pending: number;
  unread: number;
}) {
  return (
    <View
      className="flex-row gap-2 rounded-lg border border-km-line bg-km-ink p-1"
      style={{ borderCurve: 'continuous' }}>
      <SegButton active={tab === 'approvals'} label="approvals" badge={pending} onPress={() => setTab('approvals')} />
      <SegButton
        active={tab === 'notifications'}
        label="notifications"
        badge={unread}
        onPress={() => setTab('notifications')}
      />
    </View>
  );
}

export default function InboxScreen() {
  const params = useLocalSearchParams<{ tab?: string }>();
  const connected = useConnection((s) => s.status === 'connected' && !!s.token);
  const [tab, setTab] = useState<Tab>(params.tab === 'notifications' ? 'notifications' : 'approvals');

  // Follow deep-link param changes without a sync effect (adjust state during
  // render, per the React "you might not need an effect" guidance).
  const [seenParam, setSeenParam] = useState(params.tab);
  if (params.tab !== seenParam) {
    setSeenParam(params.tab);
    if (params.tab === 'notifications' || params.tab === 'approvals') setTab(params.tab);
  }

  const { data: proposals = [], isLoading: pLoading } = useProposals('');
  const { data: notifData, isLoading: nLoading } = useNotifications();
  const markRead = useMarkNotificationRead();
  const markAll = useMarkAllNotificationsRead();

  const pending = proposals.filter((p) => p.status === 'pending');
  const decided = proposals.filter((p) => p.status !== 'pending');
  const notifications = notifData?.notifications ?? [];
  const unread = notifData?.unread ?? 0;

  return (
    <ScrollView
      className="flex-1 bg-km-ink"
      contentContainerStyle={{ padding: 16, gap: 14 }}
      contentInsetAdjustmentBehavior="automatic">
      <View className="gap-1">
        <Text className="font-display text-2xl text-km-amber">inbox</Text>
        <Text className="font-mono text-xs text-km-muted">
          {tab === 'approvals'
            ? 'karmax asks before it acts — approve, tweak, or reject with feedback.'
            : 'updates karmax has pushed to you — briefings, reminders, alerts.'}
        </Text>
      </View>

      <Tabs tab={tab} setTab={setTab} pending={pending.length} unread={unread} />

      {!connected ? (
        <Text className="font-mono text-xs text-km-muted">not connected — open config.</Text>
      ) : tab === 'approvals' ? (
        <>
          {pLoading ? (
            <ActivityIndicator style={{ marginTop: 24 }} color={KM.muted} />
          ) : pending.length === 0 && decided.length === 0 ? (
            <Text className="mt-6 font-mono text-xs leading-5 text-km-muted">
              no approvals yet — karmax will ask here when it wants to act (send a message, schedule
              something, follow up with someone).
            </Text>
          ) : null}

          {pending.map((p) => (
            <ProposalCard key={p.id} p={p} />
          ))}

          {decided.length > 0 ? (
            <View className="gap-3 pt-2">
              <Text className="font-mono text-[11px] uppercase text-km-muted">recent</Text>
              {decided.map((p) => (
                <DecidedRow key={p.id} p={p} />
              ))}
            </View>
          ) : null}
        </>
      ) : (
        <>
          {unread > 0 ? (
            <Pressable
              onPress={() => markAll.mutate()}
              className="self-end rounded-md border border-km-line px-3 py-1.5"
              style={{ borderCurve: 'continuous' }}>
              <Text className="font-mono text-[11px] text-km-muted">mark all read</Text>
            </Pressable>
          ) : null}

          {nLoading ? (
            <ActivityIndicator style={{ marginTop: 24 }} color={KM.muted} />
          ) : notifications.length === 0 ? (
            <Text className="mt-6 font-mono text-xs leading-5 text-km-muted">
              no updates yet — your morning briefing and any alerts karmax pushes will appear here.
            </Text>
          ) : (
            notifications.map((n) => (
              <NotificationRow
                key={n.id}
                n={n}
                onPress={() => {
                  if (!n.read) markRead.mutate(n.id);
                }}
              />
            ))
          )}
        </>
      )}
    </ScrollView>
  );
}
