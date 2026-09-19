import {
  useCallback,
  useEffect,
  useState,
  type ButtonHTMLAttributes,
  type ReactNode,
} from "react";
import { api, type User, type Video } from "./api";

const paths: Record<string, ReactNode> = {
  grid: (
    <>
      <rect x="3" y="3" width="7" height="7" rx="1" />
      <rect x="14" y="3" width="7" height="7" rx="1" />
      <rect x="3" y="14" width="7" height="7" rx="1" />
      <rect x="14" y="14" width="7" height="7" rx="1" />
    </>
  ),
  people: (
    <>
      <circle cx="9" cy="8" r="3" />
      <path d="M3 21v-3a6 6 0 0 1 12 0v3M16 5a3 3 0 0 1 0 6m3 10v-3a6 6 0 0 0-2-4" />
    </>
  ),
  heart: (
    <path d="M20.8 4.6a5.5 5.5 0 0 0-7.8 0L12 5.7l-1.1-1.1a5.5 5.5 0 0 0-7.8 7.8L12 21l8.8-8.6a5.5 5.5 0 0 0 0-7.8Z" />
  ),
  message: (
    <path d="M21 11.5a9 9 0 0 1-9.5 9 10 10 0 0 1-4-.9L3 21l1.4-4.5a10 10 0 0 1-.9-4A9 9 0 1 1 21 11.5Z" />
  ),
  bell: (
    <>
      <path d="M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9M10 21h4" />
    </>
  ),
  plus: <path d="M12 5v14M5 12h14" />,
  play: <path d="m9 5 11 7-11 7Z" />,
  arrow: <path d="m10 5-7 7 7 7M3 12h18" />,
  share: (
    <>
      <path d="m14 3 7 7-7 7M21 10H9v10H3V10" />
    </>
  ),
  settings: (
    <>
      <path d="M4 6h16M4 12h16M4 18h16" />
      <circle cx="8" cy="6" r="2" />
      <circle cx="16" cy="12" r="2" />
      <circle cx="10" cy="18" r="2" />
    </>
  ),
  upload: (
    <>
      <path d="M12 16V3m-5 5 5-5 5 5M4 16v5h16v-5" />
    </>
  ),
  check: <path d="m5 12 4 4L19 6" />,
  close: <path d="m6 6 12 12M6 18 18 6" />,
  video: (
    <>
      <rect x="3" y="5" width="13" height="14" rx="2" />
      <path d="m16 10 5-3v10l-5-3" />
    </>
  ),
};
export function Icon({
  name,
  className = "",
}: {
  name: string;
  className?: string;
}) {
  return (
    <svg
      className={`icon ${className}`}
      width="20"
      height="20"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.7"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      {paths[name] || paths.video}
    </svg>
  );
}
export function Button({
  children,
  variant = "primary",
  className = "",
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: "primary" | "secondary" | "ghost";
}) {
  return (
    <button
      type="button"
      className={`button ${variant} ${className}`}
      {...props}
    >
      {children}
    </button>
  );
}
export function Avatar({ user }: { user?: User | null }) {
  const [failed, setFailed] = useState(false);
  useEffect(() => setFailed(false), [user?.avatarUrl]);
  return (
    <span className="avatar" aria-hidden="true">
      {user?.avatarUrl && !failed ? (
        <img src={user.avatarUrl} alt="" onError={() => setFailed(true)} />
      ) : (
        (user?.username?.[0] || "O").toUpperCase()
      )}
    </span>
  );
}
export function PageHeading({
  title,
  description,
  children,
}: {
  title: string;
  description?: string;
  children?: ReactNode;
}) {
  return (
    <div className="page-heading">
      <div>
        <h1>{title}</h1>
        {description && <p>{description}</p>}
      </div>
      {children}
    </div>
  );
}
export function Empty({
  title = "暂无内容",
  body,
  action,
  onAction,
}: {
  title?: string;
  body: string;
  action?: string;
  onAction?: () => void;
}) {
  return (
    <div className="empty">
      <div className="empty-symbol">
        <Icon name="video" />
      </div>
      <h2>{title}</h2>
      <p>{body}</p>
      {action && <Button onClick={onAction}>{action}</Button>}
    </div>
  );
}
export function ErrorState({
  message = "加载失败，请重试。",
  retry,
}: {
  message?: string;
  retry?: () => void;
}) {
  return (
    <div className="error-line" role="alert">
      <span>{message}</span>
      {retry && (
        <Button variant="secondary" onClick={retry}>
          重试
        </Button>
      )}
    </div>
  );
}
export function Loading({ cards = false }: { cards?: boolean }) {
  return cards ? (
    <div
      className="video-grid skeleton-grid"
      role="status"
      aria-label="正在加载视频"
    >
      {Array.from({ length: 8 }, (_, i) => (
        <div key={i} className="skeleton-card">
          <div />
          <span />
          <span />
        </div>
      ))}
    </div>
  ) : (
    <div className="loading" role="status">
      <span className="spinner" />
      正在加载…
    </div>
  );
}
export function useResource<T>(path: string | null) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(!!path);
  const [version, setVersion] = useState(0);
  const reload = useCallback(() => setVersion((v) => v + 1), []);
  useEffect(() => {
    let active = true;
    setData(null);
    setError("");
    setLoading(!!path);
    if (path)
      void api<T>(path)
        .then((value) => {
          if (active) setData(value);
        })
        .catch((err) => {
          if (active) setError(errorMessage(err));
        })
        .finally(() => {
          if (active) setLoading(false);
        });
    return () => {
      active = false;
    };
  }, [path, version]);
  return { data, setData, error, loading, reload };
}
export function errorMessage(error: unknown) {
  const code = error instanceof Error ? error.message : String(error);
  const messages: Record<string, string> = {
    login_required: "请先登录后继续操作。",
    invalid_session: "登录已过期，请重新登录。",
    invalid_credentials: "用户名或密码不正确，请检查后重试。",
    invalid_registration: "请检查用户名、密码和邀请码。",
    invalid_invite_or_username: "邀请码无效或用户名已被使用。",
    invalid_username: "用户名需为 3–40 位字母、数字或下划线。",
    invalid_password: "新密码需为 12–72 字节。",
    invalid_profile: "请检查个人资料，简介内容可能过长。",
    invalid_comment: "评论内容为空或过长，请修改后重试。",
    invalid_message: "消息内容为空或过长，请修改后重试。",
    rate_limited: "操作过于频繁，请稍后重试。",
    video_not_found: "视频不存在或已删除。",
    user_not_found: "用户不存在。",
    update_failed: "保存失败，用户名可能已被使用。",
  };
  return messages[code] || "操作失败，请检查网络后重试。";
}
export const date = (value: string) =>
  new Intl.DateTimeFormat("zh-CN", { month: "short", day: "numeric" }).format(
    new Date(value),
  );
