import { useState } from 'react';
import { useSafeAreaInsets } from 'react-native-safe-area-context';

import { KM } from '@/components/km/colors';
import { Field } from '@/components/km/field';
import { useIntegrations } from '@/lib/hooks';
import { Pressable, ScrollView, Text, View } from '@/tw';
import { useConnection, type ConnStatus } from '@/stores/connection';

function statusLine(status: ConnStatus): { label: string; color: string } {
  switch (status) {
    case 'connected':
      return { label: 'online', color: KM.green };
    case 'connecting':
      return { label: 'connecting…', color: KM.amber };
    case 'error':
      return { label: 'offline', color: KM.red };
    default:
      return { label: '—', color: KM.muted };
  }
}

function intColor(status: string): string {
  switch (status) {
    case 'connected':
    case 'available':
    case 'registered':
    case 'online':
    case 'configured':
      return KM.green;
    case 'disconnected':
      return KM.amber;
    case 'missing':
    case 'offline':
      return KM.red;
    default:
      return KM.muted;
  }
}

export default function ConfigScreen() {
  const insets = useSafeAreaInsets();
  const { baseUrl, token, status, agent, knownAddresses, setBaseUrl, setToken, detect } =
    useConnection();
  const [urlInput, setUrlInput] = useState(baseUrl ?? '');
  const [tokenInput, setTokenInput] = useState(token);
  const [busy, setBusy] = useState(false);
  const s = statusLine(status);
  const integrations = useIntegrations();

  const onSave = async () => {
    setBusy(true);
    try {
      await setToken(tokenInput.trim());
      if (urlInput.trim()) await setBaseUrl(urlInput.trim());
      else await detect();
    } finally {
      setBusy(false);
    }
  };

  return (
    <ScrollView
      className="flex-1 bg-km-ink"
      contentContainerStyle={{ padding: 20, gap: 24, paddingBottom: insets.bottom + 24 }}
      contentInsetAdjustmentBehavior="automatic">
      <View className="gap-1">
        <Text className="font-display text-2xl text-km-amber">config</Text>
        <Text className="font-mono text-xs text-km-muted">
          link to the karmax daemon over wifi or tailscale.
        </Text>
      </View>

      <View className="gap-2">
        <Text className="font-mono text-xs uppercase text-km-muted">status</Text>
        <View
          className="gap-1.5 rounded-md border border-km-line bg-km-panel p-4"
          style={{ borderCurve: 'continuous' }}>
          <View className="flex-row items-center gap-2">
            <View style={{ width: 7, height: 7, borderRadius: 4, backgroundColor: s.color }} />
            <Text className="font-mono-medium text-[13px] text-km-text">{s.label}</Text>
            {agent ? <Text className="font-mono text-xs text-km-muted">{`· ${agent}`}</Text> : null}
          </View>
          {baseUrl ? (
            <Text selectable className="font-mono text-xs text-km-muted">
              {baseUrl}
            </Text>
          ) : null}
        </View>
      </View>

      <View className="gap-3">
        <Field
          label="endpoint"
          value={urlInput}
          onChangeText={setUrlInput}
          placeholder="http://<karmax-host>:9091 (blank = auto)"
        />
        <Field
          label="access token"
          value={tokenInput}
          onChangeText={setTokenInput}
          placeholder="KARMAX_API_TOKEN"
          secure
        />
        <Pressable
          onPress={onSave}
          disabled={busy}
          className="items-center rounded-md py-3"
          style={{ borderCurve: 'continuous', backgroundColor: KM.amber }}>
          <Text className="font-mono-bold text-[13px]" style={{ color: KM.ink }}>
            {busy ? 'linking…' : 'save & connect'}
          </Text>
        </Pressable>
        <Pressable
          onPress={() => detect()}
          className="items-center rounded-md border border-km-line py-3"
          style={{ borderCurve: 'continuous' }}>
          <Text className="font-mono-medium text-[13px] text-km-text">scan</Text>
        </Pressable>
      </View>

      <View className="gap-2">
        <Text className="font-mono text-xs uppercase text-km-muted">integrations</Text>
        <View
          className="gap-2.5 rounded-md border border-km-line bg-km-panel p-3"
          style={{ borderCurve: 'continuous' }}>
          {(integrations.data ?? []).length === 0 ? (
            <Text className="font-mono text-xs text-km-muted">
              {integrations.isLoading ? 'checking…' : 'unavailable'}
            </Text>
          ) : (
            (integrations.data ?? []).map((it) => (
              <View key={it.id} className="flex-row items-center gap-2">
                <View style={{ width: 7, height: 7, borderRadius: 4, backgroundColor: intColor(it.status) }} />
                <Text className="font-mono-medium text-[12px] text-km-text">{it.name}</Text>
                <Text className="flex-1 text-right font-mono text-[11px] text-km-muted" numberOfLines={1}>
                  {it.detail || it.status}
                </Text>
              </View>
            ))
          )}
        </View>
      </View>

      {knownAddresses.length > 0 ? (
        <View className="gap-2">
          <Text className="font-mono text-xs uppercase text-km-muted">known addresses</Text>
          {knownAddresses.map((addr) => (
            <Pressable
              key={addr}
              onPress={() => {
                setUrlInput(addr);
                setBaseUrl(addr);
              }}
              className="rounded-md border border-km-line bg-km-panel px-3 py-3"
              style={{ borderCurve: 'continuous' }}>
              <Text selectable className="font-mono text-xs text-km-text">
                {addr}
              </Text>
            </Pressable>
          ))}
        </View>
      ) : null}
    </ScrollView>
  );
}
