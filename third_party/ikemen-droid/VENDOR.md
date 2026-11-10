# ikemen-droid in this repository

This folder holds the Android app that `build/build.sh Android` packages the game into. It's SDL's Java activity, with Jesuszilla's start-up, game folder picker, asset extraction and touch controls.

- Source: https://github.com/Jesuszilla/ikemen-droid, branch `main`
- Commit: dbc5cfb49b1fed1d4a732ff48b439dd684d54145 (2026-07-24)
- License: zlib, in `LICENSE.txt`, with the notices in `README.md`

`build/build.sh` copies this folder to `build/android-apk/ikemen-droid` (`ANDROID_APK_REPO`). It stages the engine's libraries and the game's assets in that copy, so building never changes this folder. `ANDROID_APK_REPO` can still name another checkout or a git URL.

## Local changes

- **`ControllerOverlay`:**
  - The on-screen buttons play as a controller named "IKEMEN Touch Controls", with no vendor or product ID and a fixed device ID. SDL keeps that name, so the game can recognise it, and the ID can't collide with a real device.
  - The buttons can be hidden, which also removes their controller.
  - Each button shows the glyph image the game writes for it, or its letter where there is none.
- **`TouchControls`** (new):
  - **Setting:** Automatic, Always show or Always hide, chosen from a small settings button. Automatic hides the buttons on Android TV, without a touch screen, and while a real controller is connected.
  - **Keyboard button:** shown while the game has a text box open and its keyboard is closed.
  - **Remote hint:** without a touch screen, as on Android TV, the same case shows "Press OK to type, or Back to cancel" instead.
- **`SDLActivity`:**
  - **From the game:** it handles commands 0x8001 (a text box opened or closed) and 0x8002 (the glyph images changed) in `onUnhandledMessage`.
  - **To the game:** it calls `nativeShowKeyboard`.
  - **Text field:** SDL's `DummyEdit` lives in a layer of its own (`mTextLayout`), which the 5 s fade of the controls leaves alone, and asks the keyboard for no suggestions or corrections.
- **`AssetExtractor`:**
  - It always replaces `external/script`.
  - Any other file the folder already has, it replaces only if it wrote that file itself and the file is unchanged since; it keeps that record in `save/.ikemen-droid-assets`. A game's own `select.def`, `fight.def`, glyphs, server list and mods therefore stay.
- **Resources:**
  - `round_button.xml` moves from `drawable-v24` to `drawable`, so that Android 5 and 6 can load the layout.
  - New: the settings and keyboard icons, their strings, and an Android TV banner.
- **Manifest:** a `LEANBACK_LAUNCHER` entry and the banner, with the leanback feature not required.
