import AsyncStorage from '@react-native-async-storage/async-storage';
import { StatusBar } from 'expo-status-bar';
import {
  ActivityIndicator,
  Alert,
  Image,
  KeyboardAvoidingView,
  Linking,
  Modal,
  Platform,
  Pressable,
  SafeAreaView,
  Share,
  StyleSheet,
  Text,
  TextInput,
  View,
  type DimensionValue,
  type ImageStyle,
} from 'react-native';
import {
  ArrowLeft,
  ExternalLink,
  Home,
  RefreshCw,
  Settings,
  Share2,
  WifiOff,
  X,
} from 'lucide-react-native';
import { useCallback, useEffect, useMemo, useRef, useState, type ComponentType } from 'react';
import { WebView } from 'react-native-webview';
import {
  defaultCoordinatorURL,
  normalizeCoordinatorURL,
  webViewOriginWhitelist,
} from './src/coordinatorURL';

const coordinatorStorageKey = 'crabbox.mobile.coordinator-url';
const appIcon = require('./assets/icon.png');

type IconComponent = ComponentType<{
  color?: string;
  size?: number;
  strokeWidth?: number;
}>;

type IconButtonProps = {
  icon: IconComponent;
  label: string;
  disabled?: boolean;
  onPress: () => void;
};

export default function App() {
  const webView = useRef<WebView>(null);
  const [booting, setBooting] = useState(true);
  const [homeURL, setHomeURL] = useState(defaultCoordinatorURL);
  const [draftURL, setDraftURL] = useState(defaultCoordinatorURL);
  const [currentURL, setCurrentURL] = useState(defaultCoordinatorURL);
  const [pageTitle, setPageTitle] = useState('Crabbox');
  const [canGoBack, setCanGoBack] = useState(false);
  const [isLoading, setIsLoading] = useState(true);
  const [loadFailed, setLoadFailed] = useState(false);
  const [progress, setProgress] = useState(0);
  const [settingsVisible, setSettingsVisible] = useState(false);
  const [urlError, setURLError] = useState('');
  const [webViewKey, setWebViewKey] = useState(0);

  useEffect(() => {
    let mounted = true;

    AsyncStorage.getItem(coordinatorStorageKey)
      .then((storedURL) => {
        if (!mounted) {
          return;
        }

        const normalized = normalizeCoordinatorURL(storedURL ?? '', { allowLocalHTTP: __DEV__ });
        const nextURL = normalized ?? defaultCoordinatorURL;
        setHomeURL(nextURL);
        setDraftURL(nextURL);
        setCurrentURL(nextURL);
      })
      .catch(() => {
        if (mounted) {
          setHomeURL(defaultCoordinatorURL);
          setDraftURL(defaultCoordinatorURL);
          setCurrentURL(defaultCoordinatorURL);
        }
      })
      .finally(() => {
        if (mounted) {
          setBooting(false);
        }
      });

    return () => {
      mounted = false;
    };
  }, []);

  const currentHost = useMemo(() => hostLabel(currentURL || homeURL), [currentURL, homeURL]);
  const loadingWidth = `${Math.max(6, Math.round(progress * 100))}%` as DimensionValue;
  const originWhitelist = useMemo(() => webViewOriginWhitelist(homeURL), [homeURL]);

  const reload = useCallback(() => {
    setLoadFailed(false);
    setIsLoading(true);
    webView.current?.reload();
  }, []);

  const goHome = useCallback(() => {
    setLoadFailed(false);
    setCurrentURL(homeURL);
    setWebViewKey((value) => value + 1);
  }, [homeURL]);

  const shareCurrentURL = useCallback(async () => {
    try {
      await Share.share({
        title: pageTitle || 'Crabbox',
        message: currentURL,
        url: currentURL,
      });
    } catch (error) {
      Alert.alert('Share unavailable', readableError(error));
    }
  }, [currentURL, pageTitle]);

  const openCurrentURL = useCallback(() => {
    Linking.openURL(currentURL).catch((error) => {
      Alert.alert('Could not open link', readableError(error));
    });
  }, [currentURL]);

  const saveCoordinatorURL = useCallback(async () => {
    const normalized = normalizeCoordinatorURL(draftURL, { allowLocalHTTP: __DEV__ });
    if (!normalized) {
      setURLError('Enter an HTTPS URL. HTTP is available only for localhost in development builds.');
      return;
    }

    setURLError('');
    setHomeURL(normalized);
    setCurrentURL(normalized);
    setLoadFailed(false);
    setSettingsVisible(false);
    setWebViewKey((value) => value + 1);
    await AsyncStorage.setItem(coordinatorStorageKey, normalized);
  }, [draftURL]);

  const resetCoordinatorURL = useCallback(async () => {
    setDraftURL(defaultCoordinatorURL);
    setURLError('');
    setHomeURL(defaultCoordinatorURL);
    setCurrentURL(defaultCoordinatorURL);
    setLoadFailed(false);
    setSettingsVisible(false);
    setWebViewKey((value) => value + 1);
    await AsyncStorage.setItem(coordinatorStorageKey, defaultCoordinatorURL);
  }, []);

  if (booting) {
    return (
      <View style={styles.bootScreen}>
        <StatusBar style="light" />
        <Image source={appIcon} style={styles.bootIcon as ImageStyle} />
        <ActivityIndicator color={colors.accent} />
      </View>
    );
  }

  return (
    <View style={styles.app}>
      <StatusBar style="light" />
      <SafeAreaView style={styles.safeArea}>
        <View style={styles.header}>
          <View style={styles.brandRow}>
            <Image source={appIcon} style={styles.brandIcon as ImageStyle} />
            <View style={styles.brandText}>
              <Text numberOfLines={1} style={styles.title}>
                Crabbox
              </Text>
              <Text numberOfLines={1} style={styles.host}>
                {currentHost}
              </Text>
            </View>
          </View>
          <View style={styles.headerActions}>
            <View style={[styles.statusPill, loadFailed && styles.statusPillError]}>
              <View style={[styles.statusDot, loadFailed && styles.statusDotError]} />
              <Text style={[styles.statusText, loadFailed && styles.statusTextError]}>
                {loadFailed ? 'Offline' : isLoading ? 'Loading' : 'Live'}
              </Text>
            </View>
            <IconButton
              icon={Settings}
              label="Open coordinator settings"
              onPress={() => {
                setDraftURL(homeURL);
                setURLError('');
                setSettingsVisible(true);
              }}
            />
          </View>
        </View>

        {isLoading && !loadFailed ? (
          <View style={styles.progressTrack}>
            <View style={[styles.progressBar, { width: loadingWidth }]} />
          </View>
        ) : (
          <View style={styles.progressTrack} />
        )}

        <View style={styles.browser}>
          <WebView
            key={webViewKey}
            ref={webView}
            source={{ uri: homeURL }}
            style={styles.webView}
            originWhitelist={originWhitelist}
            sharedCookiesEnabled
            thirdPartyCookiesEnabled
            setSupportMultipleWindows={false}
            allowsBackForwardNavigationGestures
            pullToRefreshEnabled
            decelerationRate="normal"
            onShouldStartLoadWithRequest={(request) => {
              if (shouldOpenExternally(request.url)) {
                Linking.openURL(request.url).catch(() => undefined);
                return false;
              }

              return true;
            }}
            onLoadStart={() => {
              setProgress(0);
              setIsLoading(true);
              setLoadFailed(false);
            }}
            onLoadEnd={() => setIsLoading(false)}
            onLoadProgress={(event) => setProgress(event.nativeEvent.progress)}
            onError={() => {
              setLoadFailed(true);
              setIsLoading(false);
            }}
            onNavigationStateChange={(state) => {
              setCanGoBack(state.canGoBack);
              setCurrentURL(state.url || homeURL);
              setPageTitle(state.title || 'Crabbox');
            }}
          />

          {loadFailed && (
            <View style={styles.errorPanel}>
              <WifiOff color={colors.warning} size={34} strokeWidth={2.4} />
              <Text style={styles.errorTitle}>Could not reach Crabbox</Text>
              <Text style={styles.errorCopy}>{currentHost}</Text>
              <View style={styles.errorActions}>
                <Pressable
                  accessibilityRole="button"
                  onPress={reload}
                  style={({ pressed }) => [styles.ctaButton, pressed && styles.ctaButtonPressed]}
                >
                  <RefreshCw color={colors.ctaInk} size={18} strokeWidth={2.5} />
                  <Text style={styles.ctaText}>Retry</Text>
                </Pressable>
                <Pressable
                  accessibilityRole="button"
                  onPress={openCurrentURL}
                  style={({ pressed }) => [
                    styles.secondaryCTAButton,
                    pressed && styles.secondaryCTAButtonPressed,
                  ]}
                >
                  <ExternalLink color={colors.text} size={18} strokeWidth={2.4} />
                  <Text style={styles.secondaryCTAText}>Open</Text>
                </Pressable>
              </View>
            </View>
          )}
        </View>

        <View style={styles.navBar}>
          <IconButton
            disabled={!canGoBack}
            icon={ArrowLeft}
            label="Go back"
            onPress={() => webView.current?.goBack()}
          />
          <IconButton icon={Home} label="Go to Crabbox home" onPress={goHome} />
          <IconButton icon={RefreshCw} label="Reload" onPress={reload} />
          <IconButton icon={Share2} label="Share current page" onPress={shareCurrentURL} />
          <IconButton icon={ExternalLink} label="Open current page in browser" onPress={openCurrentURL} />
        </View>
      </SafeAreaView>

      <Modal
        animationType="slide"
        transparent
        visible={settingsVisible}
        onRequestClose={() => setSettingsVisible(false)}
      >
        <KeyboardAvoidingView
          behavior={Platform.OS === 'ios' ? 'padding' : undefined}
          style={styles.modalRoot}
        >
          <Pressable style={styles.modalBackdrop} onPress={() => setSettingsVisible(false)} />
          <View style={styles.settingsSheet}>
            <View style={styles.sheetHeader}>
              <Text style={styles.sheetTitle}>Coordinator</Text>
              <IconButton icon={X} label="Close settings" onPress={() => setSettingsVisible(false)} />
            </View>
            <Text style={styles.inputLabel}>URL</Text>
            <TextInput
              autoCapitalize="none"
              autoCorrect={false}
              keyboardType="url"
              onChangeText={(value) => {
                setDraftURL(value);
                setURLError('');
              }}
              onSubmitEditing={saveCoordinatorURL}
              placeholder="https://crabbox.sh"
              placeholderTextColor={colors.muted}
              returnKeyType="go"
              selectTextOnFocus
              style={[styles.urlInput, urlError ? styles.urlInputError : undefined]}
              value={draftURL}
            />
            {urlError ? <Text style={styles.inputError}>{urlError}</Text> : null}
            <View style={styles.sheetActions}>
              <Pressable
                accessibilityRole="button"
                onPress={resetCoordinatorURL}
                style={({ pressed }) => [
                  styles.sheetSecondaryButton,
                  pressed && styles.secondaryCTAButtonPressed,
                ]}
              >
                <Text style={styles.secondaryCTAText}>Use crabbox.sh</Text>
              </Pressable>
              <Pressable
                accessibilityRole="button"
                onPress={saveCoordinatorURL}
                style={({ pressed }) => [styles.sheetPrimaryButton, pressed && styles.ctaButtonPressed]}
              >
                <Text style={styles.ctaText}>Connect</Text>
              </Pressable>
            </View>
          </View>
        </KeyboardAvoidingView>
      </Modal>
    </View>
  );
}

