import { Link } from 'expo-router';
import { useRef, useState } from 'react';
import { ActivityIndicator, FlatList, KeyboardAvoidingView } from 'react-native';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { BootSequence } from '@/components/km/boot-sequence';
import { Caret } from '@/components/km/caret';
import { KM } from '@/components/km/colors';
import { LogLine, type LogRole } from '@/components/km/log-line';
import { KrmxMarkdown } from '@/components/km/markdown-message';
import { PresenceHeader } from '@/components/km/presence-header';
import type { Message } from '@/lib/api';
import { useMessages, useResetConversation, useSendMessage } from '@/lib/hooks';
import { Pressable, Text, TextInput, View } from '@/tw';
import { useConnection } from '@/stores/connection';

type Row = { id: string; role: LogRole; content?: string; time?: string; thinking?: boolean };

function sortOldestFirst(messages: Message[]): Message[] {
  return [...messages].sort((a, b) => (a.created_at ?? '').localeCompare(b.created_at ?? ''));
}

function ConfigNotice({ message }: { message: string }) {
  return (
    <Link href="/settings" asChild>
      <Pressable className="px-4 py-4">
        <LogLine role="sys" content={message} />
      </Pressable>
    </Link>
  );
}

export default function ChatScreen() {
  const insets = useSafeAreaInsets();
  const status = useConnection((s) => s.status);
  const hasToken = useConnection((s) => !!s.token);
  const canChat = status === 'connected' && hasToken;

  const { data: messages = [], isLoading } = useMessages();
  const send = useSendMessage();
  const reset = useResetConversation();
  const [text, setText] = useState('');
  const [pending, setPending] = useState<string | null>(null);

  const now = new Date().toISOString();
  const listRef = useRef<FlatList<Row>>(null);
  const rows: Row[] = [];
  sortOldestFirst(messages).forEach((m, i) =>
    rows.push({
      id: `${m.created_at ?? 'm'}-${i}`,
      role: m.role === 'user' ? 'you' : 'krmx',
      content: m.content,
      time: m.created_at,
    }),
  );
  if (pending) {
    rows.push({ id: 'pending-you', role: 'you', content: pending, time: now });
    rows.push({ id: 'thinking', role: 'krmx', time: now, thinking: true });
  }

  const onSend = () => {
    const trimmed = text.trim();
    if (!trimmed || send.isPending) return;
    setText('');
    setPending(trimmed);
    send.mutate(trimmed, { onSettled: () => setPending(null) });
  };
  const canSend = canChat && !!text.trim() && !send.isPending;

  return (
    <KeyboardAvoidingView
      behavior={process.env.EXPO_OS === 'ios' ? 'padding' : undefined}
      style={{ flex: 1 }}>
      <View className="flex-1 bg-km-ink" style={{ paddingTop: insets.top }}>
        <PresenceHeader />

        {canChat && messages.length > 0 ? (
          <View className="flex-row justify-end border-b border-km-line bg-km-ink px-3 py-1.5">
            <Pressable onPress={() => reset.mutate()} disabled={reset.isPending} hitSlop={8}>
              <Text className="font-mono text-[12px] text-km-muted">＋ new conversation</Text>
            </Pressable>
          </View>
        ) : null}

        {!canChat ? (
          <View className="flex-1">
            <ConfigNotice
              message={
                status === 'error'
                  ? "can't reach karmax — check the daemon or your tailnet › config"
                  : status === 'connecting'
                    ? 'linking to karmax…'
                    : 'connected · add your access token in config to talk › config'
              }
            />
          </View>
        ) : (
          <FlatList
            ref={listRef}
            data={rows}
            keyExtractor={(r) => r.id}
            onContentSizeChange={() => listRef.current?.scrollToEnd({ animated: false })}
            renderItem={({ item }) => {
              if (item.thinking) {
                return (
                  <LogLine role="krmx" time={item.time} content="working " trailing={<Caret color={KM.amber} />} />
                );
              }
              if (item.role === 'krmx') {
                return <KrmxMarkdown content={item.content} time={item.time} />;
              }
              return <LogLine role={item.role} time={item.time} content={item.content} />;
            }}
            contentContainerStyle={{ padding: 16, gap: 6, flexGrow: 1, justifyContent: 'flex-end' }}
            keyboardDismissMode="interactive"
            ListEmptyComponent={
              isLoading ? (
                <ActivityIndicator style={{ marginTop: 40 }} color={KM.muted} />
              ) : (
                <View className="gap-3 pt-2">
                  <BootSequence />
                  <LogLine role="sys" content="assign a task, ask for advice, or just talk. karmax remembers." />
                </View>
              )
            }
          />
        )}

        {send.isError ? (
          <Text className="px-4 pb-1 font-mono text-xs text-km-red">
            {`! ${(send.error as Error)?.message ?? 'send failed'}`}
          </Text>
        ) : null}

        <View
          className="flex-row items-center gap-2 border-t border-km-line bg-km-ink px-3 pt-2"
          style={{ paddingBottom: insets.bottom + 8 }}>
          <Text className="font-mono-bold text-base text-km-amber">›</Text>
          <TextInput
            className="max-h-28 flex-1 font-mono text-[15px] text-km-text"
            placeholder={canChat ? 'type a command…' : 'connect in config first'}
            placeholderTextColor={KM.muted}
            value={text}
            onChangeText={setText}
            editable={canChat}
            multiline
          />
          <Pressable
            onPress={onSend}
            disabled={!canSend}
            className="h-9 w-10 items-center justify-center rounded-md"
            style={{ borderCurve: 'continuous', backgroundColor: canSend ? KM.amber : KM.panel }}>
            {send.isPending ? (
              <ActivityIndicator color={KM.ink} />
            ) : (
              <Text className="font-mono-bold text-base" style={{ color: canSend ? KM.ink : KM.muted }}>
                ↵
              </Text>
            )}
          </Pressable>
        </View>
      </View>
    </KeyboardAvoidingView>
  );
}
