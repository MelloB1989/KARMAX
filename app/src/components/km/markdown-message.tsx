import * as Clipboard from 'expo-clipboard';
import Markdown from 'react-native-markdown-display';

import { Pressable, Text, View } from '@/tw';

import { KM, MONO } from './colors';
import { formatLogTime } from './log-line';

// Markdown styled to the KARMAX terminal palette.
const mdStyles = {
  body: { color: KM.text, fontFamily: MONO, fontSize: 13, lineHeight: 20 },
  heading1: { color: KM.amber, fontFamily: 'SpaceMono_700Bold', fontSize: 17, marginTop: 6, marginBottom: 4 },
  heading2: { color: KM.amber, fontFamily: 'JetBrainsMono_700Bold', fontSize: 15, marginTop: 6, marginBottom: 4 },
  heading3: { color: KM.amber, fontFamily: 'JetBrainsMono_500Medium', fontSize: 14, marginTop: 4, marginBottom: 2 },
  strong: { fontFamily: 'JetBrainsMono_700Bold', color: KM.text },
  link: { color: KM.cyan, textDecorationLine: 'underline' as const },
  bullet_list_icon: { color: KM.amber },
  ordered_list_icon: { color: KM.amber },
  code_inline: { fontFamily: MONO, color: KM.cyan, backgroundColor: KM.panel, paddingHorizontal: 4, borderRadius: 4 },
  fence: { fontFamily: MONO, color: KM.text, backgroundColor: KM.panel, padding: 10, borderRadius: 8, borderWidth: 1, borderColor: KM.line },
  code_block: { fontFamily: MONO, color: KM.text, backgroundColor: KM.panel, padding: 10, borderRadius: 8, borderWidth: 1, borderColor: KM.line },
  blockquote: { backgroundColor: KM.panel, borderLeftColor: KM.amber, borderLeftWidth: 3, paddingHorizontal: 10, paddingVertical: 2 },
  hr: { backgroundColor: KM.line, height: 1 },
  table: { borderColor: KM.line, borderWidth: 1 },
  tr: { borderColor: KM.line },
  th: { padding: 6, color: KM.amber },
  td: { padding: 6 },
};

// KARMAX's replies render as markdown (the machine's voice). Long-press to copy.
export function KrmxMarkdown({ content, time }: { content?: string; time?: string }) {
  const body = content ?? '';
  return (
    <Pressable onLongPress={() => Clipboard.setStringAsync(body)}>
      <View className="gap-1">
        <Text className="font-mono text-[13px] leading-5">
          <Text className="text-km-muted">{`[${formatLogTime(time)}] `}</Text>
          <Text className="font-mono-medium text-km-amber">krmx ‹</Text>
        </Text>
        <View className="pl-1">
          <Markdown style={mdStyles}>{body}</Markdown>
        </View>
      </View>
    </Pressable>
  );
}
