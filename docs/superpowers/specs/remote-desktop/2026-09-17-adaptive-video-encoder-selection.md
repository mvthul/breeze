# Adaptive video encoder selection for remote desktop

## Purpose

Define how the Breeze agent and viewer should select and negotiate video
encoders across NVIDIA NVENC, Intel Quick Sync, AMD hardware encoders, and the
CPU fallback. The design must prevent black screens when a GPU driver rejects a
resolution or stops producing output.

## Responsibilities

### Agent

The agent owns capture, encoder selection, resolution negotiation, fallback,
and encoded-frame production. The viewer must not decide which physical GPU is
used.

The agent should report these values in session diagnostics:

- capture adapter and display dimensions;
- selected encoder and vendor;
- negotiated encoder dimensions;
- hardware/software status;
- fallback reason and retry count;
- captured, encoded, sent, skipped, and dropped frames.

### Viewer

The viewer requests a policy and displays the negotiated result. It should:

- offer `Auto`, `NVIDIA NVENC`, `Intel Quick Sync`, `AMD AMF`, and `CPU`; 
- send the preference as a session policy, never as executable code;
- show the actual encoder selected by the agent;
- show the negotiated stream dimensions and FPS;
- tolerate a codec/stream renegotiation during fallback.

The default must be `Auto`.

## Automatic selection order

The agent should use the capture adapter and hardware inventory to rank
available encoders. A suitable default policy is:

```text
capture-adapter-matched hardware encoder
  -> other supported hardware encoder
  -> CPU H.264 encoder
```

For a hybrid Windows client where the capture adapter is Intel, the concrete
order is:

```text
Intel Quick Sync / Media Foundation
  -> NVIDIA NVENC
  -> AMD AMF
  -> OpenH264 or x264 CPU fallback
```

If the capture adapter is NVIDIA, NVENC should be attempted first. The order
must not be hard-coded globally; it should be derived from adapter identity,
the selected policy, and successful capability probes.

## Resolution negotiation

Resolution negotiation happens before encoder initialization:

1. Capture the source display dimensions.
2. Ask the selected encoder for its supported dimensions and profile limits.
3. Preserve aspect ratio and choose even dimensions suitable for H.264 4:2:0.
4. Apply a safety margin below codec limits.
5. Initialize the encoder with the negotiated dimensions.
6. Scale CPU frames when source and encoder dimensions differ.
7. Use a GPU scaler for texture-based paths; never submit a texture whose size
   disagrees with the encoder configuration.

The viewer should receive the negotiated dimensions through the media stream
and diagnostics rather than assuming the physical display resolution.

## Probe and fallback rules

An encoder is not considered usable merely because its DLL or MFT exists. The
agent must complete a bounded probe:

- initialize the encoder;
- submit a valid test frame;
- require encoded output within a short timeout;
- reject `INVALID_PARAM`, repeated empty output, stalls, and device-loss errors.

Fallback behavior:

```text
initialization failure -> try next ranked encoder
three consecutive startup output failures -> try next ranked encoder
runtime stall/device loss -> flush and retry once
repeated runtime failure -> demote and switch encoder
all hardware unavailable -> CPU fallback
```

Every transition must force an IDR/keyframe and update the session diagnostics.
The viewer must remain connected while the agent changes encoder backend when
the codec and transport remain compatible.

## Manual selection

Manual selection changes the ranking, not the safety rules:

```text
Auto             ranked capability-based selection
NVIDIA NVENC     NVIDIA first, then safe fallback
Intel Quick Sync Intel first, then safe fallback
AMD AMF          AMD first, then safe fallback
CPU              skip hardware and use software directly
```

A manually selected encoder may still fall back unless the user explicitly
chooses a future `Strict` mode. If fallback occurs, the viewer should show the
actual backend and reason instead of silently reporting the requested backend.

## Recommended implementation order

1. Shared encoder capability and negotiated-dimensions model in the agent.
2. Reliable Intel Quick Sync/MFT selection and output probe.
3. NVENC high-resolution negotiation and GPU scaling.
4. AMD AMF parity.
5. Agent diagnostics and viewer encoder/FPS display.
6. Manual selection policy and end-to-end tests.

## Acceptance criteria

- A `7680x2160` display never produces a black stream solely because one
  encoder rejects the native dimensions.
- The agent reports the actual backend and negotiated resolution.
- Hardware fallback keeps the session above the CPU fallback frame rate on
  supported hardware.
- CPU fallback remains valid, bounded, and visibly reported.
- Automatic and manual policies are covered by agent and viewer tests.