function IconButton({ disabled = false, icon: Icon, label, onPress }: IconButtonProps) {
  return (
    <Pressable
      accessibilityLabel={label}
      accessibilityRole="button"
      disabled={disabled}
      onPress={onPress}
      style={({ pressed }) => [
        styles.iconButton,
        disabled && styles.iconButtonDisabled,
        pressed && !disabled && styles.iconButtonPressed,
      ]}
    >
      <Icon color={disabled ? colors.disabled : colors.text} size={20} strokeWidth={2.35} />
    </Pressable>
  );
}

function hostLabel(value: string): string {
  try {
    const url = new URL(value);
    return url.host;
  } catch {
    return 'crabbox.sh';
  }
}

function readableError(error: unknown): string {
  return error instanceof Error ? error.message : 'The request could not be completed.';
}

function shouldOpenExternally(url: string): boolean {
  return /^(mailto|tel|sms|itms-apps):/i.test(url);
}

const colors = {
  app: '#101010',
  panel: '#171717',
  panelAlt: '#202020',
  border: '#303030',
  text: '#f7f7f4',
  muted: '#9fa6ad',
  accent: '#31d0aa',
  accentAlt: '#27b6c3',
  warning: '#ff7966',
  ctaInk: '#07231d',
  disabled: '#6f767d',
};

