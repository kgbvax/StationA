/// The DX card of the 75AUTO page: the station that 75auto works now, in
/// the right column.
///
/// From top to bottom: the call with the try or repeat count in the title,
/// the DXCC entity under the call, the QSO stage chips, the QRZ picture across the card with the
/// operator name and the distance under it, and at the bottom the
/// proposal strip.
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

import 'package:cached_network_image/cached_network_image.dart';
import 'package:flutter/foundation.dart' show kIsWeb;
import 'package:flutter/material.dart';
import 'package:provider/provider.dart';

import '../../store/auto_op.dart';
import '../../store/bus_store.dart';
import '../theme.dart';
import 'auto_card.dart';
import 'auto_live.dart';
import 'auto_readouts.dart';
import 'next_action_card.dart';

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
    final hasEntity = hasDx && entity != null && entity.isNotEmpty;
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
              child: Text(
                value,
                key: const ValueKey('auto-dx-call'),
                maxLines: 1,
                style: AppTheme.mono(
                  26,
                  color: color,
                  weight: FontWeight.w700,
                ).copyWith(height: 1.1),
              ),
            ),
            if (hasEntity)
              Text(
                entity,
                key: const ValueKey('auto-dx-entity'),
                maxLines: 1,
                overflow: TextOverflow.ellipsis,
                style: AppTheme.body(
                  14,
                  color: AppTheme.txtMute,
                  weight: FontWeight.w600,
                ).copyWith(height: 1.25),
              ),
            const SizedBox(height: 3),
            _StageChips(call: hasDx ? call : null, stage: st?.qsoStage),
            const SizedBox(height: 6),
            Expanded(
              child: hasDx
                  ? _Station(name: st?.dxName, image: st?.dxImage, km: st?.dxKm)
                  // The strip below has the text of a proposal or a latch.
                  : (view?.latched == true || pending != null
                        ? const SizedBox.shrink()
                        : _Doing(text: t)),
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

/// The QSO stage chips. A chip is green when its stage was seen for the
/// current DX call, and lit for the stage now. 75auto names the stage of
/// the latest message only, so a stage that the QSO skips (RPT when we
/// answer a CQ) stays dark instead of green. LOG is the end, so it shows
/// green. The chips fade while the data is not fresh: a retained stage of
/// a session that has stopped is not a live stage.
class _StageChips extends StatefulWidget {
  final String? call;
  final String? stage;

  const _StageChips({required this.call, required this.stage});

  @override
  State<_StageChips> createState() => _StageChipsState();
}

class _StageChipsState extends State<_StageChips> {
  final _seen = <int>{};
  String? _seenFor;

  @override
  Widget build(BuildContext context) {
    if (widget.call != _seenFor) {
      _seen.clear();
      _seenFor = widget.call;
    }
    final now = widget.call == null || widget.stage == null
        ? -1
        : qsoStages.indexOf(widget.stage!);
    if (now >= 0) _seen.add(now);
    return AutoTicker(
      period: const Duration(seconds: 1),
      builder: (context, live) {
        final row = FittedBox(
          fit: BoxFit.scaleDown,
          alignment: Alignment.centerLeft,
          child: Row(
            children: [
              for (var i = 0; i < qsoStages.length; i++) ...[
                AutoStageChip(
                  qsoStages[i],
                  done:
                      _seen.contains(i) && (i != now || qsoStages[i] == 'LOG'),
                  current: i == now && qsoStages[i] != 'LOG',
                ),
                const SizedBox(width: 3),
              ],
            ],
          ),
        );
        return live.live ? row : Opacity(opacity: 0.45, child: row);
      },
    );
  }
}

/// The QRZ picture across the card, and under it the operator name and
/// the distance.
class _Station extends StatelessWidget {
  final String? name;
  final String? image;
  final int? km;

  const _Station({required this.name, required this.image, required this.km});

  @override
  Widget build(BuildContext context) {
    final url = image ?? '';
    final hasImage = url.startsWith('https://') || url.startsWith('http://');
    final kmText = km == null || km! <= 0 ? null : '${km}km';
    final text = Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      mainAxisSize: MainAxisSize.min,
      children: [
        if ((name ?? '').isNotEmpty)
          Text(
            name!,
            key: const ValueKey('auto-dx-name'),
            maxLines: 1,
            overflow: TextOverflow.ellipsis,
            style: AppTheme.body(
              14,
              weight: FontWeight.w600,
            ).copyWith(height: 1.25),
          ),
        if (kmText != null)
          Text(
            kmText,
            key: const ValueKey('auto-dx-km'),
            maxLines: 1,
            style: AppTheme.mono(
              13,
              color: AppTheme.txtMute,
              weight: FontWeight.w600,
            ).copyWith(height: 1.3),
          ),
      ],
    );
    return LayoutBuilder(
      builder: (context, c) {
        // The picture takes the width of the card, the name and the distance
        // go under it. Its height is what is left above them, at most 3/4 of
        // the width. Below 32 dp the card shows no picture.
        final textHeight =
            ((name ?? '').isNotEmpty ? 18.0 : 0) +
            (kmText != null ? 17.0 : 0) +
            4;
        final h = (c.maxHeight - textHeight).clamp(0.0, c.maxWidth * 0.75);
        return SingleChildScrollView(
          // Clipped, not overflowing, when a banner leaves little height.
          physics: const NeverScrollableScrollPhysics(),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              if (hasImage && h >= 32) ...[
                ClipRRect(
                  borderRadius: BorderRadius.circular(3),
                  child: SizedBox(
                    key: const ValueKey('auto-dx-image'),
                    width: c.maxWidth,
                    height: h,
                    child: _QrzPicture(url),
                  ),
                ),
                const SizedBox(height: 4),
              ],
              text,
            ],
          ),
        );
      },
    );
  }
}

