//go:build windows

package desktop

import (
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// vbvSizeForBitrate returns the VBV buffer size (in bits) for a given bitrate,
// targeting 500ms of headroom (bitrate / 2). The floor of 500K bits ensures
// I-frames remain viable even at MinBitrate (500 Kbps), where the 500ms
// ratio would yield only 250K — too small for a 1080p keyframe.
func vbvSizeForBitrate(bitrate int) uint32 {
	vbvSize := uint32(bitrate / 2)
	if vbvSize < 500000 {
		vbvSize = 500000
	}
	return vbvSize
}

// mftEncoder implements encoderBackend using Windows Media Foundation Transform.
// It discovers and uses hardware H264 encoders (NVENC, QuickSync, AMD VCE)
// via the MFT enumeration API, falling back to the software H264 MFT.
type mftEncoder struct {
	mu sync.Mutex
	convertTimer

	cfg    EncoderConfig
	width  int
	height int
	stride int

	// COM handles (persistent across frames)
	transform       uintptr // IMFTransform
	codecAPI        uintptr // ICodecAPI (for dynamic bitrate), may be 0
	inited          bool
	isHW            bool
	providesSamples bool // MFT allocates its own output samples
	outputBufSize   int  // required output buffer size from GetOutputStreamInfo

	// Async MFT event model. Hardware MFTs (Intel QuickSync, NVENC, AMD VCE) are
	// asynchronous: they deliver METransformNeedInput / METransformHaveOutput
	// events through IMFMediaEventGenerator, and ProcessInput/ProcessOutput must
	// be gated on those events. Driving an async MFT with synchronous polling
	// (ProcessInput then ProcessOutput every frame) makes it accept input but
	// never produce output — the permanent-stall signature on Intel UHD 630.
	// eventGen is 0 for synchronous MFTs (e.g. the software fallback), in which
	// case the legacy synchronous path is used.
	eventGen         uintptr  // IMFMediaEventGenerator, 0 if MFT is synchronous
	asyncMode        bool     // true when eventGen != 0 (drive via event handshake)
	needInputCredits int      // METransformNeedInput events received, not yet satisfied
	pendingOutput    [][]byte // encoded frames drained from HaveOutput, returned FIFO

	// Frame timing
	frameIdx  uint64
	startTime time.Time

	// Thread affinity
	threadLocked   bool
	comInitialized bool

	// Pixel format of incoming frames
	pixelFormat PixelFormat

	// GPU zero-copy pipeline
	d3d11Device    uintptr // ID3D11Device (from capturer, not owned)
	d3d11Context   uintptr // ID3D11DeviceContext (from capturer, not owned)
	gpuConv        *gpuConverter
	gpuFrameCount  uint64  // frames since gpuConv was (re)created, for diagnostic logging
	dxgiManager    uintptr // IMFDXGIDeviceManager
	dxgiResetToken uint32
	gpuEnabled     bool
	gpuFailed      bool // permanently disabled after init failure
	zeroCopyLogged bool // true after first successful zero-copy output is logged
	// useDXGISamples: feed the MFT DXGI-surface (GPU texture) input samples
	// instead of CPU memory buffers — skips the NV12 readback + memcpy per
	// frame. Requires the async event handshake (asyncMode) and the DXGI
	// device manager (dxgiManager). Cleared if the zero-copy path stalls,
	// downgrading to the GPU-convert + readback path.
	useDXGISamples bool

	// Keyframe forcing: set when we want the next output to be an IDR.
	forceKeyframePending bool

	// Diagnostic: consecutive Encode() calls that returned nil (MFT buffering).
	consecutiveNilOutputs int
	// lastStallFlush prevents rapid flush loops when the MFT is fundamentally
	// broken (not just warming up). Minimum 5s between stall-triggered flushes.
	lastStallFlush time.Time
	// stallFlushCount tracks consecutive stall-flush cycles without the encoder
	// producing any output. After 2+ cycles, the encoder is permanently stalled.
	stallFlushCount    int
	outputSinceFlush   bool
	permanentlyStalled bool
}

func init() {
	registerHardwareFactory(newMFTEncoder)
}

func newMFTEncoder(cfg EncoderConfig) (encoderBackend, error) {
	if cfg.Codec != CodecH264 {
		return nil, fmt.Errorf("MFT encoder only supports H264, got %s", cfg.Codec)
	}
	if cfg.GPUVendor == "intel" {
		if err := probeOneVPLRuntime(); err == nil {
			slog.Info("Intel oneVPL runtime detected; dedicated QSV backend is not enabled yet")
		} else {
			slog.Debug("Intel oneVPL runtime unavailable; continuing with Media Foundation fallback", "error", err.Error())
		}
	}
	// Probe for hardware MFTs at creation time so the factory fails fast
	// when no GPU encoder is available. This lets newBackend() fall through
	// to OpenH264 instead of returning a struct that fails lazily on Encode().
	if !probeHardwareMFT() {
		return nil, fmt.Errorf("no hardware H264 MFT available")
	}
	return &mftEncoder{
		cfg:       cfg,
		startTime: time.Now(),
	}, nil
}

// probeHardwareMFT checks if a hardware H264 encoder MFT exists without
// fully initializing it. Returns false on headless servers / basic GPUs
// (e.g. Matrox G200) that lack hardware H264 encoding.
func probeHardwareMFT() bool {
	// COM init (best-effort, may already be initialized)
	hr, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded)
	if int32(hr) < 0 && uint32(hr) != 0x80010106 {
		return false
	}
	procMFStartup.Call(mfVersion, mfStartupFull)

	// Some Intel drivers do not publish their concrete type information until
	// activation. Probe the unfiltered hardware category first, then retain the
	// typed query for drivers that require it. Log both results: this makes a
	// driver/call-ABI problem distinguishable from a genuine lack of hardware.
	if count, hr := enumerateMFTActivations(
		mftEnumFlagHardware|mftEnumFlagSortAndFilter, nil, nil,
	); count > 0 && int32(hr) >= 0 {
		slog.Info("MFT hardware probe succeeded", "query", "untyped", "count", count, "hresult", fmt.Sprintf("0x%08X", uint32(hr)))
		return true
	} else {
		slog.Warn("MFT hardware probe returned no candidates", "query", "untyped", "count", count, "hresult", fmt.Sprintf("0x%08X", uint32(hr)))
	}

	inputType := mftRegisterTypeInfo{mfMediaTypeVideo, mfVideoFormatNV12}
	outputType := mftRegisterTypeInfo{mfMediaTypeVideo, mfVideoFormatH264}
	count, hr := enumerateMFTActivations(
		mftEnumFlagHardware|mftEnumFlagSortAndFilter, &inputType, &outputType,
	)
	if count > 0 && int32(hr) >= 0 {
		slog.Info("MFT hardware probe succeeded", "query", "nv12-h264", "count", count, "hresult", fmt.Sprintf("0x%08X", uint32(hr)))
		return true
	}
	slog.Warn("MFT hardware probe returned no candidates", "query", "nv12-h264", "count", count, "hresult", fmt.Sprintf("0x%08X", uint32(hr)))
	return false
}

