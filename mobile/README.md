# Crabbox Mobile

iOS-first Expo app for `https://crabbox.sh` and self-hosted Crabbox
coordinators. The app keeps GitHub OAuth and portal cookies inside a native
WebView, adds iOS safe-area chrome, and provides native reload, share, browser
open, and coordinator switching controls.

## Requirements

- macOS
- Node.js 22.12 or newer
- Xcode with an iOS simulator installed
- Expo CLI through `npx`

If `npm run ios` reports that Xcode is not fully installed, finish Xcode setup:

```sh
sudo xcode-select -s /Applications/Xcode.app/Contents/Developer
sudo xcodebuild -runFirstLaunch
```

## Run On iOS

```sh
cd mobile
npm install
npm run check
npm run ios
```

For the Expo dev menu instead:

```sh
cd mobile
npm run start -- --localhost
```

Press `i` in the Expo terminal to open the iOS simulator.

## Coordinator URL

The app starts at `https://crabbox.sh`. Tap the settings button in the header to
switch to another coordinator, for example `https://broker.example.com`.

## Native Project

This app uses Expo managed workflow. Native `ios/` and `android/` folders are
generated locally only when needed:

```sh
cd mobile
npx expo prebuild --platform ios
```
