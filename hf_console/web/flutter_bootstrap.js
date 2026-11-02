{{flutter_js}}
{{flutter_build_config}}

// No service worker: the web channel is LAN-only and always online, and a
// caching worker kept serving the previous build after a deploy. The old
// worker in browsers that registered one unregisters itself
// (tool/web/flutter_service_worker.js, copied in by deploy.sh).
_flutter.loader.load();
