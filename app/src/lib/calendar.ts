import * as Calendar from 'expo-calendar';
import { Platform } from 'react-native';

export type CalendarEventSpec = {
  title: string;
  start: string;
  end?: string | null;
  notes?: string | null;
  location?: string | null;
  alarm_minutes_before?: number | null;
};

export type ReminderSpec = {
  title: string;
  due?: string | null;
  notes?: string | null;
};

async function defaultEventCalendarId(): Promise<string | null> {
  if (Platform.OS === 'ios') {
    try {
      const def = await Calendar.getDefaultCalendarAsync();
      if (def?.id) return def.id;
    } catch {
      // fall through to scanning calendars
    }
  }
  const cals = await Calendar.getCalendarsAsync(Calendar.EntityTypes.EVENT);
  return cals.find((c) => c.allowsModifications)?.id ?? cals[0]?.id ?? null;
}

export async function createCalendarEvent(spec: CalendarEventSpec): Promise<string> {
  const { status } = await Calendar.requestCalendarPermissions();
  if (status !== 'granted') throw new Error('calendar permission denied');

  const calId = await defaultEventCalendarId();
  if (!calId) throw new Error('no writable calendar');

  const start = new Date(spec.start);
  if (Number.isNaN(start.getTime())) throw new Error('invalid start date');
  const end = spec.end ? new Date(spec.end) : new Date(start.getTime() + 60 * 60 * 1000);
  const alarms =
    spec.alarm_minutes_before != null ? [{ relativeOffset: -Math.abs(spec.alarm_minutes_before) }] : undefined;

  return Calendar.createEventAsync(calId, {
    title: spec.title,
    startDate: start,
    endDate: end,
    notes: spec.notes ?? undefined,
    location: spec.location ?? undefined,
    alarms,
  });
}

export async function createReminder(spec: ReminderSpec): Promise<string> {
  if (Platform.OS !== 'ios') throw new Error('reminders are iOS-only');

  const { status } = await Calendar.requestRemindersPermissions();
  if (status !== 'granted') throw new Error('reminders permission denied');

  const cals = await Calendar.getCalendarsAsync(Calendar.EntityTypes.REMINDER);
  const list = cals.find((c) => c.allowsModifications) ?? cals[0];
  if (!list) throw new Error('no reminders list');

  const due = spec.due ? new Date(spec.due) : undefined;
  return Calendar.createReminderAsync(list.id, {
    title: spec.title,
    notes: spec.notes ?? undefined,
    dueDate: due,
    alarms: due ? [{ absoluteDate: due.toISOString() }] : undefined,
  });
}
