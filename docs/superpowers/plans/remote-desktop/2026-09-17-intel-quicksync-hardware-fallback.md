# Intel Quick Sync hardware fallback

## Context

On the LEGION9i test client, DXGI capture succeeds at `7680x2160` and the
NVIDIA RTX 4090 is detected. Direct NVENC initialization returns
`INVALID_PARAM` for that native size. The agent then falls back to OpenH264,
which currently produces approximately 2–3 FPS because conversion and CPU
encoding take roughly 300 ms per frame.

The machine also exposes Intel UHD Graphics. The current
`encoder_quicksync.go` implementation is a build-tagged placeholder, so it
does not provide a usable Intel Quick Sync encoder. The Windows MFT path is
generic and currently selects the NVIDIA hardware path first.

## Goal

Keep hardware encoding active on systems where the primary GPU encoder rejects
the native desktop dimensions, with Intel Quick Sync as the Windows fallback.
Software OpenH264 remains the final fallback and must still receive dimensions
below its validated pixel ceiling.

## Implementation

1. Implement Intel Quick Sync through the existing Windows Media Foundation
   hardware-MFT path rather than the placeholder build-tag implementation.
2. Make hardware encoder selection vendor-aware and record the selected MFT
   identity in the session log.
3. Negotiate an encoder-safe even resolution before hardware initialization;
   resize CPU frames when the negotiated encoder dimensions differ from the
   DXGI desktop dimensions.
4. Keep the direct NVENC path available when it accepts the requested profile
   and dimensions. If it rejects them, retry with the Intel/MFT path before
   selecting OpenH264.
5. Preserve consent, input, cursor coordinates, aspect ratio, keyframe
   recovery, and the existing WebRTC transport behavior.

## Acceptance criteria

- A `7680x2160` session logs hardware encoding through NVENC or Intel Quick
  Sync/MFT, not OpenH264, when the client has a supported GPU driver.
- The viewer receives more than 10 FPS on the LEGION9i test client under
  normal desktop activity.
- OpenH264 remains a working fallback below its pixel limit.
- No frame is sent with dimensions rejected by the selected encoder.
- Consent and remote input continue to work.

## Verification

- Run the agent Go tests with race detection where the toolchain is available.
- Build the Windows agent, user-helper, Helper MSI, and Viewer MSI through the
  private-fork workflow.
- Install all artifacts on LEGION9i.
- Verify logs for encoder selection, negotiated dimensions, encoded frames,
  sent frames, viewer decoded frames, and dropped frames.