// initialize sets up COM, finds an MFT H264 encoder, and configures it.
// Called lazily on the first Encode with known dimensions.
func (m *mftEncoder) initialize(width, height, stride int) error {
	// Lock this goroutine to an OS thread for COM thread affinity
	if !m.threadLocked {
		runtime.LockOSThread()
		m.threadLocked = true
	}

	// COM init
	hr, _, _ := procCoInitializeEx.Call(0, coinitMultithreaded)
	if int32(hr) < 0 && uint32(hr) != 0x80010106 { // ignore RPC_E_CHANGED_MODE
		runtime.UnlockOSThread()
		m.threadLocked = false
		return fmt.Errorf("CoInitializeEx failed: 0x%08X", uint32(hr))
	}
	m.comInitialized = int32(hr) >= 0

	// MFStartup
	hr, _, _ = procMFStartup.Call(mfVersion, mfStartupFull)
	if int32(hr) < 0 {
		m.abortFailedInitialization(0, false)
		return fmt.Errorf("MFStartup failed: 0x%08X", uint32(hr))
	}

	// Select and configure a hardware H264 encoder as one transaction.  Intel
	// Quick Sync publishes several activations on some driver versions; an
	// activation is not usable until its actual media types have negotiated.
	transform, err := m.findAndConfigureHardwareEncoder(width, height)
	if err != nil {
		m.abortFailedInitialization(0, true)
		return fmt.Errorf("no H264 encoder found: %w", err)
	}
	isHW := true

	// Attach DXGI only after a candidate has completed CPU/NV12 media-type
	// negotiation. This keeps a failed candidate from leaving device-manager
	// state behind and makes the CPU-NV12 path a reliable baseline.
	if m.d3d11Device != 0 && !m.gpuFailed {
		m.tryInitGPUPipeline(transform)
	}

	// Enable low-latency mode
	m.setLowLatency(transform)

	// Begin streaming
	if _, err := comCall(transform, vtblProcessMessage, mftMessageNotifyBeginStreaming, 0); err != nil {
		slog.Warn("MFT BeginStreaming failed (non-fatal)", "error", err.Error())
	}
	if _, err := comCall(transform, vtblProcessMessage, mftMessageNotifyStartOfStream, 0); err != nil {
		slog.Warn("MFT StartOfStream failed (non-fatal)", "error", err.Error())
	}

	m.transform = transform
	m.width = width
	m.height = height
	m.stride = stride
	m.isHW = isHW
	m.inited = true

	// Async MFT detection: hardware MFTs implement IMFMediaEventGenerator and
	// must be driven via the METransformNeedInput/METransformHaveOutput event
	// handshake. QueryInterface is the authoritative test — a synchronous MFT
	// (e.g. the software fallback) returns E_NOINTERFACE, leaving eventGen=0 and
	// the legacy synchronous ProcessInput/ProcessOutput path in effect.
	m.eventGen = 0
	m.asyncMode = false
	m.needInputCredits = 0
	m.pendingOutput = nil
	var eventGen uintptr
	if _, qiErr := comCall(m.transform, vtblQueryInterface,
		uintptr(unsafe.Pointer(&iidIMFMediaEventGenerator)),
		uintptr(unsafe.Pointer(&eventGen)),
	); qiErr == nil && eventGen != 0 {
		m.eventGen = eventGen
		m.asyncMode = true
		slog.Info("Async MFT event model enabled (IMFMediaEventGenerator)", "isHW", isHW)
	} else if isHW {
		// A hardware MFT with no event generator is unusual; the sync path may
		// still stall, but we let it try rather than fail init.
		slog.Warn("Hardware MFT does not expose IMFMediaEventGenerator, using synchronous path",
			"error", fmt.Sprintf("%v", qiErr))
	}
	// Zero-copy input requires the async handshake: feeding DXGI-surface
	// samples to a synchronously-driven hardware MFT is the historical stall
	// (tested on Kit pre-async). If the MFT turned out synchronous, revert to
	// CPU-buffer input now.
	if m.useDXGISamples && !m.asyncMode {
		slog.Warn("Zero-copy input disabled: MFT is not async")
		m.teardownDXGIManager()
	}

	// Query output stream info for buffer requirements and sample allocation
	var streamInfo mftOutputStreamInfo
	hr, _, _ = syscall.SyscallN(
		m.vtblFn(vtblGetOutputStreamInfo),
		m.transform,
		0, // stream ID
		uintptr(unsafe.Pointer(&streamInfo)),
	)
	if int32(hr) >= 0 {
		m.providesSamples = (streamInfo.dwFlags & mftOutputStreamProvidesSamples) != 0
		m.outputBufSize = int(streamInfo.cbSize)
	}
	// Ensure we have a reasonable minimum buffer size
	if m.outputBufSize <= 0 {
		// Default: uncompressed frame size (generous for H264 output)
		m.outputBufSize = width * height * 3 / 2
	}

	// Acquire ICodecAPI for dynamic bitrate control.
	// QueryInterface on the transform for IID_ICodecAPI.
	var codecAPI uintptr
	_, qiErr := comCall(m.transform, vtblQueryInterface,
		uintptr(unsafe.Pointer(&iidICodecAPI)),
		uintptr(unsafe.Pointer(&codecAPI)),
	)
	if qiErr == nil && codecAPI != 0 {
		m.codecAPI = codecAPI

		// Set GOP size (keyframe interval) = 3 seconds at configured FPS.
		// Longer GOPs reduce the frequency of large I-frames that cause
		// visible quality dips under CBR rate control. WebRTC PLI/FIR
		// handles on-demand keyframe recovery for packet loss cases.
		cfgFPS := m.cfg.FPS
		if cfgFPS <= 0 {
			cfgFPS = 30
		}
		gopSize := uint32(cfgFPS * 3)
		if gopSize < 30 {
			gopSize = 30
		}
		gv := comVariant{vt: vtUI4, val: uint64(gopSize)}
		if _, err := comCall(codecAPI, vtblCodecAPISetValue,
			uintptr(unsafe.Pointer(&codecAPIAVEncMPVGOPSize)),
			uintptr(unsafe.Pointer(&gv)),
		); err != nil {
			slog.Debug("ICodecAPI SetValue(GOPSize) failed (non-fatal)", "gopSize", gopSize, "error", err.Error())
		} else {
			slog.Debug("GOP size set via ICodecAPI", "gopSize", gopSize)
		}

		// Zero-latency configuration: eliminate encoder frame buffering.
		// Screen sharing is real-time — every frame in should produce a frame
		// out immediately. Buffering only adds lag.

		// 1. Disable B-frames: B-frames require future reference frames,
		//    adding 1+ frame of reordering latency.
		bv := comVariant{vt: vtUI4, val: 0}
		if _, err := comCall(codecAPI, vtblCodecAPISetValue,
			uintptr(unsafe.Pointer(&codecAPIAVEncMPVDefaultBPictureCount)),
			uintptr(unsafe.Pointer(&bv)),
		); err != nil {
			slog.Debug("ICodecAPI SetValue(BPictureCount=0) failed (non-fatal)", "error", err.Error())
		}

		// 2. CBR rate control with VBV buffer for bitrate smoothing.
		rv := comVariant{vt: vtUI4, val: uint64(eAVEncCommonRateControlMode_CBR)}
		if _, err := comCall(codecAPI, vtblCodecAPISetValue,
			uintptr(unsafe.Pointer(&codecAPIAVEncCommonRateControlMode)),
			uintptr(unsafe.Pointer(&rv)),
		); err != nil {
			slog.Warn("CBR rate control configuration failed", "error", err.Error())
		}
		vbvSize := vbvSizeForBitrate(m.cfg.Bitrate)
		vbv := comVariant{vt: vtUI4, val: uint64(vbvSize)}
		if _, vbvErr := comCall(codecAPI, vtblCodecAPISetValue,
			uintptr(unsafe.Pointer(&codecAPIAVEncCommonBufferSize)),
			uintptr(unsafe.Pointer(&vbv)),
		); vbvErr != nil {
			slog.Warn("VBV buffer configuration failed", "error", vbvErr.Error())
		}

		// 3. CODECAPI_AVLowLatencyMode: forces single-frame encoding mode.
		//    MF_LOW_LATENCY (set via IMFAttributes) is a different property
		//    that controls pipeline delay. CODECAPI_AVLowLatencyMode controls
		//    whether the encoder uses multi-frame or single-frame mode.
		//    VT_BOOL: VARIANT_TRUE = -1
		llv := comVariant{vt: vtBool, val: uint64(0xFFFF)} // VARIANT_TRUE
		if _, err := comCall(codecAPI, vtblCodecAPISetValue,
			uintptr(unsafe.Pointer(&codecAPIAVLowLatencyMode)),
			uintptr(unsafe.Pointer(&llv)),
		); err != nil {
			slog.Debug("ICodecAPI SetValue(AVLowLatencyMode) failed (non-fatal)", "error", err.Error())
		}

		// 4. Quality vs speed: 0 = fastest encoding, minimize per-frame latency.
		//    Higher values (up to 100) favor quality over speed.
		qvs := comVariant{vt: vtUI4, val: 0}
		if _, err := comCall(codecAPI, vtblCodecAPISetValue,
			uintptr(unsafe.Pointer(&codecAPIAVEncCommonQualityVsSpeed)),
			uintptr(unsafe.Pointer(&qvs)),
		); err != nil {
			slog.Debug("ICodecAPI SetValue(QualityVsSpeed=0) failed (non-fatal)", "error", err.Error())
		}
	} else {
		slog.Debug("ICodecAPI not available on this MFT (dynamic bitrate disabled)", "error", fmt.Sprintf("%v", qiErr))
	}

	// If streaming requested a keyframe before init, apply now (best-effort).
	if m.forceKeyframePending {
		_ = m.forceKeyframeLocked()
	}

	// NOTE: the DXGI device manager is installed earlier in this function
	// (tryInitGPUPipeline, before media-type negotiation) when zero-copy
	// DXGI-surface input is available. The earlier belief that hardware MFTs
	// "stall when fed DXGI surface samples (tested on Kit)" was actually the
	// async-MFT-driven-synchronously bug — with the event handshake
	// (encodeAsync/pumpEvents) surface input works. See
	// docs/remote-desktop-performance/findings.md, "Zero-copy DXGI input".

	hwStr := "software"
	if isHW {
		hwStr = "hardware"
	}
	slog.Info("MFT H264 encoder initialized",
		"type", hwStr,
		"width", width,
		"height", height,
		"bitrate", m.cfg.Bitrate,
		"fps", m.cfg.FPS,
		"rateControl", "cbr",
		"providesSamples", m.providesSamples,
		"outputBufSize", m.outputBufSize,
		"hasCodecAPI", m.codecAPI != 0,
		"gpuPipeline", m.gpuEnabled,
	)
	return nil
}

