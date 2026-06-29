import { ExpoSpeechRecognitionModule, useSpeechRecognitionEvent } from 'expo-speech-recognition';
import { useCallback, useState } from 'react';

import { useSpeech } from '@/lib/speech';

// useDictation wires expo-speech-recognition into a simple toggle: tap to start
// listening, transcripts stream to onText (interim updates + a final result),
// tap again (or natural end of speech) to stop. STT is the voice-input
// counterpart to expo-speech TTS.
export function useDictation(onText: (transcript: string, isFinal: boolean) => void) {
  const [listening, setListening] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useSpeechRecognitionEvent('start', () => setListening(true));
  useSpeechRecognitionEvent('end', () => setListening(false));
  useSpeechRecognitionEvent('result', (e) => {
    const transcript = e.results?.[0]?.transcript ?? '';
    if (transcript) onText(transcript, e.isFinal);
  });
  useSpeechRecognitionEvent('error', (e) => {
    setError(e.message || String(e.error));
    setListening(false);
  });

  const start = useCallback(async () => {
    setError(null);
    // Don't let TTS and the mic talk over each other.
    useSpeech.getState().stop();
    try {
      const perm = await ExpoSpeechRecognitionModule.requestPermissionsAsync();
      if (!perm.granted) {
        setError('microphone / speech permission denied');
        return;
      }
      ExpoSpeechRecognitionModule.start({ lang: 'en-US', interimResults: true, continuous: false });
    } catch (err) {
      setError((err as Error)?.message ?? 'could not start dictation');
      setListening(false);
    }
  }, []);

  const stop = useCallback(() => {
    try {
      ExpoSpeechRecognitionModule.stop();
    } catch {
      // ignore
    }
  }, []);

  const toggle = useCallback(() => {
    if (listening) stop();
    else void start();
  }, [listening, start, stop]);

  return { listening, error, toggle };
}
