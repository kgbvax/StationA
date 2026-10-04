import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/mqtt/bridge_endpoint.dart';

void main() {
  test('page origin with explicit port', () {
    final ep = bridgeEndpoint(Uri.parse('http://192.168.1.178:8091/'));
    expect(ep.url, 'ws://192.168.1.178:8091/mqtt');
    expect(ep.port, 8091); // must reach client.port, not the 1883 default
  });

  test('implicit http port is 80', () {
    final ep = bridgeEndpoint(Uri.parse('http://scmino/#/console'));
    expect(ep.url, 'ws://scmino:80/mqtt');
    expect(ep.port, 80);
  });

  test('https page uses wss on 443', () {
    final ep = bridgeEndpoint(Uri.parse('https://console.example/'));
    expect(ep.url, 'wss://console.example:443/mqtt');
    expect(ep.port, 443);
  });
}
