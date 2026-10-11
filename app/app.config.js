// Whose app this is — the store bundle id, the Expo account, the EAS project —
// is deployment config, not source. Set them in app/.env.local (git-ignored)
// for local builds and as EAS environment variables for EAS builds:
//   KARMAX_APP_ID=com.yourdomain.karmax
//   KARMAX_EXPO_OWNER=<expo account>
//   KARMAX_EAS_PROJECT_ID=<uuid from `eas init`>
module.exports = ({ config }) => {
  const appId = process.env.KARMAX_APP_ID;
  const owner = process.env.KARMAX_EXPO_OWNER;
  const projectId = process.env.KARMAX_EAS_PROJECT_ID;
  return {
    ...config,
    ...(owner && { owner }),
    ios: { ...config.ios, ...(appId && { bundleIdentifier: appId }) },
    android: { ...config.android, ...(appId && { package: appId }) },
    extra: { ...config.extra, ...(projectId && { eas: { projectId } }) },
    ...(projectId && { updates: { ...config.updates, url: `https://u.expo.dev/${projectId}` } }),
  };
};
