import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef } from 'react';
import { Platform } from 'react-native';

import {
  completeDeviceAction,
  decideProposal,
  fetchActivity,
  fetchDeviceActions,
  fetchIntegrations,
  fetchMemoryEntries,
  fetchMemoryTree,
  fetchMessages,
  fetchProfile,
  fetchProposals,
  forgetMemoryEntry,
  registerPushToken,
  runJob,
  saveProfile,
  sendChat,
  resetConversation,
  fetchCleanupQuestion,
  submitCleanupAnswer,
  fetchMemoryGraph,
  rebuildMemoryGraph,
  syncContacts,
  fetchContactsCount,
} from '@/lib/api';
import { getAllContacts } from '@/lib/contacts';
import {
  createCalendarEvent,
  createReminder,
  type CalendarEventSpec,
  type ReminderSpec,
} from '@/lib/calendar';
import { registerForPushAsync } from '@/lib/notifications';
import { useConnection } from '@/stores/connection';

export function useMessages() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const status = useConnection((s) => s.status);

  return useQuery({
    queryKey: ['messages', baseUrl],
    queryFn: () => fetchMessages(baseUrl as string, token),
    enabled: status === 'connected' && !!baseUrl && !!token,
    refetchInterval: 15_000,
  });
}

export function useSendMessage() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (message: string) => sendChat(baseUrl as string, token, message),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['messages', baseUrl] }),
  });
}

export function useResetConversation() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: () => resetConversation(baseUrl as string, token),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['messages', baseUrl] }),
  });
}

// useContactsStatus reports how many contacts KARMAX has synced.
export function useContactsStatus() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const connected = useConnection((s) => s.status === 'connected');
  return useQuery({
    queryKey: ['contacts-status', baseUrl],
    queryFn: () => fetchContactsCount(baseUrl as string, token),
    enabled: connected && !!baseUrl && !!token,
  });
}

// useSyncContactsNow reads the phone directory and uploads it on demand.
export function useSyncContactsNow() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async () => {
      const contacts = await getAllContacts();
      return syncContacts(baseUrl as string, token, contacts);
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['contacts-status', baseUrl] }),
  });
}

// useContactSync uploads the phone's contact directory to KARMAX once per
// session so it can resolve WhatsApp numbers to saved names.
export function useContactSync() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const connected = useConnection((s) => s.status === 'connected');
  const done = useRef(false);
  useEffect(() => {
    if (!connected || !baseUrl || !token || done.current) return;
    done.current = true;
    (async () => {
      try {
        const contacts = await getAllContacts();
        if (contacts.length) await syncContacts(baseUrl, token, contacts);
      } catch {
        done.current = false; // allow a retry next time
      }
    })();
  }, [connected, baseUrl, token]);
}

export function useMemoryGraph() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const connected = useConnection((s) => s.status === 'connected');
  return useQuery({
    queryKey: ['memory-graph', baseUrl],
    queryFn: () => fetchMemoryGraph(baseUrl as string, token),
    enabled: connected && !!baseUrl && !!token,
    staleTime: 60_000,
  });
}

export function useRebuildGraph() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: () => rebuildMemoryGraph(baseUrl as string, token),
    onSuccess: (data) => queryClient.setQueryData(['memory-graph', baseUrl], data),
  });
}

export function useCleanupQuestion() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  return useMutation({ mutationFn: () => fetchCleanupQuestion(baseUrl as string, token) });
}

export function useCleanupAnswer() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (body: { memory_id: string; memory: string; question: string; answer: string }) =>
      submitCleanupAnswer(baseUrl as string, token, body),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['memory-entries', baseUrl] });
      queryClient.invalidateQueries({ queryKey: ['memory-tree', baseUrl] });
    },
  });
}

export function useDeviceActions(status = '') {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const connected = useConnection((s) => s.status === 'connected');
  return useQuery({
    queryKey: ['device-actions', status, baseUrl],
    queryFn: () => fetchDeviceActions(baseUrl as string, token, status),
    enabled: connected && !!baseUrl && !!token,
    refetchInterval: 15_000,
  });
}

