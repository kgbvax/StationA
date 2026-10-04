import 'package:flutter_test/flutter_test.dart';
import 'package:hf_console/vhfcam/vhfcam_service.dart';

void main() {
  test('missing or empty stored value resolves to the scmino default', () {
    expect(resolveVhfcamBaseUrl(null), defaultVhfcamBaseUrl);
    expect(resolveVhfcamBaseUrl('  '), defaultVhfcamBaseUrl);
    expect(defaultVhfcamBaseUrl, 'http://192.168.1.178:8083');
  });

  test('a saved shari URL migrates to the scmino default', () {
    for (final legacy in [
      'http://192.168.1.139:8083',
      'http://192.168.1.140:8083/',
      'http://shari:8083',
      'http://SHARI:8083',
    ]) {
      expect(resolveVhfcamBaseUrl(legacy), defaultVhfcamBaseUrl, reason: legacy);
    }
  });

  test('any other URL is kept as entered', () {
    expect(resolveVhfcamBaseUrl(' http://scmino:8083 '), 'http://scmino:8083');
    expect(resolveVhfcamBaseUrl('http://10.0.0.5:9000'), 'http://10.0.0.5:9000');
  });
}