const styles = StyleSheet.create({
  app: {
    flex: 1,
    backgroundColor: colors.app,
  },
  safeArea: {
    flex: 1,
    backgroundColor: colors.app,
  },
  bootScreen: {
    flex: 1,
    alignItems: 'center',
    justifyContent: 'center',
    gap: 18,
    backgroundColor: colors.app,
  },
  bootIcon: {
    width: 92,
    height: 92,
    borderRadius: 21,
  },
  header: {
    minHeight: 68,
    paddingHorizontal: 14,
    paddingVertical: 9,
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'space-between',
    gap: 12,
    borderBottomWidth: StyleSheet.hairlineWidth,
    borderBottomColor: colors.border,
    backgroundColor: colors.panel,
  },
  brandRow: {
    flex: 1,
    minWidth: 0,
    flexDirection: 'row',
    alignItems: 'center',
    gap: 10,
  },
  brandIcon: {
    width: 42,
    height: 42,
    borderRadius: 10,
  },
  brandText: {
    flex: 1,
    minWidth: 0,
  },
  title: {
    color: colors.text,
    fontSize: 20,
    fontWeight: '800',
  },
  host: {
    marginTop: 2,
    color: colors.muted,
    fontSize: 12,
    fontWeight: '600',
  },
  headerActions: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: 8,
  },
  statusPill: {
    minHeight: 32,
    paddingHorizontal: 10,
    borderRadius: 999,
    backgroundColor: '#122821',
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: '#23584b',
    flexDirection: 'row',
    alignItems: 'center',
    gap: 7,
  },
  statusPillError: {
    backgroundColor: '#301918',
    borderColor: '#6a3029',
  },
  statusDot: {
    width: 7,
    height: 7,
    borderRadius: 4,
    backgroundColor: colors.accent,
  },
  statusDotError: {
    backgroundColor: colors.warning,
  },
  statusText: {
    color: '#bff4e7',
    fontSize: 12,
    fontWeight: '700',
  },
  statusTextError: {
    color: '#ffc2b9',
  },
  progressTrack: {
    height: 2,
    backgroundColor: colors.app,
  },
  progressBar: {
    height: 2,
    backgroundColor: colors.accent,
  },
  browser: {
    flex: 1,
    backgroundColor: colors.app,
  },
  webView: {
    flex: 1,
    backgroundColor: colors.app,
  },
  navBar: {
    minHeight: 62,
    paddingHorizontal: 12,
    paddingTop: 8,
    paddingBottom: 10,
    borderTopWidth: StyleSheet.hairlineWidth,
    borderTopColor: colors.border,
    backgroundColor: colors.panel,
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'space-between',
    gap: 8,
  },
  iconButton: {
    width: 46,
    height: 42,
    borderRadius: 8,
    alignItems: 'center',
    justifyContent: 'center',
    backgroundColor: colors.panelAlt,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: colors.border,
  },
  iconButtonPressed: {
    backgroundColor: '#2a2a2a',
  },
  iconButtonDisabled: {
    opacity: 0.42,
  },
  errorPanel: {
    ...StyleSheet.absoluteFill,
    paddingHorizontal: 28,
    alignItems: 'center',
    justifyContent: 'center',
    backgroundColor: colors.app,
  },
  errorTitle: {
    marginTop: 14,
    color: colors.text,
    fontSize: 22,
    fontWeight: '800',
    textAlign: 'center',
  },
  errorCopy: {
    marginTop: 8,
    color: colors.muted,
    fontSize: 14,
    fontWeight: '600',
    textAlign: 'center',
  },
  errorActions: {
    marginTop: 24,
    flexDirection: 'row',
    alignItems: 'center',
    gap: 10,
  },
  ctaButton: {
    minHeight: 44,
    minWidth: 112,
    paddingHorizontal: 16,
    borderRadius: 8,
    backgroundColor: colors.accent,
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'center',
    gap: 8,
  },
  ctaButtonPressed: {
    backgroundColor: '#25b894',
  },
  ctaText: {
    color: colors.ctaInk,
    fontSize: 15,
    fontWeight: '800',
  },
  secondaryCTAButton: {
    minHeight: 44,
    minWidth: 104,
    paddingHorizontal: 16,
    borderRadius: 8,
    backgroundColor: colors.panelAlt,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: colors.border,
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'center',
    gap: 8,
  },
  secondaryCTAButtonPressed: {
    backgroundColor: '#2a2a2a',
  },
  secondaryCTAText: {
    color: colors.text,
    fontSize: 15,
    fontWeight: '700',
  },
  modalRoot: {
    flex: 1,
    justifyContent: 'flex-end',
  },
  modalBackdrop: {
    ...StyleSheet.absoluteFill,
    backgroundColor: 'rgba(0, 0, 0, 0.55)',
  },
  settingsSheet: {
    padding: 18,
    paddingBottom: 28,
    borderTopLeftRadius: 18,
    borderTopRightRadius: 18,
    backgroundColor: colors.panel,
    borderTopWidth: StyleSheet.hairlineWidth,
    borderColor: colors.border,
  },
  sheetHeader: {
    minHeight: 44,
    flexDirection: 'row',
    alignItems: 'center',
    justifyContent: 'space-between',
    gap: 12,
  },
  sheetTitle: {
    color: colors.text,
    fontSize: 20,
    fontWeight: '800',
  },
  inputLabel: {
    marginTop: 12,
    marginBottom: 8,
    color: colors.muted,
    fontSize: 12,
    fontWeight: '800',
    textTransform: 'uppercase',
  },
  urlInput: {
    minHeight: 48,
    borderRadius: 8,
    borderWidth: 1,
    borderColor: colors.border,
    backgroundColor: colors.app,
    color: colors.text,
    paddingHorizontal: 12,
    fontSize: 16,
    fontWeight: '600',
  },
  urlInputError: {
    borderColor: colors.warning,
  },
  inputError: {
    marginTop: 8,
    color: '#ffb5aa',
    fontSize: 13,
    fontWeight: '600',
  },
  sheetActions: {
    marginTop: 16,
    flexDirection: 'row',
    gap: 10,
  },
  sheetSecondaryButton: {
    flex: 1,
    minHeight: 46,
    borderRadius: 8,
    backgroundColor: colors.panelAlt,
    borderWidth: StyleSheet.hairlineWidth,
    borderColor: colors.border,
    alignItems: 'center',
    justifyContent: 'center',
  },
  sheetPrimaryButton: {
    flex: 1,
    minHeight: 46,
    borderRadius: 8,
    backgroundColor: colors.accent,
    alignItems: 'center',
    justifyContent: 'center',
  },
});
