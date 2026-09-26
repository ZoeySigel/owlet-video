export type User = { id: number; username: string; bio: string; avatarUrl: string; createdAt: string };
export type Video = { id: number; userId: number; author: User; title: string; description: string; playUrl: string; coverUrl: string; likesCount: number; commentsCount: number; popularity: number; publishedAt: string };
export type Comment = { id: number; userId: number; author: User; videoId: number; body: string; createdAt: string };
export type Message = { id: number; senderId: number; recipientId: number; body: string; createdAt: string };
export type Notice = { id: number; userId: number; actorId: number; videoId?: number; kind: string; body: string; readAt?: string; createdAt: string };
export type Feed = { items: Video[]; nextCursor: string };

const prefix = '/api/v1';
let refreshPromise: Promise<boolean> | null = null;

// Only public resources can survive a page unmount. Private messages, account
// state, likes and following feeds always go through the authenticated API.
const publicCache = new Map<string, { value: unknown; expires: number }>();
const publicRequests = new Map<string, Promise<unknown>>();
let cacheGeneration = 0;
function publicPath(path: string): boolean {
  if (/^\/videos\/[0-9]+(?:\/comments)?$/.test(path)) return true;
  if (/^\/users\/[0-9]+(?:\/videos|\/followers|\/following)?$/.test(path)) return true;
  if (/^\/tags\/[^/?]+\/videos$/.test(path)) return true;
  return /^\/videos\?sort=(latest|hot|likes)(?:&cursor=[^&]+)?$/.test(path);
}
export function clearPublicCache() {
  cacheGeneration++;
  publicCache.clear();
  publicRequests.clear();
}
function rememberPublic(path: string, value: unknown) {
  publicCache.delete(path);
  publicCache.set(path, { value, expires: Date.now() + 15000 });
  if (publicCache.size > 64) publicCache.delete(publicCache.keys().next().value!);
}
export function cachedPublic<T>(path: string | null): T | null {
  if (!path || !publicPath(path)) return null;
  const entry = publicCache.get(path);
  if (!entry) return null;
  if (entry.expires <= Date.now()) { publicCache.delete(path); return null; }
  publicCache.delete(path);
  publicCache.set(path, entry);
  return entry.value as T;
}
export function rememberVideo(video: Video) { rememberPublic(`/videos/${video.id}`, video); }
export function readResource<T>(path: string, fresh = false): Promise<T> {
  if (!publicPath(path)) return api<T>(path);
  if (fresh) { clearPublicCache(); }
  const cached = cachedPublic<T>(path);
  if (cached !== null) return Promise.resolve(cached);
  const pending = publicRequests.get(path);
  if (pending) return pending as Promise<T>;
  if (publicRequests.size >= 64) return api<T>(path);
  const generation = cacheGeneration;
  const request = api<T>(path).then(value => {
    if (generation === cacheGeneration) rememberPublic(path, value);
    return value;
  }).finally(() => { if (publicRequests.get(path) === request) publicRequests.delete(path); });
  publicRequests.set(path, request);
  return request;
}

export function refreshSession(): Promise<boolean> {
  clearPublicCache();
  refreshPromise ??= fetch(prefix + '/auth/refresh', {
    method: 'POST', credentials: 'same-origin', signal: AbortSignal.timeout(30000),
  }).then(r => r.ok).finally(() => { refreshPromise = null; });
  return refreshPromise;
}

export async function api<T>(path: string, init: RequestInit = {}, retry = true): Promise<T> {
  const mutation = !['GET', 'HEAD'].includes((init.method || 'GET').toUpperCase());
  if (mutation) clearPublicCache();
  const headers = new Headers(init.headers);
  if (init.body && !(init.body instanceof FormData) && !(init.body instanceof Blob)) headers.set('Content-Type', 'application/json');
  const response = await fetch(prefix + path, { ...init, headers, credentials: 'same-origin' });
  if (mutation) clearPublicCache();
  if (response.status === 401 && retry && !path.startsWith('/auth/')) {
    if (await refreshSession()) return api<T>(path, init, false);
  }
  if (!response.ok) {
    let code = `HTTP ${response.status}`;
    try { code = (await response.json()).error || code; } catch { /* network body */ }
    throw new Error(code);
  }
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}

export const json = (value: unknown) => JSON.stringify(value);