// abortFailedInitialization balances the COM/MF setup when a hardware
// candidate is rejected before m.inited becomes true. Without this, a failed
// high-resolution negotiation leaves the capture goroutine pinned and causes
// subsequent sessions to inherit incomplete Media Foundation state.
func (m *mftEncoder) abortFailedInitialization(transform uintptr, mfStarted bool) {
	if transform != 0 {
		comRelease(transform)
	}
	if m.dxgiManager != 0 {
		comRelease(m.dxgiManager)
		m.dxgiManager = 0
	}
	m.useDXGISamples = false
	if mfStarted {
		procMFShutdown.Call()
	}
	if m.comInitialized {
		procCoUninitialize.Call()
		m.comInitialized = false
	}
	// This helper is only called synchronously from initialize, on the same
	// capture goroutine that called LockOSThread.
	if m.threadLocked {
		runtime.UnlockOSThread()
	}
	m.threadLocked = false
}

// findAndConfigureHardwareEncoder enumerates every hardware activation and
// accepts one only after it has negotiated NV12 input and H264 output. MFTEnumEx
// registrations are advisory: hybrid-GPU machines frequently enumerate an
// encoder that belongs to an inactive adapter or that rejects the requested
// resolution. Keeping every attempt self-contained avoids poisoning the next
// candidate with an incomplete COM/DXGI configuration.
func (m *mftEncoder) findAndConfigureHardwareEncoder(width, height int) (uintptr, error) {
	inputType := mftRegisterTypeInfo{
		guidMajorType: mfMediaTypeVideo,
		guidSubtype:   mfVideoFormatNV12,
	}
	outputType := mftRegisterTypeInfo{
		guidMajorType: mfMediaTypeVideo,
		guidSubtype:   mfVideoFormatH264,
	}

	flags := uint32(mftEnumFlagHardware | mftEnumFlagSortAndFilter)
	if transform, err := m.configureEnumeratedHardwareCandidates(flags, nil, nil, width, height); err == nil {
		return transform, nil
	} else {
		slog.Warn("Untyped hardware MFT enumeration did not negotiate an encoder", "error", err.Error())
	}

	// Some drivers expose only typed registrations. This is deliberately a
	// second enumeration rather than a second configuration pass over a stale
	// transform, so each candidate starts in a clean state.
	transform, err := m.configureEnumeratedHardwareCandidates(flags, &inputType, &outputType, width, height)
	if err != nil {
		return 0, fmt.Errorf("no hardware H264 encoder negotiated (software encoding handled by OpenH264): %w", err)
	}
	return transform, nil
}

