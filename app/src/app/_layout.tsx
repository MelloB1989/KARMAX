import '@/global.css';

import {
  JetBrainsMono_400Regular,
  JetBrainsMono_500Medium,
  JetBrainsMono_700Bold,
} from '@expo-google-fonts/jetbrains-mono';
import { SpaceMono_700Bold } from '@expo-google-fonts/space-mono';
import { useQueryClient } from '@tanstack/react-query';
import { DarkTheme, ThemeProvider, useRouter } from 'expo-router';
import { useFonts } from 'expo-font';
import * as SplashScreen from 'expo-splash-screen';
import { StatusBar } from 'expo-status-bar';
import { useEffect } from 'react';
import { AppState } from 'react-native';

import AppTabs from '@/components/app-tabs';
import { useContactSync, useProcessDeviceActions, usePushRegistration } from '@/lib/hooks';
import { addNotificationListeners } from '@/lib/notifications';
import { QueryProvider } from '@/lib/query-provider';
import { useConnection } from '@/stores/connection';
import { registerBackgroundTasks } from '@/tasks';

SplashScreen.preventAutoHideAsync();

// AppBootstrap runs inside the QueryProvider so it can refresh queries and
// navigate in response to connection changes, notifications, and foreground.
function AppBootstrap() {
  const init = useConnection((s) => s.init);
  const queryClient = useQueryClient();
  const router = useRouter();

  usePushRegistration();
  useProcessDeviceActions();
  useContactSync();

  useEffect(() => {
    init();
  }, [init]);

  // Register background execution (periodic BGTaskScheduler sync + push-triggered
  // background sync) so KARMAX's queued on-device actions run while backgrounded.
  useEffect(() => {
    void registerBackgroundTasks();
  }, []);

  // Refresh on incoming push; deep-link to the right surface when tapped
  // (approvals → inbox, otherwise → chat).
  useEffect(() => {
    return addNotificationListeners(
      () => {
        queryClient.invalidateQueries({ queryKey: ['messages'] });
        queryClient.invalidateQueries({ queryKey: ['proposals'] });
        queryClient.invalidateQueries({ queryKey: ['notifications'] });
      },
      (response) => {
        const data = response.notification.request.content.data as { type?: string } | undefined;
        if (data?.type === 'proposal') router.navigate('/inbox');
        else if (data?.type === 'notification') router.navigate('/inbox?tab=notifications');
        else router.navigate('/');
      },
    );
  }, [queryClient, router]);

  // Re-detect KARMAX and refresh on app foreground (WiFi <-> cellular, restarts).
  useEffect(() => {
    const sub = AppState.addEventListener('change', (state) => {
      if (state === 'active') {
        void useConnection.getState().detect();
        queryClient.invalidateQueries({ queryKey: ['messages'] });
      }
    });
    return () => sub.remove();
  }, [queryClient]);

  return null;
}

export default function RootLayout() {
  const [fontsLoaded] = useFonts({
    JetBrainsMono_400Regular,
    JetBrainsMono_500Medium,
    JetBrainsMono_700Bold,
    SpaceMono_700Bold,
  });

  useEffect(() => {
    if (fontsLoaded) SplashScreen.hideAsync();
  }, [fontsLoaded]);

  if (!fontsLoaded) return null;

  return (
    <QueryProvider>
      <AppBootstrap />
      <ThemeProvider value={DarkTheme}>
        <StatusBar style="light" />
        <AppTabs />
      </ThemeProvider>
    </QueryProvider>
  );
}
