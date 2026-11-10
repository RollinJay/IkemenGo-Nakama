# ikemen-droid in this repository

This folder holds the Android app that `build/build.sh Android` packages the game into. It's SDL's Java activity, with Jesuszilla's start-up, game folder picker, asset extraction and touch controls.

- Source: https://github.com/Jesuszilla/ikemen-droid, branch `main`
- Commit: dbc5cfb49b1fed1d4a732ff48b439dd684d54145 (2026-07-24)
- License: zlib, in `LICENSE.txt`, with the notices in `README.md`

`build/build.sh` copies this folder to `build/android-apk/ikemen-droid` (`ANDROID_APK_REPO`). It stages the engine's libraries and the game's assets in that copy, so building never changes this folder. `ANDROID_APK_REPO` can still name another checkout or a git URL.

## Local changes

None yet: the files are as they are at the commit above.
