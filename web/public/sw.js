// agents-operator service worker: push notifications only. It does not
// cache anything, so the app is never served stale; a PWA needs no offline
// copy of a page whose whole point is a live connection.

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (event) => event.waitUntil(self.clients.claim()));

self.addEventListener("push", (event) => {
  let n = {};
  try {
    n = event.data ? event.data.json() : {};
  } catch {
    n = { title: "agents-operator", body: event.data ? event.data.text() : "" };
  }
  event.waitUntil(
    self.registration.showNotification(n.title || "agents-operator", {
      body: n.body || "",
      tag: n.tag || undefined,
      renotify: !!n.tag,
      icon: "/icon-192.png",
      badge: "/icon-192.png",
      data: { url: n.url || "/" },
    }),
  );
});

// A tap opens the page the notification is about: in a window of the app
// that is already open, or a new one.
self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = new URL((event.notification.data && event.notification.data.url) || "/", self.location.origin).href;
  event.waitUntil(
    (async () => {
      const wins = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
      for (const w of wins) {
        if (new URL(w.url).origin === self.location.origin) {
          await w.focus();
          w.postMessage({ type: "navigate", url });
          return;
        }
      }
      await self.clients.openWindow(url);
    })(),
  );
});
