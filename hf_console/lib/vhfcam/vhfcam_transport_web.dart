// vhfcam_transport_web.dart — web stub. dart:io does not exist in the browser,
// and video_player's web implementation cannot decode the HLS/MPEG-TS live
// stream either. The CAM page therefore renders its web placeholder (a link to
// the :8083 reference page) and keeps every control disabled; the stub makes
// that state the steady one — every request reports unreachable.

Future<(bool, String)> vhfcamHttpGet(String url, Duration timeout) async => (false, '');

Future<bool> vhfcamHttpPost(String url, Duration timeout) async => false;
