// Background execution for KARMAX. Per Expo Router guidance, task definitions
// live in their own module and are imported at the top of the root layout so
// they register before navigation runs (the OS may launch the app straight into
// the background to run these — no views are mounted).
import * as BackgroundTask from 'expo-background-task';
import * as Notifications from 'expo-notifications';
import * as TaskManager from 'expo-task-manager';

import { syncDeviceActions } from '@/lib/device-sync';
import { useConnection } from '@/stores/connection';

export const BG_SYNC_TASK = 'karmax-bg-sync';
export const BG_NOTIFICATION_TASK = 'karmax-bg-notification';

// runSync locates the daemon (saved/known addresses) and applies any pending
// on-device actions KARMAX queued (calendar, reminders). Used by both the
// periodic task and the push-triggered task.
async function runSync(): Promise<boolean> {
  let { baseUrl, token } = useConnection.getState();
  if (!baseUrl || !token) {
    await useConnection.getState().detect();
    ({ baseUrl, token } = useConnection.getState());
  }
  if (!baseUrl || !token) return false;
  try {
    await syncDeviceActions(baseUrl, token);
    return true;
  } catch {
    return false;
  }
}

// Periodic background task (BGTaskScheduler) — iOS runs it opportunistically,
// weighing battery, network, and usage. Not continuous.
TaskManager.defineTask(BG_SYNC_TASK, async () => {
  const ok = await runSync();
  return ok ? BackgroundTask.BackgroundTaskResult.Success : BackgroundTask.BackgroundTaskResult.Failed;
});

// Background-notification task — the daemon can wake the app at any time with a
// (silent, content-available) data push to force an immediate sync. This is the
// "aggressive" lever: the server decides when the phone needs to act.
TaskManager.defineTask(BG_NOTIFICATION_TASK, async () => {
  await runSync();
});

// registerBackgroundTasks wires both tasks. Safe to call repeatedly; no-ops on
// unsupported environments (e.g. the iOS simulator).
export async function registerBackgroundTasks(): Promise<void> {
  try {
    await BackgroundTask.registerTaskAsync(BG_SYNC_TASK, { minimumInterval: 15 });
  } catch {
    // already registered or unsupported — ignore
  }
  try {
    await Notifications.registerTaskAsync(BG_NOTIFICATION_TASK);
  } catch {
    // ignore
  }
}
