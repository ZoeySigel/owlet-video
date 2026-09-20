import { refreshSession } from "./api";

class UploadError extends Error {
  constructor(message: string, readonly status = 0) { super(message); }
}

// Transport progress is separate from server acknowledgement: a fully sent
// chunk is not resumable until the server has verified and committed it.
function sendChunk(path: string, part: Blob, hash: string, signal: AbortSignal,
  progress: (loaded: number) => void): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) { reject(new Error("upload_cancelled")); return; }
    const xhr = new XMLHttpRequest();
    let settled = false;
    let idle: ReturnType<typeof setTimeout>;
    const finish = (error?: Error) => {
      if (settled) return;
      settled = true;
      clearTimeout(idle);
      signal.removeEventListener("abort", cancel);
      if (error) reject(error); else resolve();
    };
    const cancel = () => { finish(new Error("upload_cancelled")); xhr.abort(); };
    const touch = () => {
      if (settled) return;
      clearTimeout(idle);
      idle = setTimeout(() => { finish(new UploadError("upload_timeout")); xhr.abort(); }, 120000);
    };
    xhr.open("PUT", "/api/v1" + path);
    xhr.timeout = 10 * 60 * 1000;
    xhr.setRequestHeader("X-Chunk-MD5", hash);
    xhr.upload.onprogress = (event) => {
      touch();
      progress(Math.min(event.loaded, part.size));
    };
    xhr.upload.onload = touch;
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) { finish(); return; }
      let code = `HTTP ${xhr.status}`;
      try { code = JSON.parse(xhr.responseText).error || code; } catch { /* proxy response */ }
      finish(new UploadError(code, xhr.status));
    };
    xhr.onerror = () => finish(new UploadError("upload_network_error"));
    xhr.ontimeout = () => finish(new UploadError("upload_timeout"));
    xhr.onabort = () => finish(new Error("upload_cancelled"));
    signal.addEventListener("abort", cancel, { once: true });
    touch();
    try { xhr.send(part); } catch (error) { finish(error instanceof Error ? error : new Error("upload_network_error")); }
  });
}

export async function uploadChunk(path: string, part: Blob, hash: string,
  signal: AbortSignal, progress: (loaded: number) => void): Promise<void> {
  try {
    await sendChunk(path, part, hash, signal, progress);
  } catch (error) {
    if (!(error instanceof UploadError) || error.status !== 401 || signal.aborted) throw error;
    if (!(await refreshSession())) throw new Error("invalid_session");
    progress(0);
    await sendChunk(path, part, hash, signal, progress);
  }
}

export function uploadProgress(size: number, chunkSize: number, confirmed: Set<number>, inFlight: Map<number, number>): number {
  let bytes = 0;
  for (const index of confirmed) bytes += Math.max(0, Math.min(chunkSize, size - index * chunkSize));
  for (const [index, loaded] of inFlight) if (!confirmed.has(index)) bytes += loaded;
  return 15 + Math.floor(Math.min(bytes / size, 1) * 70);
}
