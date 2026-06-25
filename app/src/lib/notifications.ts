import Constants from 'expo-constants';
import * as Device from 'expo-device';
import * as Notifications from 'expo-notifications';
import { Platform } from 'react-native';

// How notifications behave while the app is foregrounded (SDK 53+ fields).
Notifications.setNotificationHandler({
  handleNotification: async () => ({
    shouldPlaySound: true,
    shouldSetBadge: false,
    shouldShowBanner: true,
    shouldShowList: true,
  }),
});

// registerForPushAsync requests permission and returns the Expo push token, or
// null when unavailable (simulator, denied permission, or running in Expo Go
// without a development build — remote push requires a dev build from SDK 53).
export async function registerForPushAsync(): Promise<string | null> {
  if (!Device.isDevice) return null;

  if (Platform.OS === 'android') {
    await Notifications.setNotificationChannelAsync('default', {
      name: 'default',
      importance: Notifications.AndroidImportance.MAX,
    });
  }

  const existing = await Notifications.getPermissionsAsync();
  let status = existing.status;
  if (status !== 'granted') {
    status = (await Notifications.requestPermissionsAsync()).status;
  }
  if (status !== 'granted') return null;

  const projectId = Constants.expoConfig?.extra?.eas?.projectId;
  try {
    const token = await Notifications.getExpoPushTokenAsync(
      projectId ? { projectId } : undefined,
    );
    return token.data;
  } catch {
    // No EAS project configured yet, or unsupported in Expo Go.
    return null;
  }
}

// addNotificationListeners wires received + tapped handlers and returns a
// cleanup function that removes both subscriptions.
export function addNotificationListeners(
  onReceive?: (notification: Notifications.Notification) => void,
  onRespond?: (response: Notifications.NotificationResponse) => void,
): () => void {
  const received = Notifications.addNotificationReceivedListener((n) => onReceive?.(n));
  const responded = Notifications.addNotificationResponseReceivedListener((r) => onRespond?.(r));
  return () => {
    received.remove();
    responded.remove();
  };
}