// useProcessDeviceActions polls for pending on-device actions and performs them
// locally (EventKit calendar/reminders), reporting each result back to KARMAX.
export function useProcessDeviceActions() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const connected = useConnection((s) => s.status === 'connected');
  const busy = useRef(false);

  useEffect(() => {
    if (!connected || !baseUrl || !token) return;

    const tick = async () => {
      if (busy.current) return;
      busy.current = true;
      try {
        const pending = await fetchDeviceActions(baseUrl, token, 'pending');
        for (const a of pending) {
          try {
            let result = 'unsupported';
            if (a.kind === 'calendar_event') {
              result = `event:${await createCalendarEvent(a.payload as unknown as CalendarEventSpec)}`;
            } else if (a.kind === 'reminder') {
              result = `reminder:${await createReminder(a.payload as unknown as ReminderSpec)}`;
            }
            await completeDeviceAction(baseUrl, token, a.id, 'done', result);
          } catch (e) {
            await completeDeviceAction(baseUrl, token, a.id, 'failed', (e as Error)?.message ?? 'failed');
          }
        }
      } catch {
        // ignore; retry next tick
      } finally {
        busy.current = false;
      }
    };

    tick();
    const interval = setInterval(tick, 20_000);
    return () => clearInterval(interval);
  }, [connected, baseUrl, token]);
}

export function useIntegrations() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const connected = useConnection((s) => s.status === 'connected');
  return useQuery({
    queryKey: ['integrations', baseUrl],
    queryFn: () => fetchIntegrations(baseUrl as string, token),
    enabled: connected && !!baseUrl && !!token,
    refetchInterval: 30_000,
  });
}

export function useActivity() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const connected = useConnection((s) => s.status === 'connected');
  return useQuery({
    queryKey: ['activity', baseUrl],
    queryFn: () => fetchActivity(baseUrl as string, token),
    enabled: connected && !!baseUrl && !!token,
    refetchInterval: 8_000,
  });
}

export function useRunJob() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => runJob(baseUrl as string, token, id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['activity'] }),
  });
}

export function useMemoryEntries(q = '') {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const connected = useConnection((s) => s.status === 'connected');
  return useQuery({
    queryKey: ['memory-entries', q, baseUrl],
    queryFn: () => fetchMemoryEntries(baseUrl as string, token, q),
    enabled: connected && !!baseUrl && !!token,
  });
}

export function useMemoryTree() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const connected = useConnection((s) => s.status === 'connected');
  return useQuery({
    queryKey: ['memory-tree', baseUrl],
    queryFn: () => fetchMemoryTree(baseUrl as string, token),
    enabled: connected && !!baseUrl && !!token,
  });
}

export function useForgetEntry() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => forgetMemoryEntry(baseUrl as string, token, id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['memory-entries'] });
      queryClient.invalidateQueries({ queryKey: ['memory-tree'] });
    },
  });
}

export function useProfile() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const connected = useConnection((s) => s.status === 'connected');
  return useQuery({
    queryKey: ['profile', baseUrl],
    queryFn: () => fetchProfile(baseUrl as string, token),
    enabled: connected && !!baseUrl && !!token,
  });
}

export function useSaveProfile() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (content: string) => saveProfile(baseUrl as string, token, content),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['profile'] }),
  });
}

export function useProposals(status = 'pending') {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const connected = useConnection((s) => s.status === 'connected');

  return useQuery({
    queryKey: ['proposals', status, baseUrl],
    queryFn: () => fetchProposals(baseUrl as string, token, status),
    enabled: connected && !!baseUrl && !!token,
    refetchInterval: 12_000,
  });
}

export function useDecideProposal() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const queryClient = useQueryClient();

  return useMutation({
    mutationFn: (args: { id: string; decision: 'approve' | 'reject'; edit?: string; note?: string }) =>
      decideProposal(baseUrl as string, token, args.id, args.decision, args.edit, args.note),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['proposals'] });
      queryClient.invalidateQueries({ queryKey: ['messages'] });
    },
  });
}

// usePushRegistration obtains the Expo push token once connected and registers
// it with KARMAX so the agent can proactively push to this device. No-ops in
// Expo Go / on a simulator (token unavailable) and retries when inputs change.
export function usePushRegistration() {
  const baseUrl = useConnection((s) => s.baseUrl);
  const token = useConnection((s) => s.token);
  const status = useConnection((s) => s.status);
  const registered = useRef(false);

  useEffect(() => {
    if (registered.current) return;
    if (status !== 'connected' || !baseUrl || !token) return;

    registered.current = true;
    (async () => {
      const expoPushToken = await registerForPushAsync();
      if (!expoPushToken) {
        registered.current = false;
        return;
      }
      try {
        await registerPushToken(baseUrl, token, expoPushToken, Platform.OS);
      } catch {
        registered.current = false;
      }
    })();
  }, [status, baseUrl, token]);
}
