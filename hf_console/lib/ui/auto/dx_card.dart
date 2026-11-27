/// The DX card of the 75AUTO page: the station that 75auto works now, in
/// the right column.
///
/// From top to bottom: the call with the DXCC entity and the try or repeat
/// count, the QSO stage chips, the QRZ picture with the operator name and
/// the distance, and at the bottom the proposal strip.
///
/// - The proposal strip shows only while a proposal waits: its text, the
///   confirm countdown, and Approve and Reject.
/// - A latched stop shows "LATCHED · resume at the station" in red there,
///   with the stop reason.
/// - Without a DX call, the card says what 75auto does now, or why the
///   rules propose nothing.
///
/// The picture is a network image from the URL that 75auto has from the
/// callbook. While it loads, or when it fails, the place stays empty. The
/// web build shows it as an <img> element when the host sends no CORS
/// headers.
library;

import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../store/auto_op.dart';
import '../../store/bus_store.dart';
import '../theme.dart';
import 'auto_card.dart';
import 'auto_readouts.dart';
import 'next_action_card.dart';

/// The size of the QRZ picture.
const autoDxImageSize = 72.0;

class DxCard extends StatelessWidget {
  const DxCard({super.key});

  @override
  Widget build(BuildContext context) {
    final view = context.select<BusStore, AutoOpView?>((s) => s.autoOp);
    final st = view?.station;
    final call = st?.dxCall;
    final hasDx = call != null && call.isNotEmpty;
    final pending = view?.latched == true ? null : view?.pending;

    final title = <String>['DX call'];
    final entity = st?.dxEntity;
    if (hasDx && entity != null && entity.isNotEmpty) title.add(entity);
    final rpt = st?.repeatCount ?? 0;
    final tries = st?.tryCount ?? 0;
    if (hasDx && rpt > 0) {
      title.add('rpt $rpt');
    } else if (hasDx && tries > 0) {
      title.add('try $tries');
    }

    final String value;
    final Color color;
    if (call == null) {
      value = '?';
      color = AppTheme.txtMute;
    } else if (call.isEmpty) {
      value = 'no DX';
      color = AppTheme.txtFaint;
    } else {
      value = call;
      color = AppTheme.accent;
    }
    final stage = st?.qsoStage;
    final now = stage == null ? -1 : qsoStages.indexOf(stage);
    final t = nextActionText(view);

    return AutoCard(
      key: const ValueKey('auto-dx-card'),
      title: title.join(' · '),
      trailing: pending == null ? null : AutoCountdown(pending: pending),
      child: Padding(
        padding: const EdgeInsets.fromLTRB(8, 4, 8, 6),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            FittedBox(
              fit: BoxFit.scaleDown,
              alignment: Alignment.centerLeft,
              child: Text(value,
                  key: const ValueKey('auto-dx-call'),
                  maxLines: 1,
                  style: AppTheme.mono(26, color: color, weight: FontWeight.w700).copyWith(height: 1.1)),
            ),
            const SizedBox(height: 3),
            FittedBox(
              fit: BoxFit.scaleDown,
              alignment: Alignment.centerLeft,
              child: Row(children: [
                for (var i = 0; i < qsoStages.length; i++) ...[
                  AutoStageChip(qsoStages[i], done: hasDx && now >= 0 && i < now, current: hasDx && i == now),
                  const SizedBox(width: 3),
                ],
              ]),
            ),
            const SizedBox(height: 6),
            Expanded(
              child: hasDx
                  ? _Station(name: st?.dxName, image: st?.dxImage, km: st?.dxKm)
                  // The strip below has the text of a proposal or a latch.
                  : (view?.latched == true || pending != null ? const SizedBox.shrink() : _Doing(text: t)),
            ),
            if (view?.latched == true)
              _Latched(text: t)
            else if (pending != null)
              _ProposalStrip(text: t, pending: pending),
          ],
        ),
      ),
    );
  }
}

/// The QRZ picture, the operator name, and the distance.
class _Station extends StatelessWidget {
  final String? name;
  final String? image;
  final int? km;

  const _Station({required this.name, required this.image, required this.km});

