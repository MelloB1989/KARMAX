import { completeDeviceAction, fetchDeviceActions } from './api';
import { createCalendarEvent, createReminder, type CalendarEventSpec, type ReminderSpec } from './calendar';

// syncDeviceActions pulls KARMAX's pending on-device actions and performs them
// locally (EventKit calendar/reminders), reporting each result back. Plain async
// (no hooks) so it can run from a foreground poll AND from background tasks.
export async function syncDeviceActions(baseUrl: string, token: string): Promise<number> {
  const pending = await fetchDeviceActions(baseUrl, token, 'pending');
  let done = 0;
  for (const a of pending) {
    try {
      let result = 'unsupported';
      if (a.kind === 'calendar_event') {
        result = `event:${await createCalendarEvent(a.payload as unknown as CalendarEventSpec)}`;
      } else if (a.kind === 'reminder') {
        result = `reminder:${await createReminder(a.payload as unknown as ReminderSpec)}`;
      }
      await completeDeviceAction(baseUrl, token, a.id, 'done', result);
      done++;
    } catch (e) {
      await completeDeviceAction(baseUrl, token, a.id, 'failed', (e as Error)?.message ?? 'failed');
    }
  }
  return done;
}
