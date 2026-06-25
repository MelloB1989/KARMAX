import { useState } from 'react';
import { ActivityIndicator } from 'react-native';

import { KM } from '@/components/km/colors';
import type { Proposal } from '@/lib/api';
import { useDecideProposal, useProposals } from '@/lib/hooks';
import { Pressable, ScrollView, Text, TextInput, View } from '@/tw';
import { useConnection } from '@/stores/connection';

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

      <TextInput
        className="rounded-md border border-km-line bg-km-ink px-3 py-2.5 font-mono text-xs text-km-text"
        style={{ borderCurve: 'continuous' }}
        placeholder="note to karmax (optional)"
        placeholderTextColor={KM.muted}
        value={note}
        onChangeText={setNote}
        editable={!busy}
      />

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
          <Text className="font-mono-medium text-[13px] text-km-red">reject</Text>
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
              approve & run
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
      {p.result ? (
        <Text selectable className="font-mono text-xs leading-5 text-km-muted">
          {p.result}
        </Text>
      ) : null}
    </View>
  );
}

export default function InboxScreen() {
  const connected = useConnection((s) => s.status === 'connected' && !!s.token);
  const { data: proposals = [], isLoading } = useProposals('');

  const pending = proposals.filter((p) => p.status === 'pending');
  const decided = proposals.filter((p) => p.status !== 'pending');

  return (
    <ScrollView
      className="flex-1 bg-km-ink"
      contentContainerStyle={{ padding: 16, gap: 14 }}
      contentInsetAdjustmentBehavior="automatic">
      <View className="gap-1">
        <Text className="font-display text-2xl text-km-amber">approvals</Text>
        <Text className="font-mono text-xs text-km-muted">
          karmax asks before it acts — approve, tweak, or reject.
        </Text>
      </View>

      {!connected ? (
        <Text className="font-mono text-xs text-km-muted">not connected — open config.</Text>
      ) : isLoading ? (
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
    </ScrollView>
  );
}
