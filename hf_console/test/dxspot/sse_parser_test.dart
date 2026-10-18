import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/dxspot/sse_parser.dart';

void main() {
  late List<String> got;
  late SseParser p;
  setUp(() {
    got = [];
    p = SseParser(got.add);
  });

  test('dispatches default events on a blank line, strips one leading space', () {
    p.addLine('data: {"a":1}');
    p.addLine('');
    expect(got, ['{"a":1}']);
  });

  test('joins multi-line data with newlines', () {
    p.addLine('data: one');
    p.addLine('data:two');
    p.addLine('');
    expect(got, ['one\ntwo']);
  });

  test('ignores comments (heartbeats) and id/retry fields', () {
    p.addLine(': hb');
    p.addLine('');
    p.addLine('id: 7');
    p.addLine('retry: 1000');
    p.addLine('data: x');
    p.addLine('');
    expect(got, ['x']);
  });

  test('drops named events, keeps explicit message events', () {
    p.addLine('event: history_end');
    p.addLine('data: {}');
    p.addLine('');
    p.addLine('event: server_error');
    p.addLine('data: boom');
    p.addLine('');
    p.addLine('event: message');
    p.addLine('data: spot');
    p.addLine('');
    p.addLine('data: next');
    p.addLine('');
    expect(got, ['spot', 'next']);
  });
}
