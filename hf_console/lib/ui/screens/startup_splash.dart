import 'dart:async';

import 'package:flutter/material.dart';

import '../theme.dart';

/// Launcher icon asset (same source flutter_launcher_icons uses).
const appIconAsset = 'assets/Gemini_Generated_Image_n0qlmmn0qlmmn0ql.jpg';

/// Boot splash: app icon, title, and — while the first broker connect is in
/// flight — the target address plus a determinate progress bar counting down
/// the wait budget, so a unreachable broker never reads as a hang. When the
/// budget expires, [onWaitExpired] hands over to the console (whose offline
/// indicator takes over) while the service keeps retrying in the background.
class StartupSplash extends StatefulWidget {
  final String? host;
  final int? port;

  /// Total wait budget in seconds; null disables the countdown/expiry.
  final int? waitSeconds;
  final VoidCallback onWaitExpired;

  const StartupSplash({
    super.key,
    this.host,
    this.port,
    required this.waitSeconds,
    required this.onWaitExpired,
  });

  @override
  State<StartupSplash> createState() => _StartupSplashState();
}

class _StartupSplashState extends State<StartupSplash> {
  Timer? _tick;
  int _elapsed = 0;
  bool _expired = false;

  bool get _connecting => widget.host != null;

  @override
  void initState() {
    super.initState();
    _tick = Timer.periodic(const Duration(seconds: 1), (_) {
      if (!mounted || _expired) return;
      setState(() => _elapsed++);
      final wait = widget.waitSeconds;
      if (wait != null && _elapsed >= wait) {
        _expired = true;
        widget.onWaitExpired();
      }
    });
  }

  @override
  void dispose() {
    _tick?.cancel();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final wait = widget.waitSeconds;
    final remaining = wait == null ? null : (wait - _elapsed).clamp(0, wait);
    final progress = wait == null ? null : (_elapsed / wait).clamp(0.0, 1.0);

    return Scaffold(
      backgroundColor: AppTheme.page,
      body: Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: [
            ClipRRect(
              borderRadius: BorderRadius.circular(22),
              child: Image.asset(
                appIconAsset,
                width: 108,
                height: 108,
                fit: BoxFit.cover,
                errorBuilder: (_, __, ___) => Container(
                  width: 108,
                  height: 108,
                  decoration: BoxDecoration(
                    color: AppTheme.pane,
                    border: Border.all(color: AppTheme.cardLine),
                    borderRadius: BorderRadius.circular(22),
                  ),
                  child: Icon(Icons.sensors, size: 48, color: AppTheme.accent),
                ),
              ),
            ),
            const SizedBox(height: 20),
            Text('Mühle HF', style: AppTheme.display(26)),
            const SizedBox(height: 10),
            Text(
              _connecting ? 'connecting to ${widget.host}:${widget.port}' : 'starting',
              style: AppTheme.mono(13, color: AppTheme.txtMute),
            ),
            const SizedBox(height: 16),
            SizedBox(
              width: 240,
              child: wait == null
                  ? LinearProgressIndicator(
                      minHeight: 4,
                      color: AppTheme.accent,
                      backgroundColor: AppTheme.pane,
                      borderRadius: BorderRadius.circular(2),
                    )
                  : LinearProgressIndicator(
                      value: progress,
                      minHeight: 4,
                      color: AppTheme.accent,
                      backgroundColor: AppTheme.pane,
                      borderRadius: BorderRadius.circular(2),
                    ),
            ),
            const SizedBox(height: 10),
            if (remaining != null)
              Text(
                'broker wait ${remaining}s',
                style: AppTheme.mono(12, color: AppTheme.txtFaint),
              ),
          ],
        ),
      ),
    );
  }
}