/// Without a DX call: what 75auto does now, or why the rules propose
/// nothing.
class _Doing extends StatelessWidget {
  final NextActionText text;

  const _Doing({required this.text});

  @override
  Widget build(BuildContext context) {
    return LayoutBuilder(
      builder: (context, c) {
        final lines = text.why.isEmpty
            ? 0
            : ((c.maxHeight - 22) / 14).floor().clamp(0, 4);
        return Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            Text(
              text.what,
              key: const ValueKey('auto-next-what'),
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
              style: AppTheme.display(
                17,
                color: text.whatColor,
                weight: FontWeight.w700,
                letterSpacing: 0.04,
              ).copyWith(height: 1.2),
            ),
            if (lines > 0)
              Text(
                text.why,
                key: const ValueKey('auto-next-why'),
                maxLines: lines,
                overflow: TextOverflow.ellipsis,
                style: AppTheme.body(
                  11,
                  color: AppTheme.txtMute,
                ).copyWith(height: 1.2),
              ),
          ],
        );
      },
    );
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
      decoration: BoxDecoration(
        border: Border(top: BorderSide(color: AppTheme.cardLine)),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        mainAxisSize: MainAxisSize.min,
        children: [
          Text(
            text.what,
            key: const ValueKey('auto-next-what'),
            maxLines: 1,
            overflow: TextOverflow.ellipsis,
            style: AppTheme.display(
              17,
              color: text.whatColor,
              weight: FontWeight.w700,
              letterSpacing: 0.04,
            ).copyWith(height: 1.2),
          ),
          if (text.why.isNotEmpty)
            Text(
              text.why,
              key: const ValueKey('auto-next-why'),
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
              style: AppTheme.body(
                11,
                color: AppTheme.txtMute,
              ).copyWith(height: 1.2),
            ),
          const SizedBox(height: 4),
          ProposalButtons(pending: pending),
        ],
      ),
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
      decoration: BoxDecoration(
        border: Border(
          top: BorderSide(color: AppTheme.blend(AppTheme.red, 0.6)),
        ),
      ),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        mainAxisSize: MainAxisSize.min,
        children: [
          Text(
            text.what,
            key: const ValueKey('auto-next-what'),
            maxLines: 1,
            overflow: TextOverflow.ellipsis,
            style: AppTheme.display(
              15,
              color: AppTheme.red,
              weight: FontWeight.w700,
            ).copyWith(height: 1.2),
          ),
          if (text.why.isNotEmpty)
            Text(
              text.why,
              key: const ValueKey('auto-next-why'),
              maxLines: 2,
              overflow: TextOverflow.ellipsis,
              style: AppTheme.body(
                11,
                color: AppTheme.txtMute,
              ).copyWith(height: 1.2),
            ),
        ],
      ),
    );
  }
}

/// The QRZ picture. On Android and iOS the picture is kept in a file cache
/// on the device (cached_network_image), so the same picture loads only
/// once, also after a restart. The web build uses the cache of the browser.
/// Faces are near the top of most QRZ pictures, so the top stays in view.
class _QrzPicture extends StatelessWidget {
  final String url;

  const _QrzPicture(this.url);

  @override
  Widget build(BuildContext context) {
    if (kIsWeb) {
      return Image.network(
        url,
        fit: BoxFit.cover,
        alignment: Alignment.topCenter,
        // The web build fetches under CORS. Without CORS headers on the
        // picture host it falls back to an <img> element.
        webHtmlElementStrategy: WebHtmlElementStrategy.fallback,
        errorBuilder: (_, _, _) => const SizedBox.expand(),
      );
    }
    return CachedNetworkImage(
      imageUrl: url,
      fit: BoxFit.cover,
      alignment: Alignment.topCenter,
      fadeInDuration: Duration.zero,
      placeholder: (_, _) => const SizedBox.expand(),
      errorWidget: (_, _, _) => const SizedBox.expand(),
    );
  }
}
