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
npm ci
npm run validate
npm run ios
```

For the Expo dev menu instead:

```sh
cd mobile
npm run start -- --localhost
```

Press `i` in the Expo terminal to open the iOS simulator.

## End-To-End Test

Start from the PR checkout:

```sh
cd /Users/yossi.eliaz/Documents/crabbox-pr379/mobile
npm ci
npm run doctor:ios
npm run validate
```

If `doctor:ios` cannot find `simctl`, install Xcode from the Mac App Store,
open Xcode once, install an iOS simulator runtime, then select Xcode:

```sh
sudo xcode-select -s /Applications/Xcode.app/Contents/Developer
sudo xcodebuild -runFirstLaunch
npm run doctor:ios
```

Run the app in the iOS simulator:

```sh
npm run ios
```

For a physical iPhone with Expo Go installed, keep the phone and Mac on the same
network, then run:

```sh
npm run start
```

Scan the QR code from Expo Go. If the phone cannot reach the Mac over LAN, use:

```sh
npx expo start --tunnel
```

For a native device/simulator build instead of Expo Go:

```sh
npm run prebuild:ios
npm run run:ios
```

E2E proof checklist:

- Open the app and confirm the header shows `crabbox.sh`.
- Complete the GitHub/portal login flow with private details redacted.
- Tap reload and confirm the portal session stays signed in.
- Open coordinator settings and switch to another HTTPS coordinator.
- Try a non-local `http://` coordinator and confirm the app rejects it.
- In a development build, optionally try `http://localhost:8787` and confirm
  loopback HTTP is accepted only for local testing.
- Capture a redacted screenshot or short recording and add it to the PR.

## Coordinator URL

The app starts at `https://crabbox.sh`. Tap the settings button in the header to
switch to another coordinator, for example `https://broker.example.com`.

Coordinator sessions require HTTPS. Development builds also accept local
loopback HTTP URLs such as `http://localhost:8787` for testing a broker on the
same machine; production builds reject HTTP coordinators and the WebView does
not whitelist arbitrary cleartext origins.

## Native Project

This app uses Expo managed workflow. Native `ios/` and `android/` folders are
generated locally only when needed:

```sh
cd mobile
npx expo prebuild --platform ios
```
