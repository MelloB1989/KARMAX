import type { ReactNode } from 'react';

import { Text } from '@/tw';

export type LogRole = 'you' | 'krmx' | 'sys';

export function formatLogTime(iso?: string): string {
  if (!iso) return '--:--';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '--:--';
  return d.toTimeString().slice(0, 5);
}

const PREFIX: Record<LogRole, string> = { you: 'you ›', krmx: 'krmx ‹', sys: '···' };
const PREFIX_COLOR: Record<LogRole, string> = {
  you: 'text-km-cyan',
  krmx: 'text-km-amber',
  sys: 'text-km-muted',
};

// A single line of the conversation log: [hh:mm] <role> content
export function LogLine({
  role,
  content,
  time,
  trailing,
}: {
  role: LogRole;
  content?: string;
  time?: string;
  trailing?: ReactNode;
}) {
  return (
    <Text selectable className="font-mono text-[13px] leading-5 text-km-text">
      <Text className="text-km-muted">{`[${formatLogTime(time)}] `}</Text>
      <Text className={`font-mono-medium ${PREFIX_COLOR[role]}`}>{`${PREFIX[role]} `}</Text>
      {content}
      {trailing}
    </Text>
  );
}
