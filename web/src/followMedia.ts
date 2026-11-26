// Keeps a scroll view at its end while content above it grows.
//
// A view jumps to the bottom once its messages are rendered, but parts of
// them arrive later: pictures and videos get their size when they load, a
// chat message's "Worked through N steps" block appears when its turn has
// been fetched. Each pushes the newest message out of view, and growing
// content fires no scroll event, so "stay at the bottom" never ran again.
// A ResizeObserver on every child of the view catches all of it (new
// children are observed as they are added); media load events are kept as
// a second trigger. atBottom is the reader's position as of their last
// scroll, so a reader who scrolled up is left alone.
export function followMediaLoads(el: HTMLElement, atBottom: () => boolean): () => void {
  const stick = () => {
    if (atBottom()) el.scrollTop = el.scrollHeight;
  };
  const onLoad = (e: Event) => {
    const t = e.target;
    if (t instanceof HTMLImageElement || t instanceof HTMLVideoElement) stick();
  };
  el.addEventListener("load", onLoad, true);
  el.addEventListener("loadedmetadata", onLoad, true);

  const ro = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(() => stick());
  const observeAll = () => {
    if (!ro) return;
    for (const child of Array.from(el.children)) ro.observe(child);
  };
  observeAll();
  const mo = new MutationObserver((records) => {
    if (!ro) return;
    for (const r of records) r.addedNodes.forEach((n) => n instanceof Element && ro.observe(n));
    stick(); // a new message or block at the end
  });
  mo.observe(el, { childList: true });

  return () => {
    el.removeEventListener("load", onLoad, true);
    el.removeEventListener("loadedmetadata", onLoad, true);
    ro?.disconnect();
    mo.disconnect();
  };
}
