// cam_feed_panel.dart — the antenna camera feed (vhfcam-restream preview).
//
// The camera itself is a UniFi Protect RTSPS source; vhfcam-restream on shari
// transcodes it (with the AZ/EL/freq/TX overlay burned in server-side) into a
// live HLS playlist served from its :8083 preview server:
//
//   http://<cam base>/hls/live.m3u8     (cleartext HTTP, LAN-only, no auth)
//
// The service is installed disabled-at-boot and started ad hoc, so "offline"
// is a normal operating state, not a fault — this panel renders it as one and
// re-probes via [VhfcamService]. Latency is ~10 s behind live by design
// (HLS segmenting), which the header chip does not compensate for.
//
// video_player drives the native HLS stack (ExoPlayer on Android, AVPlayer on
// macOS — the macOS Runner Info.plist carries NSAllowsLocalNetworking for the
// cleartext URL). The web build cannot decode MPEG-TS HLS, so it gets a
// placeholder pointing at the :8083 reference page. Audio: the radio PCM rides
// in the stream's audio track (the server replaces camera audio); the feed
// therefore starts MUTED and the speaker toggle is purely local — the same
// posture as the :8083 page's mute button. mixWithOthers keeps the feed from
// claiming the platform audio session over anything else the console plays.
//
// AZ/EL/freq chips read the same bus slots the server's drawtext overlay
// does (muehle/uhf/az-rotator, muehle/uhf/el-rotator, muehle/uhf/radio) —
// crisper than the burned-in text and theme-aware, and they survive the video
// being offline.

import 'package:flutter/foundation.dart' show kIsWeb;
import 'package:flutter/material.dart';
import 'package:provider/provider.dart';
import 'package:video_player/video_player.dart';

import '../../store/bus_store.dart';
import '../../vhfcam/vhfcam_service.dart';
import '../theme.dart';
import 'cam_record_controls.dart';

class CamFeedPanel extends StatefulWidget {
  const CamFeedPanel({super.key});

  @override
  State<CamFeedPanel> createState() => _CamFeedPanelState();
}

enum _FeedState { web, serverOffline, sinkStopped, connecting, live }

class _CamFeedPanelState extends State<CamFeedPanel> {
  VideoPlayerController? _controller;

  /// Set only after a successful [VideoPlayerController.initialize] — the
  /// VideoPlayer widget asserts on an uninitialized controller, so "created"
  /// and "playable" are different states here.
  bool _playerReady = false;
  String? _controllerUrl;
  bool _muted = true;
  bool _disposed = false;

  @override
  void initState() {
    super.initState();
    // The service may already report a live stream when the page first
    // mounts (poll runs app-wide).
    WidgetsBinding.instance.addPostFrameCallback((_) => _sync());
  }

  @override
  void dispose() {
    _disposed = true;
    _controller?.removeListener(_onPlayerTick);
    _controller?.dispose();
    _controller = null;
    super.dispose();
  }

  /// Creates/tears down the player to match the service's view of the world.
  /// Called after every service notification (build) — all state transitions
  /// are guarded, and the async parts re-enter via setState when done.
  Future<void> _sync() async {
    final svc = context.read<VhfcamService>();
    final wantPlayer = !kIsWeb && svc.serverOnline && svc.streamLive;
    if (!wantPlayer) {
      if (_controller != null) {
        await _teardown();
        if (mounted) setState(() {});
      }
      return;
    }
    if (_controller != null && _controllerUrl == svc.hlsUrl) return;
    await _teardown();
    final url = svc.hlsUrl;
    final c = VideoPlayerController.networkUrl(
      Uri.parse(url),
      videoPlayerOptions: VideoPlayerOptions(mixWithOthers: true),
    );
    _controller = c;
    _controllerUrl = url;
    c.addListener(_onPlayerTick);
    try {
      await c.initialize();
      await c.setVolume(_muted ? 0.0 : 1.0);
      await c.play();
      _playerReady = true;
    } catch (_) {
      // Init failed (sink died between probe and play). Drop the controller;
      // the service's probe cycle will re-trigger creation.
      await _teardown();
      svc.probeNow();
    }
    if (mounted && !_disposed) setState(() {});
  }

