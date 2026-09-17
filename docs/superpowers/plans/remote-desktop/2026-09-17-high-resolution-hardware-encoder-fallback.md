# High-resolution hardware encoder fallback

## Context

The LEGION9i desktop is captured at `7680x2160`. Capture and WebRTC
signalling work, but the direct NVENC path rejects the native dimensions with
`INVALID_PARAM`. The current fallback uses OpenH264 at roughly 2–3 FPS because
CPU conversion and encoding take approximately 300 ms per frame.

## Goal

Keep hardware encoding active for ultrawide/high-resolution displays by
negotiating an encoder-safe resolution before hardware initialization. Preserve
the original display aspect ratio and input coordinate mapping. Use OpenH264
only as a final fallback.

## Scope

- Add a shared, even-dimension hardware encoder resolution policy.
- Apply the policy before NVENC/MFT initialization.
- Scale captured CPU frames when the encoder resolution differs from the
  display resolution.
- Ensure GPU-only paths do not submit textures whose dimensions differ from
  the encoder configuration unless the GPU scaler handles that conversion.
- Add explicit logs for source dimensions, negotiated dimensions, backend, FPS,
  and fallback reason.
- Keep consent, input, cursor mapping, and WebRTC negotiation unchanged.

## Acceptance criteria

- `7680x2160` no longer causes NVENC `INVALID_PARAM` during normal operation.
- Hardware encoding remains selected when the GPU/driver supports the
  negotiated dimensions.
- The final software fallback stays below the OpenH264 pixel ceiling.
- The viewer receives at least 10 FPS during normal desktop activity on the
  LEGION9i test client.
- No increase in packet loss, decoded-frame drops, or input-coordinate errors.

## Verification

- Run focused Go encoder/session tests and the agent test suite with race
  detection where the local toolchain is available.
- Build the Windows agent and user-helper artifacts in GitHub Actions.
- Install the new agent on LEGION9i and run a `7680x2160` consented session.
- Confirm logs show negotiated dimensions, selected backend, encoded/sent
  frames, viewer decoded frames, and zero encoder errors.
