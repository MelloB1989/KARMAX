import { Text, TextInput, View } from '@/tw';

import { KM } from './colors';

export function Field({
  label,
  value,
  onChangeText,
  placeholder,
  secure,
}: {
  label: string;
  value: string;
  onChangeText: (v: string) => void;
  placeholder?: string;
  secure?: boolean;
}) {
  return (
    <View className="gap-1.5">
      <Text className="font-mono text-xs uppercase text-km-muted">{label}</Text>
      <TextInput
        className="rounded-md border border-km-line bg-km-panel px-3 py-3 font-mono text-[13px] text-km-text"
        style={{ borderCurve: 'continuous' }}
        placeholder={placeholder}
        placeholderTextColor={KM.muted}
        value={value}
        onChangeText={onChangeText}
        autoCapitalize="none"
        autoCorrect={false}
        keyboardType={secure ? 'default' : 'url'}
        secureTextEntry={secure}
      />
    </View>
  );
}
