import { useState } from 'react';
import { ActivityIndicator } from 'react-native';

import { KM } from '@/components/km/colors';
import { formatLogTime } from '@/components/km/log-line';
import { Memory3D, type MemNodeInfo } from '@/components/km/memory-3d';
import type { CleanupQuestion, MemoryEntry, MemTreeNode } from '@/lib/api';
import {
  useCleanupAnswer,
  useCleanupQuestion,
  useForgetEntry,
  useMemoryEntries,
  useMemoryTree,
  useProfile,
  useSaveProfile,
} from '@/lib/hooks';
import { Pressable, ScrollView, Text, TextInput, View } from '@/tw';
import { useConnection } from '@/stores/connection';

type Tab = 'entries' | 'tree' | '3d' | 'cleanup' | 'profile';
const TABS: Tab[] = ['entries', 'tree', '3d', 'cleanup', 'profile'];

function Segment({ value, onChange }: { value: Tab; onChange: (t: Tab) => void }) {
  return (
    <View className="flex-row gap-1 rounded-md border border-km-line p-1" style={{ borderCurve: 'continuous' }}>
      {TABS.map((t) => (
        <Pressable
          key={t}
          onPress={() => onChange(t)}
          className="flex-1 items-center rounded py-2"
          style={{ backgroundColor: value === t ? KM.panel : 'transparent' }}>
          <Text className="font-mono text-[12px]" style={{ color: value === t ? KM.amber : KM.muted }}>
            {t}
          </Text>
        </Pressable>
      ))}
    </View>
  );
}

function EntryRow({ e }: { e: MemoryEntry }) {
  const forget = useForgetEntry();
  return (
    <View className="gap-1 border-b border-km-line pb-3">
      <Text selectable className="font-mono text-[12px] leading-5 text-km-text">
        {e.content}
      </Text>
      <View className="flex-row items-center justify-between gap-2">
        <Text className="flex-1 font-mono text-[10px] text-km-muted" numberOfLines={1}>
          {`${e.tags.join(' · ') || e.role}${e.created_at ? `  ·  ${formatLogTime(e.created_at)}` : ''}`}
        </Text>
        <Pressable onPress={() => forget.mutate(e.id)} disabled={forget.isPending}>
          <Text className="font-mono text-[11px] text-km-red">forget</Text>
        </Pressable>
      </View>
    </View>
  );
}

function TreeBranch({ node, depth = 0 }: { node: MemTreeNode; depth?: number }) {
  const children = node.children ?? [];
  const hasChildren = children.length > 0;
  const [open, setOpen] = useState(depth < 1);
  return (
    <View style={{ paddingLeft: depth * 12 }}>
      <Pressable onPress={() => hasChildren && setOpen(!open)} className="py-1">
        <Text
          className="font-mono text-[12px]"
          style={{ color: depth === 0 ? KM.amber : KM.text }}
          numberOfLines={1}>
          {`${hasChildren ? (open ? '▾ ' : '▸ ') : '· '}${node.title || node.node_id || 'node'}`}
        </Text>
      </Pressable>
      {open ? children.map((c, i) => <TreeBranch key={c.node_id ?? i} node={c} depth={depth + 1} />) : null}
    </View>
  );
}