  Future<void> _teardown() async {
    final c = _controller;
    _controller = null;
    _controllerUrl = null;
    _playerReady = false;
    if (c == null) return;
    c.removeListener(_onPlayerTick);
    try {
      await c.dispose();
    } catch (_) {
      // already disposed by the platform layer
    }
  }

  /// Player-level failure detector: a live HLS session dies when the sink
  /// stops between probes (segments 404). Tear down immediately rather than
  /// showing a frozen last frame, and ask the service to re-probe now — its
  /// rate-limited cycle turns that into at most one rebuild attempt per
  /// probe interval.
  void _onPlayerTick() {
    final c = _controller;
    if (c == null || !c.value.hasError) return;
    _teardown().then((_) {
      if (mounted && !_disposed) setState(() {});
    });
    context.read<VhfcamService>().probeNow();
  }

  void _toggleMute() {
    setState(() => _muted = !_muted);
    _controller?.setVolume(_muted ? 0.0 : 1.0);
  }

  _FeedState _stateFor(VhfcamService svc) {
    if (kIsWeb) return _FeedState.web;
    if (!svc.serverOnline) return _FeedState.serverOffline;
    if (_playerReady) return _FeedState.live;
    if (!svc.streamLive) return _FeedState.sinkStopped;
    return _FeedState.connecting;
  }

  @override
  Widget build(BuildContext context) {
    final svc = context.watch<VhfcamService>();
    // Fire-and-forget: _sync guards itself and re-enters via setState.
    _sync();
    final state = _stateFor(svc);
    final store = context.watch<BusStore>();

    final az = store.stateValueAs<double>('muehle/uhf/az-rotator', 'az');
    final el = store.stateValueAs<double>('muehle/uhf/el-rotator', 'el');
    final freqHz = store.stateValueAs<int>('muehle/uhf/radio', 'freq_hz');

    return Container(
      decoration: BoxDecoration(
        color: AppTheme.card,
        border: Border(top: BorderSide(color: AppTheme.cardLine), bottom: BorderSide(color: AppTheme.cardLine)),
      ),
      padding: const EdgeInsets.fromLTRB(12, 10, 12, 10),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        mainAxisSize: MainAxisSize.min,
        children: [
          Row(
            children: [
              Text('ANTENNA CAM', style: AppTheme.mono(12, weight: FontWeight.w700, letterSpacing: 0.14, color: AppTheme.txtMute)),
              const Spacer(),
              if (svc.status?.rec case final rec?) ...[
                RecBadge(rec: rec),
                const SizedBox(width: 6),
              ],
              _StatusChip(state: state),
              if (state == _FeedState.live) ...[
                const SizedBox(width: 8),
                _MuteButton(muted: _muted, onTap: _toggleMute),
              ],
            ],
          ),
          const SizedBox(height: 8),
          AspectRatio(
            aspectRatio: 16 / 9,
            child: Container(
              color: AppTheme.pane,
              child: switch (state) {
                _FeedState.live => ClipRect(child: VideoPlayer(_controller!)),
                _FeedState.connecting => Center(
                    child: SizedBox(
                      width: 28,
                      height: 28,
                      child: CircularProgressIndicator(color: AppTheme.accent, strokeWidth: 2.5),
                    ),
                  ),
                _ => _OfflineState(state: state, baseUrl: svc.baseUrl),
              },
            ),
          ),
          const SizedBox(height: 8),
          Wrap(
            spacing: 6,
            runSpacing: 6,
            children: [
              _ReadoutChip(label: 'AZ', value: az == null ? null : '${az.toStringAsFixed(1)}°'),
              _ReadoutChip(label: 'EL', value: el == null ? null : '${el.toStringAsFixed(1)}°'),
              _ReadoutChip(label: 'FREQ', value: freqHz == null ? null : '${(freqHz / 1e6).toStringAsFixed(3)} MHz'),
            ],
          ),
        ],
      ),
    );
  }
}

class _StatusChip extends StatelessWidget {
  final _FeedState state;

