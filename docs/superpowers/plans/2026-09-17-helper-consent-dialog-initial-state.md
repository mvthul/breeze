---
tracking_issue: null
---
# Fix Helper Consent Prompt Blank Screen and Viewer Connection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ensure that when a remote desktop session has "Require consent — the user must approve" enabled, the end user on the target machine is presented with the interactive Allow/Deny consent dialog immediately, and the remote desktop viewer connects seamlessly once allowed or when proceeding after timeout.

**Root Causes:**
1. **Helper Window Startup Race Condition:** In `apps/helper/src-tauri/src/ipc/desktop.rs`, `show_consent_window` built the webview window and immediately called `emit_consent_request(app, req)`. Because the newly spawned webview takes a few hundred milliseconds to load `index.html#consent` and mount React, the event was emitted when 0 webview listeners existed and was permanently lost. In `apps/helper/src/main.tsx`, `ConsentWindow` initialized `req` to `null`. With `if (!req) return null;`, it rendered nothing, leaving the window as a blank white rectangle. The same race condition affected `show_banner_window` / `BannerWindow`.
2. **Viewer WebRTC Answer Poll Timeout (15s vs 30s consent):** In `apps/viewer/src/lib/webrtc.ts`, `pollForAnswer` timed out after 15,000 ms. The agent's consent gate waits up to 30,000 ms for user approval. If the user takes >15s to click Allow, or if the policy specifies "Proceed" on timeout (unattended 30s wait), the viewer aborted WebRTC at 15s and fell back to WebSocket, leaving the session black and stalled at 0 FPS.
3. **Window Frame & Transparency:** The consent window was built without `transparent(true).shadow(false)`, leaving an opaque box around the card.

## Proposed Changes

### Helper App Backend (`apps/helper/src-tauri`)

#### [apps/helper/src-tauri/src/ipc/desktop.rs](file:///home/paseo/projects/breezermm/apps/helper/src-tauri/src/ipc/desktop.rs)
- [x] Store active consent request in thread-safe `PENDING_CONSENT` static mutex.
- [x] Store active banner show request in thread-safe `PENDING_BANNER` static mutex.
- [x] Expose `get_pending_consent()`, `clear_pending_consent()`, `get_pending_banner()`, and `clear_pending_banner()`.
- [x] In `show_consent_window`, configure `builder.transparent(true).shadow(false)` and apply Windows SYSTEM WebView2 data directory if applicable.
- [x] In `show_banner_window`, apply Windows SYSTEM WebView2 data directory if applicable.
- [x] In `close_consent_window` and `hide_banner_window`, clear the pending payloads.

#### [apps/helper/src-tauri/src/lib.rs](file:///home/paseo/projects/breezermm/apps/helper/src-tauri/src/lib.rs)
- [x] Register `get_consent_request` and `get_banner_payload` Tauri commands in `invoke_handler`.
- [x] Clear pending consent in `submit_consent`.

### Helper App Frontend (`apps/helper/src`)

#### [apps/helper/src/main.tsx](file:///home/paseo/projects/breezermm/apps/helper/src/main.tsx)
- [x] In `ConsentWindow`: On mount (`useEffect`), call `invoke<ConsentRequest | null>('get_consent_request')` to populate initial state immediately, while maintaining the `listen('consent-request')` subscription.
- [x] In `ConsentWindow`: Provide a loading card placeholder if `req` is null instead of returning `null`, avoiding blank white flashes.
- [x] In `BannerWindow`: On mount (`useEffect`), call `invoke<BannerPayload | null>('get_banner_payload')`.
- [x] Set `document.getElementById('root')!.className = 'consent-root'` for `#consent`.

#### [apps/helper/src/styles.css](file:///home/paseo/projects/breezermm/apps/helper/src/styles.css)
- [x] Add `.consent-root, .consent-root body { background: transparent; }` and `.consent-root .helper-consent-overlay { background: transparent; }` so the card floats cleanly with its native drop shadow and rounded corners.

#### [apps/helper/src/windows/ConsentDialog.test.tsx](file:///home/paseo/projects/breezermm/apps/helper/src/windows/ConsentDialog.test.tsx)
- [x] Unit tests for `ConsentDialog`:
  - Renders technician name, email, org name.
  - Fallback when technician name is null ("A technician", "◐").
  - Default focus is on Deny button.
  - Clicking Allow calls `onDecision(true, 'user')`.
  - Clicking Deny calls `onDecision(false, 'user')`.
  - Escape key calls `onDecision(false, 'user')`.
  - Countdown decrement and timeout policy execution.

#### [apps/helper/src/windows/SessionBanner.test.tsx](file:///home/paseo/projects/breezermm/apps/helper/src/windows/SessionBanner.test.tsx)
- [x] Unit tests for `SessionBanner`:
  - Renders session label and live elapsed timer.

### Viewer App (`apps/viewer`)

#### [apps/viewer/src/lib/webrtc.ts](file:///home/paseo/projects/breezermm/apps/viewer/src/lib/webrtc.ts)
- [x] Increase WebRTC answer poll timeout from 15,000 ms to 45,000 ms (`DEFAULT_ANSWER_POLL_TIMEOUT_MS = 45000`) so the 30s consent prompt window does not trigger a premature WebRTC timeout. Note that terminal failure states (denied, failed, ended) still exit immediately.

#### [apps/viewer/src/lib/sessionEnded.test.ts](file:///home/paseo/projects/breezermm/apps/viewer/src/lib/sessionEnded.test.ts)
- [x] Update pacing tests to verify 45s window behavior.

## Verification Plan

### Automated Tests
- Run `pnpm --filter breeze-helper test` (all 15 test suites + new ConsentDialog & SessionBanner tests).
- Run `pnpm --filter breeze-helper exec tsc --noEmit`.
- Run `pnpm --filter @breeze/viewer test` (all 27 test suites).
- Run `pnpm --filter @breeze/viewer exec tsc --noEmit`.

### Manual / Integration Verification
- Rebuild helper and verify consent window renders technician identity, countdown, and functional Allow/Deny buttons.
- Confirm WebRTC connection connects directly on Allow without falling back to black WebSocket stream.