export default function MemoryScreen() {
  const connected = useConnection((s) => s.status === 'connected' && !!s.token);
  const [tab, setTab] = useState<Tab>('entries');
  const [q, setQ] = useState('');
  const [draft, setDraft] = useState<string | null>(null);

  const entries = useMemoryEntries(tab === 'entries' ? q : '');
  const tree = useMemoryTree();
  const [sel3d, setSel3d] = useState<MemNodeInfo | null>(null);

  const cleanupQ = useCleanupQuestion();
  const cleanupA = useCleanupAnswer();
  const [cq, setCq] = useState<CleanupQuestion | null>(null);
  const [cAnswer, setCAnswer] = useState('');
  const loadNextQuestion = () => {
    setCAnswer('');
    cleanupQ.mutate(undefined, { onSuccess: setCq });
  };
  const submitAnswer = (ans: string) => {
    if (!cq?.memory_id || !ans.trim()) return;
    cleanupA.mutate(
      { memory_id: cq.memory_id, memory: cq.memory ?? '', question: cq.question ?? '', answer: ans.trim() },
      { onSettled: loadNextQuestion },
    );
  };
  const profile = useProfile();
  const saveProfile = useSaveProfile();
  const forget = useForgetEntry();
  const profileValue = draft ?? profile.data ?? '';

  return (
    <ScrollView
      className="flex-1 bg-km-ink"
      contentContainerStyle={{ padding: 16, gap: 16 }}
      contentInsetAdjustmentBehavior="automatic">
      <View className="gap-1">
        <Text className="font-display text-2xl text-km-amber">memory</Text>
        <Text className="font-mono text-xs text-km-muted">what karmax knows — inspect & correct.</Text>
      </View>

      <Segment value={tab} onChange={setTab} />

      {!connected ? (
        <Text className="font-mono text-xs text-km-muted">not connected — open config.</Text>
      ) : null}

      {connected && tab === 'entries' ? (
        <View className="gap-3">
          <TextInput
            className="rounded-md border border-km-line bg-km-panel px-3 py-2.5 font-mono text-[13px] text-km-text"
            style={{ borderCurve: 'continuous' }}
            placeholder="search memory…"
            placeholderTextColor={KM.muted}
            value={q}
            onChangeText={setQ}
            autoCapitalize="none"
            autoCorrect={false}
          />
          {entries.isLoading ? (
            <ActivityIndicator color={KM.muted} />
          ) : (entries.data ?? []).length === 0 ? (
            <Text className="font-mono text-xs text-km-muted">no entries.</Text>
          ) : (
            (entries.data ?? []).map((e) => <EntryRow key={e.id} e={e} />)
          )}
        </View>
      ) : null}

      {connected && tab === 'tree' ? (
        <View className="gap-0.5">
          {tree.isLoading ? (
            <ActivityIndicator color={KM.muted} />
          ) : tree.data ? (
            <TreeBranch node={tree.data} />
          ) : (
            <Text className="font-mono text-xs text-km-muted">empty.</Text>
          )}
        </View>
      ) : null}

      {connected && tab === '3d' ? (
        tree.isLoading ? (
          <ActivityIndicator color={KM.muted} />
        ) : (
          <View className="gap-3">
            <Memory3D root={tree.data ?? null} onSelect={setSel3d} />
            {sel3d ? (
              <View
                className="gap-2 rounded-md border border-km-line bg-km-panel p-3"
                style={{ borderCurve: 'continuous' }}>
                <View className="flex-row items-start justify-between gap-3">
                  <Text className="flex-1 font-mono-medium text-[13px] text-km-amber">{sel3d.title}</Text>
                  {sel3d.depth > 1 ? (
                    <Pressable
                      onPress={() => {
                        forget.mutate(sel3d.id);
                        setSel3d(null);
                      }}
                      hitSlop={8}>
                      <Text className="font-mono text-[12px] text-km-red">forget</Text>
                    </Pressable>
                  ) : null}
                </View>
                {sel3d.content ? (
                  <Text className="font-mono text-[12.5px] leading-5 text-km-text">{sel3d.content}</Text>
                ) : (
                  <Text className="font-mono text-[12px] text-km-muted">
                    category node — tap a leaf node to see a memory.
                  </Text>
                )}
              </View>
            ) : (
              <Text className="font-mono text-[11px] text-km-muted">tap a node to see its memory here.</Text>
            )}
          </View>
        )
      ) : null}

      {connected && tab === 'cleanup' ? (
        <View className="gap-3">
          <Text className="font-mono text-[11px] leading-4 text-km-muted">
            KARMAX asks about memories it&apos;s unsure of. Pick an option or answer in your own words to correct
            and enrich what it knows.
          </Text>

          {!cq && !cleanupQ.isPending ? (
            <Pressable
              onPress={loadNextQuestion}
              className="items-center rounded-md bg-km-amber px-3 py-3"
              style={{ borderCurve: 'continuous' }}>
              <Text className="font-mono-bold text-[13px] text-km-ink">start memory cleanup</Text>
            </Pressable>
          ) : null}

          {cleanupQ.isPending ? <ActivityIndicator color={KM.muted} style={{ marginTop: 12 }} /> : null}

          {cq?.done ? (
            <View className="gap-3">
              <Text className="font-mono text-[12px] text-km-text">
                ✓ Everything looks well-contextualized right now.
              </Text>
              <Pressable onPress={loadNextQuestion}>
                <Text className="font-mono text-[11px] text-km-muted">check again</Text>
              </Pressable>
            </View>
          ) : null}

          {cq && cq.question ? (
            <View className="gap-3">
              <View
                className="gap-1 rounded-md border border-km-line bg-km-panel p-3"
                style={{ borderCurve: 'continuous' }}>
                <Text className="font-mono text-[10px] uppercase text-km-muted">about this memory</Text>
                <Text className="font-mono text-[11.5px] leading-4 text-km-muted">{cq.memory}</Text>
              </View>

              <Text className="font-mono-medium text-[14px] leading-5 text-km-text">{cq.question}</Text>

              <View className="gap-2">
                {(cq.options ?? []).map((opt) => (
                  <Pressable
                    key={opt}
                    disabled={cleanupA.isPending}
                    onPress={() => submitAnswer(opt)}
                    className="rounded-md border border-km-line bg-km-panel px-3 py-2.5"
                    style={{ borderCurve: 'continuous' }}>
                    <Text className="font-mono text-[13px] text-km-text">{opt}</Text>
                  </Pressable>
                ))}
              </View>

              <View className="flex-row items-end gap-2">
                <TextInput
                  className="max-h-24 flex-1 rounded-md border border-km-line bg-km-panel px-3 py-2.5 font-mono text-[13px] text-km-text"
                  placeholder="or answer in your own words…"
                  placeholderTextColor={KM.muted}
                  value={cAnswer}
                  onChangeText={setCAnswer}
                  editable={!cleanupA.isPending}
                  multiline
                />
                <Pressable
                  disabled={cleanupA.isPending || !cAnswer.trim()}
                  onPress={() => submitAnswer(cAnswer)}
                  className="h-10 w-11 items-center justify-center rounded-md"
                  style={{ borderCurve: 'continuous', backgroundColor: cAnswer.trim() ? KM.amber : KM.panel }}>
                  <Text className="font-mono-bold text-base" style={{ color: cAnswer.trim() ? KM.ink : KM.muted }}>
                    ↵
                  </Text>
                </Pressable>
              </View>

              {cleanupA.isPending ? (
                <Text className="font-mono text-[11px] text-km-muted">applying & finding the next one…</Text>
              ) : (
                <Pressable onPress={loadNextQuestion}>
                  <Text className="font-mono text-[11px] text-km-muted">skip → next</Text>
                </Pressable>
              )}
            </View>
          ) : null}
        </View>
      ) : null}

      {connected && tab === 'profile' ? (
        <View className="gap-3">
          {profile.isLoading ? (
            <ActivityIndicator color={KM.muted} />
          ) : (
            <>
              <TextInput
                className="rounded-md border border-km-line bg-km-panel px-3 py-3 font-mono text-[12px] leading-5 text-km-text"
                style={{ borderCurve: 'continuous', minHeight: 260 }}
                multiline
                value={profileValue}
                onChangeText={setDraft}
                placeholder="(empty — karmax hasn't written your profile yet)"
                placeholderTextColor={KM.muted}
              />
              <Pressable
                onPress={() => {
                  if (draft != null) saveProfile.mutate(draft);
                }}
                disabled={saveProfile.isPending || draft == null}
                className="items-center rounded-md py-3"
                style={{ borderCurve: 'continuous', backgroundColor: KM.amber }}>
                <Text className="font-mono-bold text-[13px]" style={{ color: KM.ink }}>
                  {saveProfile.isPending ? 'saving…' : 'save profile'}
                </Text>
              </Pressable>
            </>
          )}
        </View>
      ) : null}
    </ScrollView>
  );
}
