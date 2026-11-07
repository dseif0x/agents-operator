// Unsent messages (text and attached files) per conversation, kept in
// IndexedDB so leaving a chat and coming back keeps them. Everything fails
// soft: without IndexedDB (private mode) drafts simply are not kept.

export interface Draft {
  text: string;
  files: File[];
}

const DB = "agents-operator";
const STORE = "drafts";

let dbp: Promise<IDBDatabase> | null = null;

function db(): Promise<IDBDatabase> {
  if (!dbp) {
    dbp = new Promise((resolve, reject) => {
      const req = indexedDB.open(DB, 1);
      req.onupgradeneeded = () => req.result.createObjectStore(STORE);
      req.onsuccess = () => resolve(req.result);
      req.onerror = () => reject(req.error);
    });
    dbp.catch(() => (dbp = null));
  }
  return dbp;
}

interface Stored {
  text: string;
  files: { name: string; type: string; blob: Blob }[];
  at: number;
}

/** loadDraft returns the draft of key, or an empty one. */
export async function loadDraft(key: string): Promise<Draft> {
  try {
    const d = await db();
    const v = await new Promise<Stored | undefined>((resolve, reject) => {
      const req = d.transaction(STORE).objectStore(STORE).get(key);
      req.onsuccess = () => resolve(req.result as Stored | undefined);
      req.onerror = () => reject(req.error);
    });
    if (!v) return { text: "", files: [] };
    return { text: v.text, files: v.files.map((f) => new File([f.blob], f.name, { type: f.type })) };
  } catch {
    return { text: "", files: [] };
  }
}

/** saveDraft keeps the draft of key; an empty draft is removed. */
export async function saveDraft(key: string, draft: Draft): Promise<void> {
  try {
    const d = await db();
    const store = d.transaction(STORE, "readwrite").objectStore(STORE);
    if (!draft.text.trim() && draft.files.length === 0) {
      store.delete(key);
      return;
    }
    // Files over 20 MiB in total are not kept (the text still is).
    let total = 0;
    const files = draft.files.filter((f) => (total += f.size) <= 20 << 20).map((f) => ({ name: f.name, type: f.type, blob: f as Blob }));
    store.put({ text: draft.text, files, at: Date.now() } satisfies Stored, key);
  } catch {
    /* not kept */
  }
}
