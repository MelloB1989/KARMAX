import * as SecureStore from 'expo-secure-store';
import { create } from 'zustand';

import { ping } from '@/lib/api';

const URL_KEY = 'karmax.baseUrl';
const TOKEN_KEY = 'karmax.token';

// Default candidates probed during auto-detect. Tailscale works on any network
// (home WiFi or cellular); the LAN IP and karmax.local cover same-WiFi use.
// Edit these to match your KARMAX daemon if your addresses change.
export const DEFAULT_CANDIDATES = [
  'http://localhost:9091', // simulator / same host
  'http://karmax.local:9091', // mDNS (same network)
  // Add your KARMAX host here (LAN or Tailscale IP), or just enter it once in Settings.
];

export type ConnStatus = 'unknown' | 'connecting' | 'connected' | 'error';

type ConnectionState = {
  baseUrl: string | null;
  token: string;
  agent: string | null;
  status: ConnStatus;
  knownAddresses: string[];
  hydrated: boolean;
  init: () => Promise<void>;
  detect: () => Promise<boolean>;
  setToken: (token: string) => Promise<void>;
  setBaseUrl: (url: string) => Promise<void>;
};

export const useConnection = create<ConnectionState>((set, get) => ({
  baseUrl: null,
  token: '',
  agent: null,
  status: 'unknown',
  knownAddresses: [],
  hydrated: false,

  // init loads the saved URL/token from secure storage, then auto-detects.
  init: async () => {
    const [savedUrl, savedToken] = await Promise.all([
      SecureStore.getItemAsync(URL_KEY),
      SecureStore.getItemAsync(TOKEN_KEY),
    ]);
    set({ baseUrl: savedUrl ?? null, token: savedToken ?? '', hydrated: true });
    await get().detect();
  },

  // detect probes the saved URL, previously-learned addresses, and the defaults
  // in parallel, then picks the first that answers /api/ping as KARMAX.
  detect: async () => {
    set({ status: 'connecting' });
    const { baseUrl, knownAddresses } = get();
    const candidates = Array.from(
      new Set([baseUrl, ...knownAddresses, ...DEFAULT_CANDIDATES].filter(Boolean) as string[]),
    );

    const probes = await Promise.all(
      candidates.map(async (url) => ({ url, result: await ping(url) })),
    );
    const hit = probes.find((p) => p.result);

    if (hit?.result) {
      // If the server runs without auth (ping.auth === false), seed a
      // placeholder token so the token-gated screens work without the user
      // typing one. The open server ignores it.
      const authRequired = hit.result.auth ?? true;
      const seedToken = !authRequired && !get().token;
      set({
        baseUrl: hit.url,
        status: 'connected',
        agent: hit.result.agent ?? null,
        knownAddresses: hit.result.addresses ?? get().knownAddresses,
        ...(seedToken ? { token: 'open' } : {}),
      });
      await SecureStore.setItemAsync(URL_KEY, hit.url);
      return true;
    }

    set({ status: 'error' });
    return false;
  },

  setToken: async (token) => {
    set({ token });
    await SecureStore.setItemAsync(TOKEN_KEY, token);
  },

  setBaseUrl: async (url) => {
    set({ baseUrl: url });
    await SecureStore.setItemAsync(URL_KEY, url);
    await get().detect();
  },
}));
