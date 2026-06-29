import * as SecureStore from 'expo-secure-store';
import * as Speech from 'expo-speech';
import { create } from 'zustand';

const AUTOSPEAK_KEY = 'karmax.autoSpeak';

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
    set({ speakingId: id });
    const clear = () => {
      if (get().speakingId === id) set({ speakingId: null });
    };
    Speech.speak(body, { rate: 1.0, pitch: 1.0, onDone: clear, onStopped: clear, onError: clear });
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
