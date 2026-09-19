export type User = { id: number; username: string; bio: string; avatarUrl: string; createdAt: string };
export type Video = { id: number; userId: number; author: User; title: string; description: string; playUrl: string; coverUrl: string; likesCount: number; commentsCount: number; popularity: number; publishedAt: string };
export type Comment = { id: number; userId: number; author: User; videoId: number; body: string; createdAt: string };
export type Message = { id: number; senderId: number; recipientId: number; body: string; createdAt: string };
export type Notice = { id: number; userId: number; actorId: number; videoId?: number; kind: string; body: string; readAt?: string; createdAt: string };
export type Feed = { items: Video[]; nextCursor: string };

const prefix = '/api/v1';
let refreshPromise: Promise<boolean> | null = null;

export async function api<T>(path: string, init: RequestInit = {}, retry = true): Promise<T> {
  const headers = new Headers(init.headers);
  if (init.body && !(init.body instanceof FormData) && !(init.body instanceof Blob)) headers.set('Content-Type', 'application/json');
  const response = await fetch(prefix + path, { ...init, headers, credentials: 'same-origin' });
  if (response.status === 401 && retry && !path.startsWith('/auth/')) {
    refreshPromise ??= fetch(prefix + '/auth/refresh', { method: 'POST', credentials: 'same-origin' }).then(r => r.ok).finally(() => { refreshPromise = null; });
    if (await refreshPromise) return api<T>(path, init, false);
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
