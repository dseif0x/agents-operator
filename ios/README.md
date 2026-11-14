# Agents for iOS

A small native wrapper around the agents-operator web app: the hub in a
full-screen web view. Unlike the Home Screen web app it is a real app, so the
**Action Button** can open it (Settings → Action Button → App → Agents), and it
appears in Shortcuts ("Open App" and an "Open Agents" action) and Spotlight.

- First launch asks for the hub address; change it later in iOS Settings → Agents.
- The login cookie is kept between launches.
- Attachments open in a sheet (Share saves them), downloads go to the share sheet,
  links to other sites open in Safari, camera/photos work for file uploads.
- Not available in an app web view: web push notifications. Keep the Home Screen
  web app installed if you want those.

## Build

CI (`.github/workflows/ios.yml`) builds an **unsigned** `Agents-unsigned.ipa` on
every change under `ios/` (artifact) and attaches it to `ios-v*` releases. Sign
and install it yourself, e.g. with Sideloadly or AltStore/SideStore (a free
Apple ID works, but the app then expires after 7 days), or with your developer
certificate.

Locally: `brew install xcodegen`, put a 1024×1024 `icon-1024.png` into
`Agents/Assets.xcassets/AppIcon.appiconset/` (CI uses
`web/public/icon-maskable-512.png`), then `xcodegen generate` and open
`Agents.xcodeproj`.