func (m *mftEncoder) configureEnumeratedHardwareCandidates(flags uint32, inputType, outputType *mftRegisterTypeInfo, width, height int) (uintptr, error) {
	ppActivate, count, hr := enumerateMFTActivationArray(flags, inputType, outputType)
	if int32(hr) < 0 || count == 0 {
		return 0, fmt.Errorf("MFTEnumEx found %d encoders (flags=0x%X, HRESULT=0x%08X)", count, flags, uint32(hr))
	}
	defer releaseMFTActivations(ppActivate, count)

	activations := unsafe.Slice((*uintptr)(unsafe.Pointer(ppActivate)), count)
	var lastErr error
	for index, activate := range activations {
		if activate == 0 {
			lastErr = fmt.Errorf("candidate %d has a nil activation", index)
			continue
		}

		var transform uintptr
		_, err := comCall(activate, vtblActivateObject,
			uintptr(unsafe.Pointer(&iidIMFTransform)),
			uintptr(unsafe.Pointer(&transform)),
		)
		if err != nil || transform == 0 {
			if err == nil {
				err = fmt.Errorf("ActivateObject returned a nil transform")
			}
			lastErr = err
			slog.Debug("MFT candidate activation rejected", "candidate", index, "candidates", count, "error", err.Error())
			continue
		}

		err = m.configureHardwareCandidate(transform, width, height)
		if err == nil {
			slog.Info("Hardware MFT candidate negotiated", "candidate", index, "candidates", count,
				"inputType", "NV12", "width", width, "height", height)
			return transform, nil
		}
		comRelease(transform)
		lastErr = err
		slog.Warn("Hardware MFT candidate rejected", "candidate", index, "candidates", count,
			"inputType", "NV12", "negotiationOrder", "input-then-output", "fallbackReason", err.Error())
	}

	return 0, fmt.Errorf("none of %d hardware MFT candidates negotiated: %w", count, lastErr)
}

