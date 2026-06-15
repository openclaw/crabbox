# Distribution

How to get Crabbox iOS onto a device, from "try it in five minutes" to a real
TestFlight build — and what each path needs from *your* Apple account.

## Quick comparison

| Path                       | Apple account        | Lifetime           | Best for                       |
| -------------------------- | -------------------- | ------------------ | ------------------------------ |
| Free provisioning          | Free Apple ID        | ~7 days, re-sign   | Trying it on your own phone    |
| TestFlight                 | **Paid** ($99/yr)    | 90 days per build  | Real testers, "ship it"        |
| App Store                  | **Paid** ($99/yr)    | Until you remove   | Public release                 |
| PWA add-to-home-screen     | **None**             | As long as you use | No-install, the portal only    |

## Free provisioning — "try today"

You can run Crabbox on *your own* iPhone with just a free Apple ID and Xcode, no
paid program required:

1. `xcodegen generate` to produce `Crabbox.xcodeproj`, then open it in Xcode.
2. Select the **Crabbox** scheme and your connected iPhone as the destination.
3. Under **Signing & Capabilities**, sign in with your Apple ID and let Xcode
   manage signing. Set a **unique bundle identifier** (free provisioning won't
   reuse `sh.crabbox.Crabbox` if someone else has claimed it — append your own
   suffix, e.g. `sh.crabbox.Crabbox.yourname`).
4. Build & run. First launch may require trusting the developer certificate on
   the device under **Settings → General → VPN & Device Management**.

Caveats of free provisioning: the app's provisioning profile expires in about
**7 days**, after which you re-run from Xcode to re-sign. You're also limited to
a handful of app installs per week. It's perfect for kicking the tires; it is
not a way to distribute to others.

## TestFlight — "real" (paid)

For sharing builds with testers, you need a **paid Apple Developer Program**
membership ($99/year):

1. Create the App ID and an app record in **App Store Connect**.
2. In Xcode, archive the **Crabbox** scheme
   (**Product → Archive**) with your team's signing.
3. Upload the archive to App Store Connect (Xcode Organizer or `xcrun altool` /
   `notarytool`).
4. Add internal and/or external testers in TestFlight. External testers require
   a short Beta App Review; each build is valid for 90 days.

The App Store release path is the same archive/upload, followed by submitting
for full App Store review.

## What needs *your* Apple account

- **Free provisioning** and **TestFlight/App Store** both run under *your* Apple
  ID / Developer account — code signing happens on your machine with your
  credentials. This repo's CI builds the iOS app **unsigned, build-only**, so it
  never needs an account; turning a build into something installable is the one
  step that requires you.
- The paid program ($99/yr) is the gate for **TestFlight and the App Store**.
  Free provisioning needs only a free Apple ID.

## PWA add-to-home-screen — no account at all

Because the Portal is just `crabbox.sh` in a WebView, the underlying coordinator
is a normal web app. If you don't want to install anything, open
`https://crabbox.sh` in Safari and use **Share → Add to Home Screen** to get a
home-screen icon and a standalone, full-screen web experience. This needs no
Apple Developer account and no signing — but it's the web portal only, without
the native Assistant and Sandboxes tabs or on-device LLM.
