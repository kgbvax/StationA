import 'package:flutter/material.dart';

import '../theme.dart';

/// A card of the 75AUTO page: a title strip on the pane color, with an
/// optional item on the right, and the content below. In a fixed-height
/// parent the content fills the rest. With [shrink] the card takes the
/// height of its content.
class AutoCard extends StatelessWidget {
  final String title;
  final Widget? trailing;
  final Widget child;
  final bool shrink;

  /// The height of the title strip.
  final double titleHeight;

  const AutoCard(
      {super.key, required this.title, this.trailing, required this.child, this.shrink = false, this.titleHeight = defaultTitleHeight});

  static const defaultTitleHeight = 22.0;

  @override
  Widget build(BuildContext context) {
    final head = Container(
      height: titleHeight,
      padding: const EdgeInsets.symmetric(horizontal: 8),
      decoration: BoxDecoration(
        color: AppTheme.pane,
        border: Border(bottom: BorderSide(color: AppTheme.cardLine)),
      ),
      child: Row(
        children: [
          Text(title.toUpperCase(), maxLines: 1, style: AppTheme.display(11, color: AppTheme.txtMute, letterSpacing: 0.08)),
          if (trailing != null) ...[
            const SizedBox(width: 8),
            Expanded(child: Align(alignment: Alignment.centerRight, child: trailing!)),
          ],
        ],
      ),
    );
    return Container(
      decoration: AppTheme.cardDecoration(),
      clipBehavior: Clip.hardEdge,
      child: Column(
        mainAxisSize: shrink ? MainAxisSize.min : MainAxisSize.max,
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [head, if (shrink) child else Expanded(child: child)],
      ),
    );
  }
}

/// A small marker tag in a list row: NEW, TGT.
class AutoTag extends StatelessWidget {
  final String label;
  final Color color;

  const AutoTag(this.label, this.color, {super.key});

  @override
  Widget build(BuildContext context) {
    return Container(
      margin: const EdgeInsets.only(left: 4),
      padding: const EdgeInsets.symmetric(horizontal: 4),
      decoration: BoxDecoration(
        border: Border.all(color: color),
        borderRadius: BorderRadius.circular(3),
      ),
      child: Text(label, style: AppTheme.display(10, color: color, letterSpacing: 0.06)),
    );
  }
}
