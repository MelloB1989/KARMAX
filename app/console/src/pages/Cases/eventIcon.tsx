import {
  ArrowRightLeft, Box, FolderPlus, GitPullRequest, MessageSquare, StickyNote, Ticket, Bell, type LucideIcon,
} from "lucide-react";

export function iconForEventKind(kind: string): LucideIcon {
  if (kind.startsWith("case.opened")) return FolderPlus;
  if (kind.startsWith("case.state")) return ArrowRightLeft;
  if (kind.startsWith("sandbox.")) return Box;
  if (kind.startsWith("pr.")) return GitPullRequest;
  if (kind.startsWith("slack.")) return MessageSquare;
  if (kind.startsWith("jira.")) return Ticket;
  if (kind === "note") return StickyNote;
  return Bell;
}

export function formatPayload(payload: string): { key: string; value: string }[] {
  try {
    const obj = JSON.parse(payload) as Record<string, unknown>;
    return Object.entries(obj).map(([key, value]) => ({ key, value: String(value) }));
  } catch {
    return payload ? [{ key: "", value: payload }] : [];
  }
}