  const _StatusChip({required this.state});

  @override
  Widget build(BuildContext context) {
    final (text, color) = switch (state) {
      _FeedState.live => ('LIVE', AppTheme.green),
      _FeedState.connecting => ('CONNECTING', AppTheme.amber),
      _FeedState.sinkStopped => ('STREAM OFF', AppTheme.amber),
      _FeedState.serverOffline => ('CAM OFFLINE', AppTheme.red),
      _FeedState.web => ('WEB', AppTheme.txtMute),
    };
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 3),
      decoration: BoxDecoration(
        color: AppTheme.blend(color, 0.12),
        border: Border.all(color: AppTheme.blend(color, 0.45)),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Text('● $text', style: AppTheme.mono(10, color: color, weight: FontWeight.w600, letterSpacing: 0.08)),
    );
  }
}

class _MuteButton extends StatelessWidget {
  final bool muted;
  final VoidCallback onTap;

  const _MuteButton({required this.muted, required this.onTap});

  @override
  Widget build(BuildContext context) {
    return InkWell(
      borderRadius: BorderRadius.circular(4),
      onTap: onTap,
      child: SizedBox(
        width: 40,
        height: 32,
        child: Icon(muted ? Icons.volume_off : Icons.volume_up, size: 20, color: AppTheme.txt),
      ),
    );
  }
}

class _OfflineState extends StatelessWidget {
  final _FeedState state;
  final String baseUrl;

  const _OfflineState({required this.state, required this.baseUrl});

  @override
  Widget build(BuildContext context) {
    final (title, sub) = switch (state) {
      _FeedState.serverOffline => (
          'CAM SERVER OFFLINE',
          'no answer from $baseUrl — start it on shari: sudo systemctl start vhfcam-restream',
        ),
      _FeedState.sinkStopped => (
          'CAMERA STREAM STOPPED',
          'the cam server answers but sends no video — start it on shari: sudo systemctl start vhfcam-restream',
        ),
      _FeedState.web => (
          'VIDEO NEEDS THE APP',
          'the web build cannot decode the HLS feed — open $baseUrl',
        ),
      _ => ('OFFLINE', ''), // unreachable; the live/connecting states go elsewhere
    };
    return Padding(
      padding: const EdgeInsets.all(16),
      child: Column(
        mainAxisAlignment: MainAxisAlignment.center,
        children: [
          Text(title, textAlign: TextAlign.center, style: AppTheme.mono(13, weight: FontWeight.w700, color: AppTheme.txtMute, letterSpacing: 0.1)),
          if (sub.isNotEmpty) ...[
            const SizedBox(height: 6),
            Text(sub, textAlign: TextAlign.center, style: AppTheme.mono(10, color: AppTheme.txtFaint)),
          ],
          if (state == _FeedState.serverOffline || state == _FeedState.sinkStopped) ...[
            const SizedBox(height: 12),
            TextButton.icon(
              onPressed: () => context.read<VhfcamService>().probeNow(),
              icon: const Icon(Icons.refresh, size: 16),
              label: Text('RETRY', style: AppTheme.mono(11, weight: FontWeight.w700)),
              style: TextButton.styleFrom(
                foregroundColor: AppTheme.txt,
                minimumSize: const Size(64, 48),
              ),
            ),
          ],
        ],
      ),
    );
  }
}

class _ReadoutChip extends StatelessWidget {
  final String label;
  final String? value;

  const _ReadoutChip({required this.label, this.value});

  @override
  Widget build(BuildContext context) {
    // Same translucent-chip idiom as the map chrome — the readouts stay
    // legible over both the pane and the (theme-dependent) video card.
    return Container(
      padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 3),
      decoration: BoxDecoration(
        color: AppTheme.pane,
        border: Border.all(color: AppTheme.cardLine),
        borderRadius: BorderRadius.circular(4),
      ),
      child: Text(
        value == null ? '$label ---' : '$label $value',
        style: AppTheme.mono(11, color: value == null ? AppTheme.txtFaint : AppTheme.txtMute, weight: FontWeight.w600),
      ),
    );
  }
}