  @override
  Widget build(BuildContext context) {
    final url = image ?? '';
    final hasImage = url.startsWith('https://') || url.startsWith('http://');
    final kmText = km == null || km! <= 0 ? null : '${_thousands(km!)} km';
    return LayoutBuilder(builder: (context, c) {
      final size = c.maxHeight.clamp(0.0, autoDxImageSize);
      return Row(crossAxisAlignment: CrossAxisAlignment.start, children: [
        if (hasImage && size >= 32) ...[
          ClipRRect(
            borderRadius: BorderRadius.circular(3),
            child: Image.network(
              url,
              key: const ValueKey('auto-dx-image'),
              width: size,
              height: size,
              fit: BoxFit.cover,
              // The web build fetches under CORS. Without CORS headers on
              // the picture host it falls back to an <img> element.
              webHtmlElementStrategy: WebHtmlElementStrategy.fallback,
              errorBuilder: (_, _, _) => SizedBox(width: size, height: size),
            ),
          ),
          const SizedBox(width: 8),
        ],
        Expanded(
          // Clipped, not overflowing, when a banner leaves little height.
          child: SingleChildScrollView(
            physics: const NeverScrollableScrollPhysics(),
            child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
            if ((name ?? '').isNotEmpty)
              Text(name!,
                  key: const ValueKey('auto-dx-name'),
                  maxLines: 2,
                  overflow: TextOverflow.ellipsis,
                  style: AppTheme.body(14, weight: FontWeight.w600).copyWith(height: 1.2)),
            if (kmText != null)
              Text(kmText,
                  key: const ValueKey('auto-dx-km'),
                  maxLines: 1,
                  style: AppTheme.mono(13, color: AppTheme.txtMute, weight: FontWeight.w600).copyWith(height: 1.4)),
          ]),
          ),
        ),
      ]);
    });
  }
}

/// Without a DX call: what 75auto does now, or why the rules propose
/// nothing.
class _Doing extends StatelessWidget {
  final NextActionText text;

  const _Doing({required this.text});

  @override
  Widget build(BuildContext context) {
    return LayoutBuilder(builder: (context, c) {
      final lines = text.why.isEmpty ? 0 : ((c.maxHeight - 22) / 14).floor().clamp(0, 4);
      return Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
        Text(text.what,
            key: const ValueKey('auto-next-what'),
            maxLines: 1,
            overflow: TextOverflow.ellipsis,
            style: AppTheme.display(17, color: text.whatColor, weight: FontWeight.w700, letterSpacing: 0.04)
                .copyWith(height: 1.2)),
        if (lines > 0)
          Text(text.why,
              key: const ValueKey('auto-next-why'),
              maxLines: lines,
              overflow: TextOverflow.ellipsis,
              style: AppTheme.body(11, color: AppTheme.txtMute).copyWith(height: 1.2)),
      ]);
    });
  }
}

/// A pending proposal: its text, then Approve and Reject.
class _ProposalStrip extends StatelessWidget {
  final NextActionText text;
  final AutoOpPending pending;

  const _ProposalStrip({required this.text, required this.pending});

  @override
  Widget build(BuildContext context) {
    return Container(
      key: const ValueKey('auto-proposal'),
      padding: const EdgeInsets.only(top: 4),
      decoration: BoxDecoration(border: Border(top: BorderSide(color: AppTheme.cardLine))),
      child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, mainAxisSize: MainAxisSize.min, children: [
        Text(text.what,
            key: const ValueKey('auto-next-what'),
            maxLines: 1,
            overflow: TextOverflow.ellipsis,
            style: AppTheme.display(17, color: text.whatColor, weight: FontWeight.w700, letterSpacing: 0.04)
                .copyWith(height: 1.2)),
        if (text.why.isNotEmpty)
          Text(text.why,
              key: const ValueKey('auto-next-why'),
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
              style: AppTheme.body(11, color: AppTheme.txtMute).copyWith(height: 1.2)),
        const SizedBox(height: 4),
        ProposalButtons(pending: pending),
      ]),
    );
  }
}

/// A latched stop: the latch text and the stop reason, in red.
class _Latched extends StatelessWidget {
  final NextActionText text;

  const _Latched({required this.text});

  @override
  Widget build(BuildContext context) {
    return Container(
      padding: const EdgeInsets.only(top: 4),
      decoration: BoxDecoration(border: Border(top: BorderSide(color: AppTheme.blend(AppTheme.red, 0.6)))),
      child: Column(crossAxisAlignment: CrossAxisAlignment.stretch, mainAxisSize: MainAxisSize.min, children: [
        Text(text.what,
            key: const ValueKey('auto-next-what'),
            maxLines: 1,
            overflow: TextOverflow.ellipsis,
            style: AppTheme.display(15, color: AppTheme.red, weight: FontWeight.w700).copyWith(height: 1.2)),
        if (text.why.isNotEmpty)
          Text(text.why,
              key: const ValueKey('auto-next-why'),
              maxLines: 2,
              overflow: TextOverflow.ellipsis,
              style: AppTheme.body(11, color: AppTheme.txtMute).copyWith(height: 1.2)),
      ]),
    );
  }
}

/// 11234 → "11 234".
String _thousands(int n) {
  final s = n.toString();
  final b = StringBuffer();
  for (var i = 0; i < s.length; i++) {
    if (i > 0 && (s.length - i) % 3 == 0) b.write(' ');
    b.write(s[i]);
  }
  return b.toString();
}