export function VideoGrid({
  videos,
  open,
}: {
  videos: Video[];
  open: (id: number) => void;
}) {
  return (
    <div className="video-grid">
      {videos.map((video) => (
        <VideoCard key={video.id} video={video} open={open} />
      ))}
    </div>
  );
}
function VideoCard({
  video,
  open,
}: {
  video: Video;
  open: (id: number) => void;
}) {
  const [failed, setFailed] = useState(false);
  return (
    <button
      className="video-card"
      onClick={() => open(video.id)}
      aria-label={`播放：${video.title}`}
    >
      <span className="video-art">
        {video.coverUrl && !failed ? (
          <img
            src={video.coverUrl}
            alt=""
            loading="lazy"
            onError={() => setFailed(true)}
          />
        ) : (
          <span className="cover-fallback">
            <Icon name="video" />
            <span>暂无封面</span>
          </span>
        )}
        <span className="play-glyph">
          <Icon name="play" />
        </span>
        <span className="cover-likes">
          <Icon name="heart" />
          {video.likesCount.toLocaleString()}
        </span>
      </span>
      <strong>{video.title}</strong>
      <span className="video-foot">
        <span className="card-author">
          <Avatar user={video.author} />
          <span>{video.author?.username || "创作者"}</span>
        </span>
        <time>{date(video.publishedAt)}</time>
      </span>
    </button>
  );
}
