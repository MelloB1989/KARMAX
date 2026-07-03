import {
  AVAudioSessionCategory,
  AVAudioSessionMode,
  ExpoSpeechRecognitionModule,
} from 'expo-speech-recognition';
import * as SecureStore from 'expo-secure-store';
import * as Speech from 'expo-speech';
import { Platform } from 'react-native';
import { create } from 'zustand';

const AUTOSPEAK_KEY = 'karmax.autoSpeak';

// primeAudioForPlayback fixes quiet / inaudible TTS on iOS. Two things make
// expo-speech play too softly: (1) the default audio session routes through the
// quiet earpiece receiver and obeys the mute switch, and (2) after dictation,
// expo-speech-recognition leaves the shared session in a `record` category.
// Switching to the `playback` category routes TTS to the loud bottom speaker
// and plays even when the ringer is on silent. No-op off iOS (and harmless if
// the native module isn't in the current build).
function primeAudioForPlayback(): void {
  if (Platform.OS !== 'ios') return;
  try {
    ExpoSpeechRecognitionModule.setCategoryIOS({
      category: AVAudioSessionCategory.playback,
      categoryOptions: [],
      mode: AVAudioSessionMode.spokenAudio,
    });
  } catch {
    // native module unavailable (web / older build) — ignore
  }
}

// speakable strips markdown so the TTS engine reads clean prose instead of
// symbols ("#", "*", backticks, link syntax, code fences).
export function speakable(md: string): string {
  return (md || '')
    .replace(/```[\s\S]*?```/g, ' . code block . ')
    .replace(/`([^`]+)`/g, '$1')
    .replace(/!\[[^\]]*\]\([^)]*\)/g, '')
    .replace(/\[([^\]]+)\]\([^)]*\)/g, '$1')
    .replace(/^\s*#{1,6}\s*/gm, '')
    .replace(/^\s*[-*•]\s+/gm, ', ')
    .replace(/[*_~>|#]/g, ' ')
    .replace(/\s+/g, ' ')
    .trim();
}

type SpeechState = {
  speakingId: string | null;
  autoSpeak: boolean;
  init: () => Promise<void>;
  speak: (id: string, text: string) => void;
  stop: () => void;
  toggle: (id: string, text: string) => void;
  setAutoSpeak: (v: boolean) => Promise<void>;
};

// useSpeech is a tiny global store: only one utterance plays at a time, so we
// track which message id is currently being spoken (for the UI toggle).
export const useSpeech = create<SpeechState>((set, get) => ({
  speakingId: null,
  autoSpeak: false,

  init: async () => {
    try {
      const v = await SecureStore.getItemAsync(AUTOSPEAK_KEY);
      set({ autoSpeak: v === '1' });
    } catch {
      // ignore — default off
    }
  },

  speak: (id, text) => {
    const body = speakable(text);
    if (!body) return;
    Speech.stop();
    // Route to the loud speaker at full volume before speaking (see above).
    primeAudioForPlayback();
    set({ speakingId: id });
    const clear = () => {
      if (get().speakingId === id) set({ speakingId: null });
    };
    Speech.speak(body, {
      rate: 1.0,
      pitch: 1.0,
      volume: 1.0,
      onDone: clear,
      onStopped: clear,
      onError: clear,
    });
  },

  stop: () => {
    Speech.stop();
    set({ speakingId: null });
  },

  toggle: (id, text) => {
    if (get().speakingId === id) get().stop();
    else get().speak(id, text);
  },

  setAutoSpeak: async (v) => {
    set({ autoSpeak: v });
    try {
      await SecureStore.setItemAsync(AUTOSPEAK_KEY, v ? '1' : '0');
    } catch {
      // ignore
    }
  },
}));