func (m *mftEncoder) configureHardwareCandidate(transform uintptr, width, height int) error {
	// Hardware MFTs are async and must be unlocked before configuration.
	if err := m.unlockAsyncMFT(transform); err != nil {
		return fmt.Errorf("async unlock: %w", err)
	}
	if err := m.setInputType(transform, width, height); err != nil {
		return fmt.Errorf("set NV12 input type: %w", err)
	}
	if err := m.setNegotiatedOutputType(transform, width, height); err != nil {
		return fmt.Errorf("set H264 output type: %w", err)
	}
	return nil
}

func (m *mftEncoder) enumAndActivate(flags uint32, inputType, outputType *mftRegisterTypeInfo) (uintptr, error) {
	ppActivate, count, hr := enumerateMFTActivationArray(flags, inputType, outputType)
	if int32(hr) < 0 || count == 0 {
		return 0, fmt.Errorf("MFTEnumEx found %d encoders (flags=0x%X, HRESULT=0x%08X)", count, flags, uint32(hr))
	}

	// ppActivate is a pointer to an array of IMFActivate pointers. Enumeration
	// commonly includes transforms that are installed but not usable in the
	// current session (for example a discrete-GPU encoder while the active
	// display belongs to an iGPU). Activating only entry zero makes that stale
	// candidate hide every valid Intel/NVIDIA/AMD encoder following it.
	defer releaseMFTActivations(ppActivate, count)

	activations := unsafe.Slice((*uintptr)(unsafe.Pointer(ppActivate)), count)
	var lastErr error
	for index, activatePtr := range activations {
		if activatePtr == 0 {
			lastErr = fmt.Errorf("candidate %d has a nil activation", index)
			continue
		}

		// ActivateObject(IID_IMFTransform, &transform). An activated transform
		// owns its own COM reference, so releasing the activation array below
		// does not invalidate the selected transform.
		var transform uintptr
		_, err := comCall(activatePtr, vtblActivateObject,
			uintptr(unsafe.Pointer(&iidIMFTransform)),
			uintptr(unsafe.Pointer(&transform)),
		)
		if err == nil && transform != 0 {
			slog.Info("MFT candidate activated", "candidate", index, "candidates", count, "flags", fmt.Sprintf("0x%X", flags))
			return transform, nil
		}
		if err == nil {
			err = fmt.Errorf("ActivateObject returned a nil transform")
		}
		lastErr = err
		slog.Debug("MFT candidate activation rejected", "candidate", index, "candidates", count, "flags", fmt.Sprintf("0x%X", flags), "error", err.Error())
	}

	return 0, fmt.Errorf("could not activate any of %d MFT candidates (flags=0x%X): %w", count, flags, lastErr)
}

// enumerateMFTActivations returns an owned IMFActivate array. The caller must
// release it with releaseMFTActivations. Keeping the GUID and type-info values
// alive is required when calling a native API through uintptr: the compiler is
// otherwise free to consider those Go values dead before the DLL call returns.
func enumerateMFTActivationArray(flags uint32, inputType, outputType *mftRegisterTypeInfo) (uintptr, uint32, uintptr) {
	var ppActivate uintptr
	var count uint32

	hr, _, _ := procMFTEnumEx.Call(
		uintptr(unsafe.Pointer(&mftCategoryVideoEncoder)),
		uintptr(flags),
		uintptr(unsafe.Pointer(inputType)),
		uintptr(unsafe.Pointer(outputType)),
		uintptr(unsafe.Pointer(&ppActivate)),
		uintptr(unsafe.Pointer(&count)),
	)
	runtime.KeepAlive(mftCategoryVideoEncoder)
	runtime.KeepAlive(inputType)
	runtime.KeepAlive(outputType)
	runtime.KeepAlive(&ppActivate)
	runtime.KeepAlive(&count)
	return ppActivate, count, hr
}

func releaseMFTActivations(ppActivate uintptr, count uint32) {
	if ppActivate == 0 {
		return
	}
	activateArray := unsafe.Slice((*uintptr)(unsafe.Pointer(ppActivate)), count)
	for _, a := range activateArray {
		comRelease(a)
	}
	procCoTaskMemFree.Call(ppActivate)
}

// enumerateMFTActivations is a diagnostic-safe query that releases its result
// array immediately.
func enumerateMFTActivations(flags uint32, inputType, outputType *mftRegisterTypeInfo) (uint32, uintptr) {
	ppActivate, count, hr := enumerateMFTActivationArray(flags, inputType, outputType)
	if ppActivate != 0 {
		releaseMFTActivations(ppActivate, count)
	}
	return count, hr
}

// setNegotiatedOutputType prefers the H264 types advertised by the driver.
// Intel's hardware MFT rejects a blank, application-constructed output type
// on recent drivers even when every individual attribute looks valid. Starting
// with its advertised type preserves driver-owned profile and level choices.
func (m *mftEncoder) setNegotiatedOutputType(transform uintptr, width, height int) error {
	var lastErr error
	for index := uintptr(0); ; index++ {
		var mediaType uintptr
		_, err := comCall(transform, vtblGetOutputAvailType, 0, index, uintptr(unsafe.Pointer(&mediaType)))
		if err != nil {
			if index == 0 {
				lastErr = fmt.Errorf("GetOutputAvailableType[0]: %w", err)
			}
			break
		}
		if mediaType == 0 {
			lastErr = fmt.Errorf("GetOutputAvailableType[%d] returned nil", index)
			continue
		}

		err = m.configureOutputMediaType(mediaType, width, height)
		if err == nil {
			_, err = comCall(transform, vtblSetOutputType, 0, mediaType, 0)
		}
		comRelease(mediaType)
		if err == nil {
			slog.Info("MFT H264 output type negotiated", "outputTypeIndex", index,
				"outputSubtype", "H264", "negotiationOrder", "driver-advertised")
			return nil
		}
		lastErr = fmt.Errorf("driver output type %d: %w", index, err)
		slog.Debug("MFT H264 output type rejected", "outputTypeIndex", index,
			"outputSubtype", "H264", "hresult", err.Error())
	}

	// Some older MFTs do not expose output types before streaming. Retain a
	// generic H264 fallback, but deliberately do not force a profile: the
	// encoder selects the valid profile/level for this resolution.
	if err := m.setGenericOutputType(transform, width, height); err == nil {
		slog.Info("MFT H264 output type negotiated", "outputTypeIndex", "generic",
			"outputSubtype", "H264", "negotiationOrder", "generic-fallback")
		return nil
	} else if lastErr != nil {
		return fmt.Errorf("driver-advertised types rejected (%w); generic fallback: %w", lastErr, err)
	} else {
		return fmt.Errorf("generic fallback: %w", err)
	}
}

