// Kill switch for the Flutter service worker earlier builds registered.
// deploy.sh copies this over the generated build/web/flutter_service_worker.js.
// A browser that still has the old worker fetches this on its update check,
// installs it, drops every cache, unregisters and reloads its open pages, so
// the next load comes straight from the server.
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    for (const key of await caches.keys()) await caches.delete(key);
    await self.registration.unregister();
    for (const client of await self.clients.matchAll({ type: 'window' })) {
      client.navigate(client.url);
    }
  })());
});
