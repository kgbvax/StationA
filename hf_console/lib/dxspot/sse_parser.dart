// sse_parser.dart — line-level Server-Sent Events frame parser (HTML spec subset).
//
// Feed it one line at a time (line terminator already stripped). A blank line
// dispatches the event; one leading space after `data:` is stripped; multiple
// `data:` lines join with `\n`. Lines starting with `:` are comments;
// `id:`/`retry:` are ignored. Only default (unnamed / `message`) events reach
// [onMessage] — named ones (horstreporter's `history_end`, `server_error`,
// `area`) are dropped, matching EventSource.onMessage in the browser.

class SseParser {
  SseParser(this.onMessage);

  final void Function(String data) onMessage;
  final StringBuffer _data = StringBuffer();
  String _event = '';

  void addLine(String line) {
    if (line.isEmpty) {
      if (_data.isNotEmpty && (_event.isEmpty || _event == 'message')) {
        onMessage(_data.toString().trimRight());
      }
      reset();
      return;
    }
    if (line.startsWith('data:')) {
      final v = line.substring(5);
      _data.writeln(v.startsWith(' ') ? v.substring(1) : v);
    } else if (line.startsWith('event:')) {
      final v = line.substring(6);
      _event = v.startsWith(' ') ? v.substring(1) : v;
    }
  }

  void reset() {
    _data.clear();
    _event = '';
  }
}