func (m *mftEncoder) setGenericOutputType(transform uintptr, width, height int) error {
	var mediaType uintptr
	hr, _, _ := procMFCreateMediaType.Call(uintptr(unsafe.Pointer(&mediaType)))
	if int32(hr) < 0 {
		return fmt.Errorf("MFCreateMediaType failed: 0x%08X", uint32(hr))
	}
	defer comRelease(mediaType)

	// Major type = Video
	if _, err := comCall(mediaType, vtblSetGUID,
		uintptr(unsafe.Pointer(&mfMTMajorType)),
		uintptr(unsafe.Pointer(&mfMediaTypeVideo)),
	); err != nil {
		return err
	}

	// Subtype = H264
	if _, err := comCall(mediaType, vtblSetGUID,
		uintptr(unsafe.Pointer(&mfMTSubtype)),
		uintptr(unsafe.Pointer(&mfVideoFormatH264)),
	); err != nil {
		return err
	}

	if err := m.configureOutputMediaType(mediaType, width, height); err != nil {
		return err
	}

	// Set on transform
	if _, err := comCall(transform, vtblSetOutputType,
		0, // stream ID
		mediaType,
		0, // flags
	); err != nil {
		return fmt.Errorf("SetOutputType: %w", err)
	}

	return nil
}

// configureOutputMediaType changes only stream properties required by the
// session. It intentionally leaves codec profile and level untouched: those
// are driver capabilities, and forcing Main profile can make Intel UHD reject
// an otherwise supported high-resolution stream.
func (m *mftEncoder) configureOutputMediaType(mediaType uintptr, width, height int) error {
	if _, err := comCall(mediaType, vtblSetUINT32,
		uintptr(unsafe.Pointer(&mfMTAvgBitrate)),
		uintptr(uint32(m.cfg.Bitrate)),
	); err != nil {
		return fmt.Errorf("set bitrate: %w", err)
	}
	if _, err := comCall(mediaType, vtblSetUINT32,
		uintptr(unsafe.Pointer(&mfMTInterlaceMode)),
		uintptr(uint32(mfVideoInterlaceProgressive)),
	); err != nil {
		return fmt.Errorf("set progressive interlace mode: %w", err)
	}
	if _, err := comCall(mediaType, vtblSetUINT64,
		uintptr(unsafe.Pointer(&mfMTFrameSize)),
		uintptr(pack64(uint32(width), uint32(height))),
	); err != nil {
		return fmt.Errorf("set frame size: %w", err)
	}
	fps := m.cfg.FPS
	if fps <= 0 {
		fps = 30
	}
	if _, err := comCall(mediaType, vtblSetUINT64,
		uintptr(unsafe.Pointer(&mfMTFrameRate)),
		uintptr(pack64(uint32(fps), 1)),
	); err != nil {
		return fmt.Errorf("set frame rate: %w", err)
	}
	if _, err := comCall(mediaType, vtblSetUINT64,
		uintptr(unsafe.Pointer(&mfMTPixelAspectRatio)),
		uintptr(pack64(1, 1)),
	); err != nil {
		return fmt.Errorf("set pixel aspect ratio: %w", err)
	}
	return nil
}

func (m *mftEncoder) setInputType(transform uintptr, width, height int) error {
	var mediaType uintptr
	hr, _, _ := procMFCreateMediaType.Call(uintptr(unsafe.Pointer(&mediaType)))
	if int32(hr) < 0 {
		return fmt.Errorf("MFCreateMediaType failed: 0x%08X", uint32(hr))
	}
	defer comRelease(mediaType)

	// Major type = Video
	if _, err := comCall(mediaType, vtblSetGUID,
		uintptr(unsafe.Pointer(&mfMTMajorType)),
		uintptr(unsafe.Pointer(&mfMediaTypeVideo)),
	); err != nil {
		return err
	}

	// Subtype = NV12
	if _, err := comCall(mediaType, vtblSetGUID,
		uintptr(unsafe.Pointer(&mfMTSubtype)),
		uintptr(unsafe.Pointer(&mfVideoFormatNV12)),
	); err != nil {
		return err
	}

	// Interlace = progressive
	if _, err := comCall(mediaType, vtblSetUINT32,
		uintptr(unsafe.Pointer(&mfMTInterlaceMode)),
		uintptr(uint32(mfVideoInterlaceProgressive)),
	); err != nil {
		return err
	}

	// Frame size
	frameSize := pack64(uint32(width), uint32(height))
	if _, err := comCall(mediaType, vtblSetUINT64,
		uintptr(unsafe.Pointer(&mfMTFrameSize)),
		uintptr(frameSize),
	); err != nil {
		return err
	}

	// Frame rate
	fps := m.cfg.FPS
	if fps <= 0 {
		fps = 30
	}
	frameRate := pack64(uint32(fps), 1)
	if _, err := comCall(mediaType, vtblSetUINT64,
		uintptr(unsafe.Pointer(&mfMTFrameRate)),
		uintptr(frameRate),
	); err != nil {
		return err
	}

	// Pixel aspect ratio
	par := pack64(1, 1)
	if _, err := comCall(mediaType, vtblSetUINT64,
		uintptr(unsafe.Pointer(&mfMTPixelAspectRatio)),
		uintptr(par),
	); err != nil {
		return err
	}

	// Default stride (NV12 Y plane stride = width).
	// Required by some hardware MFT encoders.
	if _, err := comCall(mediaType, vtblSetUINT32,
		uintptr(unsafe.Pointer(&mfMTDefaultStride)),
		uintptr(uint32(width)),
	); err != nil {
		return err
	}

	// Set on transform
	if _, err := comCall(transform, vtblSetInputType,
		0, // stream ID
		mediaType,
		0, // flags
	); err != nil {
		return fmt.Errorf("SetInputType: %w", err)
	}

	return nil
}

