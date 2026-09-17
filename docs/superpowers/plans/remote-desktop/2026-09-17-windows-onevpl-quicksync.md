# Windows oneVPL Quick Sync encoder

## Goal

Use Intel Quick Sync on Windows through the installed oneVPL runtime instead of
falling directly from the Media Foundation probe to OpenH264. The existing
Media Foundation backend remains a fallback, and systems without a usable Intel
runtime must keep working unchanged.

## Current evidence

- LEGION9i exposes `h264_qsv` and `qsv` through FFmpeg.
- A real H.264 encode initialises an accelerated oneVPL implementation.
- Breeze's `MFTEnumEx(MFT_ENUM_FLAG_HARDWARE)` returns no usable H.264 MFT and
  the session selects `mft-software`/OpenH264 at 3200x2000.
- Upstream's `encoder_quicksync.go` is a build-tagged placeholder and must not
  be enabled as-is.

## Design

1. Add a Windows-only pure-Go oneVPL dispatcher. Load `vpl.dll` dynamically;
   never make the agent require oneVPL at process start.
2. Vendor only the stable oneVPL ABI declarations needed for H.264 encode:
   loader/config/session functions, encode/query/reset/sync functions, frame
   and bitstream structures, and the D3D11 surface-sharing extension.
3. Select the Intel implementation with the oneVPL filter properties and
   require hardware acceleration. If loading, selecting, or initialising the
   runtime fails, return an error so the normal MFT/OpenH264 fallback runs.
4. Use the existing D3D11 capture texture when available. For the first
   version, support the CPU NV12 input path as a correctness fallback and add
   zero-copy D3D11 surfaces after the CPU path is validated.
5. Keep encoder ownership and fallback semantics aligned with `encoderBackend`:
   bounded async waits, keyframe forcing, bitrate updates, dimensions changes,
   and clean close/reset on monitor changes.
6. Add explicit logs: runtime path, implementation acceleration mode, codec,
   dimensions, input path, and fallback reason. Do not log raw GPU handles.

## Fallback order

`oneVPL Quick Sync` → `Windows hardware MFT` → `OpenH264` → existing placeholder
error path. A runtime that loads but cannot encode the first frame is demoted
without terminating the remote session.

## Delivery slices

1. ABI loader and capability probe, with Windows compile tests and a diagnostic
   command/test fixture.
2. CPU NV12 H.264 encode and encoder contract tests.
3. D3D11 surface sharing and monitor-switch recovery.
4. CI Windows artifact build, LEGION9i validation, and metrics comparison.

## Acceptance criteria

- LEGION9i logs `backend=onevpl-qsv` and reports hardware acceleration.
- A 3200x2000 session sustains materially above the current 6 FPS CPU path,
  with no increase in dropped frames or black-screen regressions.
- A machine without `vpl.dll`, or with a failed QSV initialisation, logs the
  reason and still starts through the existing fallback chain.
- Existing Windows, macOS, Linux, helper, viewer, and agent tests remain green.
