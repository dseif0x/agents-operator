// Keeps a scroll view at its end while pictures and videos above load.
//
// A view jumps to the bottom once its messages are rendered, but images and
// videos get their size only when they have loaded: each one then pushes the
// content up, and growing content fires no scroll event, so "stay at the
// bottom" never ran again and the reader had to scroll back down. load and
// loadedmetadata don't bubble, so they are caught in the capture phase.
export function followMediaLoads(el: HTMLElement, atBottom: () => boolean): () => void {
  const onLoad = (e: Event) => {
    const t = e.target as HTMLElement | null;
    if (!t || !(t instanceof HTMLImageElement || t instanceof HTMLVideoElement)) return;
    if (atBottom()) el.scrollTop = el.scrollHeight;
  };
  el.addEventListener("load", onLoad, true);
  el.addEventListener("loadedmetadata", onLoad, true);
  return () => {
    el.removeEventListener("load", onLoad, true);
    el.removeEventListener("loadedmetadata", onLoad, true);
  };
}