func (m *mftEncoder) setLowLatency(transform uintptr) {
	var attrs uintptr
	_, err := comCall(transform, vtblGetAttributes, uintptr(unsafe.Pointer(&attrs)))
	if err != nil || attrs == 0 {
		slog.Warn("MFT GetAttributes failed, cannot set low-latency", "error", fmt.Sprintf("%v", err))
		return
	}
	defer comRelease(attrs)
	_, err = comCall(attrs, vtblSetUINT32,
		uintptr(unsafe.Pointer(&mfLowLatency)),
		uintptr(uint32(1)),
	)
	if err != nil {
		slog.Warn("Failed to set MF_LOW_LATENCY", "error", err.Error())
	}
}

// unlockAsyncMFT sets MF_TRANSFORM_ASYNC_UNLOCK = TRUE on a hardware MFT.
// Hardware MFTs (NVENC, QuickSync, AMD VCE) are async and locked by default.
// Without unlocking, all configuration calls return MF_E_TRANSFORM_ASYNC_LOCKED.
func (m *mftEncoder) unlockAsyncMFT(transform uintptr) error {
	var attrs uintptr
	_, err := comCall(transform, vtblGetAttributes, uintptr(unsafe.Pointer(&attrs)))
	if err != nil || attrs == 0 {
		return fmt.Errorf("GetAttributes for async unlock: %w", err)
	}
	defer comRelease(attrs)

	_, err = comCall(attrs, vtblSetUINT32,
		uintptr(unsafe.Pointer(&mfTransformAsyncUnlock)),
		uintptr(uint32(1)), // TRUE
	)
	if err != nil {
		return fmt.Errorf("SetUINT32(MF_TRANSFORM_ASYNC_UNLOCK): %w", err)
	}
	slog.Info("Hardware MFT async unlock succeeded")
	return nil
}

// --- encoderBackend interface ---

func (m *mftEncoder) SetCodec(codec Codec) error {
	if codec != CodecH264 {
		return fmt.Errorf("%w: MFT encoder only supports H264, got %s", ErrInvalidCodec, codec)
	}
	return nil
}

func (m *mftEncoder) SetQuality(quality QualityPreset) error {
	m.mu.Lock()
	m.cfg.Quality = quality
	m.mu.Unlock()
	return nil
}

func (m *mftEncoder) SetBitrate(bitrate int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.Bitrate = bitrate

	if m.codecAPI == 0 || !m.inited {
		return nil
	}

	// Apply bitrate dynamically via ICodecAPI::SetValue(CODECAPI_AVEncCommonMeanBitRate, VT_UI4)
	v := comVariant{vt: vtUI4}
	v.val = uint64(uint32(bitrate))
	_, err := comCall(m.codecAPI, vtblCodecAPISetValue,
		uintptr(unsafe.Pointer(&codecAPIAVEncCommonMeanBitRate)),
		uintptr(unsafe.Pointer(&v)),
	)
	if err != nil {
		slog.Debug("ICodecAPI SetValue(bitrate) failed", "bitrate", bitrate, "error", err.Error())
		return nil // non-fatal: adaptive loop will keep trying
	}
	slog.Debug("Dynamic bitrate applied via ICodecAPI", "bitrate", bitrate)

	// Update VBV buffer to maintain 500ms ratio at new bitrate.
	// Without this, a bitrate reduction leaves the VBV oversized (allows
	// transient rate spikes, less severe than the inverse) but a bitrate
	// increase leaves it undersized (causes burst-starve).
	vbvSize := vbvSizeForBitrate(bitrate)
	vbv := comVariant{vt: vtUI4, val: uint64(vbvSize)}
	if _, err := comCall(m.codecAPI, vtblCodecAPISetValue,
		uintptr(unsafe.Pointer(&codecAPIAVEncCommonBufferSize)),
		uintptr(unsafe.Pointer(&vbv)),
	); err != nil {
		slog.Warn("ICodecAPI SetValue(BufferSize) failed during bitrate update — encoder VBV/bitrate mismatch",
			"vbvSize", vbvSize, "bitrate", bitrate, "error", err.Error())
	}

	return nil
}

func (m *mftEncoder) SetPixelFormat(pf PixelFormat) {
	m.mu.Lock()
	m.pixelFormat = pf
	m.mu.Unlock()
}

func (m *mftEncoder) SetFPS(fps int) error {
	m.mu.Lock()
	m.cfg.FPS = fps
	m.mu.Unlock()
	return nil
}

func (m *mftEncoder) SetDimensions(w, h int) error {
	// NV12 requires even dimensions; H264 macroblocks prefer multiples of 16.
	// Round down to even to avoid MF_E_INVALIDMEDIATYPE from SetInputType.
	w = w &^ 1
	h = h &^ 1
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inited && (m.width != w || m.height != h) {
		// Resolution changed — need to reinitialize
		m.shutdown()
	}
	m.width = w
	m.height = h
	m.stride = w * 4
	// Eagerly initialize the MFT so BackendIsHardware() is accurate
	// before the first Encode() call. This eliminates the blind spot where
	// the startup stall guard checks IsHardware() but gets false because
	// lazy init hasn't run yet.
	if !m.inited && m.width > 0 && m.height > 0 {
		if err := m.initialize(m.width, m.height, m.stride); err != nil {
			slog.Warn("Eager MFT initialization failed; caller must select the software fallback",
				"error", err.Error(), "width", m.width, "height", m.height)
			return err
		}
	}
	return nil
}

func (m *mftEncoder) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutdown()
	return nil
}

func (m *mftEncoder) shutdown() {
	if !m.inited {
		return
	}
	// Release GPU converter first
	if m.gpuConv != nil {
		m.gpuConv.Close()
		m.gpuConv = nil
	}
	m.gpuFrameCount = 0
	m.gpuEnabled = false
	m.gpuFailed = false
	m.forceKeyframePending = false

	// Release DXGI device manager
	if m.dxgiManager != 0 {
		comRelease(m.dxgiManager)
		m.dxgiManager = 0
	}
	m.useDXGISamples = false

	// Release the async event generator before the transform
	if m.eventGen != 0 {
		comRelease(m.eventGen)
		m.eventGen = 0
	}
	m.asyncMode = false
	m.needInputCredits = 0
	m.pendingOutput = nil

	// Release ICodecAPI before the transform
	if m.codecAPI != 0 {
		comRelease(m.codecAPI)
		m.codecAPI = 0
	}
	// Flush
	comCall(m.transform, vtblProcessMessage, mftMessageCommandFlush, 0)
	comCall(m.transform, vtblProcessMessage, mftMessageNotifyEndStreaming, 0)
	comRelease(m.transform)
	m.transform = 0
	m.inited = false
	m.frameIdx = 0
	m.startTime = time.Now()

	procMFShutdown.Call()
	if m.comInitialized {
		procCoUninitialize.Call()
		m.comInitialized = false
	}

	// NOTE: We intentionally do NOT call runtime.UnlockOSThread() here.
	// LockOSThread was called from the capture goroutine via Encode→initialize.
	// shutdown() may be called from a different goroutine (e.g., Session.Stop).
	// Calling UnlockOSThread from the wrong goroutine would unlock that goroutine's
	// thread instead. The locked thread is released when the capture goroutine exits.
	m.threadLocked = false

	slog.Info("MFT H264 encoder shut down")
}

func (m *mftEncoder) Name() string {
	if m.isHW {
		return "mft-hardware"
	}
	return "mft-software"
}

func (m *mftEncoder) IsHardware() bool {
	return m.isHW
}

func (m *mftEncoder) IsPlaceholder() bool {
	return false
}

func (m *mftEncoder) IsPermanentlyStalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.permanentlyStalled
}

// AdvanceStallDetection progresses the stall state machine during idle periods
// when no Encode() calls happen. If the encoder has pending nil outputs and
// enough time has passed since the last flush attempt, this triggers the same
// flush/permanent-stall logic that trackNilOutput uses.
func (m *mftEncoder) AdvanceStallDetection() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.permanentlyStalled || !m.inited || m.consecutiveNilOutputs == 0 {
		return
	}
	// Use the same threshold and timing logic as trackNilOutput, but
	// trigger based on the existing counter that froze when encoding stopped.
	threshold := mftStallThreshold
	if m.lastStallFlush != (time.Time{}) && time.Since(m.lastStallFlush) < 10*time.Second {
		threshold = mftStallThreshold / 2
	}
	if m.consecutiveNilOutputs >= threshold && time.Since(m.lastStallFlush) >= 2*time.Second {
		if !m.outputSinceFlush && m.stallFlushCount > 0 {
			m.stallFlushCount++
		} else {
			m.stallFlushCount = 1
		}
		m.outputSinceFlush = false

		if m.stallFlushCount >= 2 {
			if m.useDXGISamples {
				// Same downgrade as trackNilOutput: a stall on the zero-copy
				// input path reverts to readback before declaring death.
				slog.Warn("MFT stalling with zero-copy input during idle, downgrading to readback path",
					"stallFlushCount", m.stallFlushCount, "consecutiveNil", m.consecutiveNilOutputs)
				m.teardownDXGIManager()
				m.consecutiveNilOutputs = 0
				m.stallFlushCount = 0
				m.outputSinceFlush = false
				m.lastStallFlush = time.Now()
				m.forceKeyframePending = true
				return
			}
			slog.Error("MFT encoder permanently stalled during idle — flush recovery not working",
				"stallFlushCount", m.stallFlushCount,
				"consecutiveNil", m.consecutiveNilOutputs,
				"isHW", m.isHW,
			)
			m.permanentlyStalled = true
			return
		}

		slog.Warn("MFT encoder stalled during idle, flushing pipeline to recover",
			"consecutiveNil", m.consecutiveNilOutputs,
			"isHW", m.isHW,
			"stallFlushCount", m.stallFlushCount,
		)
		m.flushLocked()
		m.consecutiveNilOutputs = 0
		m.lastStallFlush = time.Now()
	}
}
