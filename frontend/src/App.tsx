"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import {
  api,
  json,
  type Comment,
  type Feed,
  type Message,
  type Notice,
  type User,
  type Video,
} from "./api";
import {
  Avatar,
  Button,
  Empty,
  ErrorState,
  Icon,
  Loading,
  PageHeading,
  VideoGrid,
  date,
  errorMessage,
  useResource,
} from "./ui";
import Publish from "./Publish";

type View =
  | "home"
  | "watch"
  | "profile"
  | "account"
  | "publish"
  | "messages"
  | "notices"
  | "settings"
  | "liked"
  | "tag";
type Sort = "latest" | "hot" | "likes" | "following";
type Route = {
  view: View;
  id?: string;
  tag?: string;
  peer?: string;
  sort?: Sort;
};
type Common = {
  navigate: (view: View, params?: Record<string, string>) => void;
  alert: (message: string) => void;
  user: User | null;
  requireUser: () => boolean;
};
const sorts: [Sort, string][] = [
  ["latest", "最新"],
  ["hot", "热门"],
  ["likes", "最多点赞"],
  ["following", "关注"],
];
const titles: Record<View, string> = {
  home: "发现",
  watch: "视频",
  profile: "个人主页",
  account: "账号",
  publish: "发布视频",
  messages: "私信",
  notices: "通知",
  settings: "设置",
  liked: "我的喜欢",
  tag: "话题",
};
function parseRoute(): Route {
  const p = new URLSearchParams(window.location.search);
  const value = p.get("view") || "home";
  const sort = p.get("sort") as Sort;
  return {
    view: Object.hasOwn(titles, value) ? (value as View) : "home",
    id: p.get("id") || undefined,
    tag: p.get("tag") || undefined,
    peer: p.get("peer") || undefined,
    sort: sorts.some(([s]) => s === sort) ? sort : "latest",
  };
}

export default function App() {
  const [route, setRoute] = useState<Route>({ view: "home", sort: "latest" });
  const [user, setUser] = useState<User | null>(null);
  const [ready, setReady] = useState(false);
  const [noticeCount, setNoticeCount] = useState(0);
  const [toast, setToast] = useState("");
  const toastTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    setRoute(parseRoute());
    const onPop = () => setRoute(parseRoute());
    window.addEventListener("popstate", onPop);
    return () => {
      window.removeEventListener("popstate", onPop);
      if (toastTimer.current) clearTimeout(toastTimer.current);
    };
  }, []);
  const refreshUser = useCallback(async () => {
    try {
      setUser(await api<User>("/auth/me"));
    } catch {
      try {
        await api("/auth/refresh", { method: "POST" });
        setUser(await api<User>("/auth/me"));
      } catch {
        setUser(null);
      }
    } finally {
      setReady(true);
    }
  }, []);
  useEffect(() => {
    void refreshUser();
  }, [refreshUser]);
  useEffect(() => {
    document.title = `${titles[route.view]} · Owlet 视频`;
  }, [route.view]);
  const refreshNotices = useCallback(() => {
    if (user)
      void api<{ count: number }>("/notifications/unread")
        .then((r) => setNoticeCount(r.count))
        .catch(() => {});
  }, [user]);
  useEffect(() => {
    if (!user) {
      setNoticeCount(0);
      return;
    }
    refreshNotices();
    const stream = new EventSource("/api/v1/notifications/stream", {
      withCredentials: true,
    });
    stream.addEventListener("notification", refreshNotices);
    return () => stream.close();
  }, [user, refreshNotices]);
  const navigate: Common["navigate"] = (view, params = {}) => {
    const p = new URLSearchParams({ view, ...params });
    window.history.pushState(
      {},
      "",
      view === "home" && !Object.keys(params).length ? "/" : "/?" + p,
    );
    setRoute(parseRoute());
    window.scrollTo({ top: 0, behavior: "instant" });
  };
  const alert = (message: string) => {
    setToast(message);
    if (toastTimer.current) clearTimeout(toastTimer.current);
    toastTimer.current = setTimeout(() => setToast(""), 4200);
  };
  const requireUser = () => {
    if (user) return true;
    alert("请先登录后继续操作");
    navigate("account");
    return false;
  };
  const props = { navigate, alert, user, requireUser };
  const navItems: {
    label: string;
    icon: string;
    view: View;
    params?: Record<string, string>;
    active: boolean;
  }[] = [
    {
      label: "发现",
      icon: "grid",
      view: "home",
      active: route.view === "home" && route.sort !== "following",
    },
    {
      label: "关注",
      icon: "people",
      view: "home",
      params: { sort: "following" },
      active: route.view === "home" && route.sort === "following",
    },
    {
      label: "我的喜欢",
      icon: "heart",
      view: "liked",
      active: route.view === "liked",
    },
    {
      label: "私信",
      icon: "message",
      view: "messages",
      active: route.view === "messages",
    },
    {
      label: "通知",
      icon: "bell",
      view: "notices",
      active: route.view === "notices",
    },
  ];
  return (
    <>
      <a className="skip-link" href="#main-content">
        跳到主要内容
      </a>
      <aside className="sidebar">
        <button
          className="brand"
          onClick={() => navigate("home")}
          aria-label="Owlet 首页"
        >
          <span className="brand-mark">
            <Icon name="play" />
          </span>
          <span className="brand-word">
            owlet<span className="brand-dot">®</span>
          </span>
        </button>
        <div className="sidebar-label">浏览视频</div>
        <nav className="main-nav" aria-label="主导航">
          {navItems.map((item) => (
            <button
              key={item.label}
              title={item.label}
              aria-label={item.label}
              aria-current={item.active ? "page" : undefined}
              className={item.active ? "active" : ""}
              onClick={() => navigate(item.view, item.params)}
            >
              <Icon name={item.icon} />
              <span>{item.label}</span>
              {item.view === "notices" && noticeCount > 0 && (
                <b className="badge">
                  {noticeCount > 99 ? "99+" : noticeCount}
                </b>
              )}
            </button>
          ))}
        </nav>
        <div className="sidebar-bottom">
          {user ? (
            <button
              className="sidebar-account"
              onClick={() => navigate("profile", { id: String(user.id) })}
            >
              <Avatar user={user} />
              <span>
                <strong>{user.username}</strong>
                <small>个人主页</small>
              </span>
            </button>
          ) : (
            <div className="sidebar-login">
              <p>
                登录后，关注创作者
                <br />
                并分享你的视频。
              </p>
              <Button onClick={() => navigate("account")}>登录 / 注册</Button>
            </div>
          )}
          <button
            className="settings-link"
            title="设置"
            aria-label="设置"
            onClick={() => navigate("settings")}
          >
            <Icon name="settings" />
            <span>设置</span>
          </button>
          <div className="sidebar-copyright">
            OWLET VIDEO © {new Date().getFullYear()}
          </div>
        </div>
      </aside>
      <header className="topbar">
        <div className="breadcrumb">
          <span>OWLET</span>
          <i>/</i>
          {route.view === "home" && route.sort === "following"
            ? "关注"
            : titles[route.view]}
        </div>
        <button className="mobile-brand" onClick={() => navigate("home")}>
          owlet®
        </button>
        <div className="header-actions">
          <Button variant="secondary" onClick={() => navigate("publish")}>
            <Icon name="plus" />
            发布<span className="desktop-word">视频</span>
          </Button>
          {user ? (
            <>
              <button
                className="icon-button header-settings"
                aria-label="设置"
                onClick={() => navigate("settings")}
              >
                <Icon name="settings" />
              </button>
              <button
                className="account-pill"
                aria-label="个人主页"
                onClick={() => navigate("profile", { id: String(user.id) })}
              >
                <Avatar user={user} />
                <span>{user.username}</span>
              </button>
            </>
          ) : (
            <Button onClick={() => navigate("account")}>登录</Button>
          )}
        </div>
      </header>
      <main id="main-content" tabIndex={-1} className="app-main">
        {!ready ? (
          <Loading />
        ) : (
          <>
            {route.view === "home" && (
              <Home
                key={`${route.sort}-${user?.id || "guest"}`}
                sort={route.sort || "latest"}
                {...props}
              />
            )}
            {route.view === "watch" && (
              <Watch
                key={`${route.id}-${user?.id || "guest"}`}
                id={route.id}
                {...props}
              />
            )}
            {route.view === "profile" && (
              <Profile key={route.id} id={route.id} {...props} />
            )}
            {route.view === "account" && (
              <Account onLogin={refreshUser} {...props} />
            )}
            {route.view === "publish" && (
              <Publish user={user} navigate={navigate} alert={alert} />
            )}
            {route.view === "messages" && (
              <Messages
                key={`${route.peer || ""}-${user?.id || ""}`}
                peer={route.peer}
                {...props}
              />
            )}
            {route.view === "notices" && (
              <Notices onRead={refreshNotices} {...props} />
            )}
            {route.view === "settings" && (
              <Settings refreshUser={refreshUser} {...props} />
            )}
            {route.view === "liked" && <Collection mode="liked" {...props} />}
            {route.view === "tag" && (
              <Collection
                key={route.tag}
                mode="tag"
                tag={route.tag}
                {...props}
              />
            )}
          </>
        )}
      </main>
      {toast && (
        <div className="toast" role="status">
          <Icon name="bell" />
          <span>{toast}</span>
          <button aria-label="关闭提示" onClick={() => setToast("")}>
            <Icon name="close" />
          </button>
        </div>
      )}
    </>
  );
}

function LoginRequired({ navigate }: Pick<Common, "navigate">) {
  return (
    <Empty
      title="请先登录"
      body="登录后即可使用此功能。注册需要邀请码。"
      action="登录 / 注册"
      onAction={() => navigate("account")}
    />
  );
}

function Home({ sort, navigate, user }: Common & { sort: Sort }) {
  const [items, setItems] = useState<Video[]>([]);
  const [cursor, setCursor] = useState("");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const active = useRef(true);
  const busy = useRef(false);
  const load = useCallback(
    async (next = "") => {
      if (busy.current) return;
      busy.current = true;
      setLoading(true);
      setError("");
      try {
        const r = await api<Feed>(
          `/videos?sort=${sort}${next ? "&cursor=" + encodeURIComponent(next) : ""}`,
        );
        if (active.current) {
          setItems((old) =>
            next
              ? [
                  ...old,
                  ...(r.items || []).filter(
                    (v) => !old.some((o) => o.id === v.id),
                  ),
                ]
              : r.items || [],
          );
          setCursor(r.nextCursor || "");
        }
      } catch (e) {
        if (active.current) setError(errorMessage(e));
      } finally {
        busy.current = false;
        if (active.current) setLoading(false);
      }
    },
    [sort],
  );
  useEffect(() => {
    active.current = true;
    if (sort !== "following" || user) void load();
    else setLoading(false);
    return () => {
      active.current = false;
    };
  }, [load, sort, user]);
  return (
    <section className="feed-section">
      <PageHeading
        title={sort === "following" ? "关注" : "发现视频"}
        description={
          sort === "following"
            ? "查看你关注的创作者发布的视频。"
            : "浏览最新发布，发现你感兴趣的内容。"
        }
      >
        <span className="section-stamp">
          <Icon name="video" />
          OWLET VIDEO
        </span>
      </PageHeading>
      <div className="feed-toolbar">
        <div className="tabs" aria-label="视频排序">
          {sorts.map(([value, label]) => (
            <button
              key={value}
              aria-pressed={sort === value}
              className={sort === value ? "selected" : ""}
              onClick={() => navigate("home", { sort: value })}
            >
              {label}
            </button>
          ))}
        </div>
        <span className="toolbar-label">
          <Icon name="grid" />
          视频列表
        </span>
      </div>
      {sort === "following" && !user ? (
        <LoginRequired navigate={navigate} />
      ) : (
        <>
          {error && (
            <ErrorState
              message={error}
              retry={() => void load(items.length ? cursor : "")}
            />
          )}{" "}
          {loading && !items.length ? (
            <Loading cards />
          ) : items.length ? (
            <>
              <VideoGrid
                videos={items}
                open={(id) => navigate("watch", { id: String(id) })}
              />
              {cursor ? (
                <Button
                  className="load-more"
                  variant="secondary"
                  disabled={loading}
                  onClick={() => void load(cursor)}
                >
                  {loading ? "正在加载…" : "加载更多"}
                </Button>
              ) : (
                <p className="list-end">已显示全部视频</p>
              )}
            </>
          ) : (
            !error && (
              <Empty
                title="暂无视频"
                body={
                  sort === "following"
                    ? "你关注的创作者暂未发布视频，去发现更多内容。"
                    : "视频发布后会显示在这里。"
                }
                action={sort === "following" ? "去发现" : "发布视频"}
                onAction={() =>
                  navigate(sort === "following" ? "home" : "publish")
                }
              />
            )
          )}
        </>
      )}
    </section>
  );
}

function FollowButton({
  id,
  user,
  requireUser,
  alert,
  onFollow,
}: Pick<Common, "user" | "requireUser" | "alert"> & {
  id: number;
  onFollow?: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);
  if (id === user?.id) return null;
  return (
    <Button
      variant="secondary"
      disabled={busy || done}
      onClick={async () => {
        if (!requireUser()) return;
        setBusy(true);
        try {
          await api("/users/" + id + "/follow", { method: "PUT" });
          setDone(true);
          alert("已关注");
          onFollow?.();
        } catch (e) {
          alert(errorMessage(e));
        } finally {
          setBusy(false);
        }
      }}
    >
      <Icon name={done ? "check" : "plus"} />
      {done ? "已关注" : busy ? "处理中…" : "关注"}
    </Button>
  );
}

function Watch({
  id,
  navigate,
  alert,
  user,
  requireUser,
}: Common & { id?: string }) {
  const video = useResource<Video>(id ? "/videos/" + id : null);
  const comments = useResource<Comment[]>(
    id ? "/videos/" + id + "/comments" : null,
  );
  const likeState = useResource<{ liked: boolean }>(
    id && user ? "/videos/" + id + "/like" : null,
  );
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [sending, setSending] = useState(false);
  const [deleting, setDeleting] = useState<number | null>(null);
  const [playerError, setPlayerError] = useState(false);
  const [playerKey, setPlayerKey] = useState(0);
  if (!id)
    return (
      <Empty
        title="未指定视频"
        body="请从视频列表选择要播放的视频。"
        action="返回发现"
        onAction={() => navigate("home")}
      />
    );
  if (video.error)
    return <ErrorState message={video.error} retry={video.reload} />;
  if (!video.data) return <Loading />;
  const v = video.data;
  const liked = likeState.data?.liked || false;
  const like = async () => {
    if (!requireUser() || busy) return;
    setBusy(true);
    try {
      const result = await api<{ liked: boolean }>("/videos/" + id + "/like", {
        method: liked ? "DELETE" : "PUT",
      });
      likeState.setData(result);
      video.setData((old) =>
        old
          ? { ...old, likesCount: Math.max(0, old.likesCount + (result.liked ? 1 : -1)) }
          : old,
      );
    } catch (e) {
      alert(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const comment = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!requireUser() || !text.trim() || sending || comments.loading || comments.error) return;
    setSending(true);
    try {
      const result = await api<Comment>("/videos/" + id + "/comments", {
        method: "POST",
        body: json({ body: text }),
      });
      comments.setData((old) => [...(old || []), result]);
      video.setData((old) =>
        old ? { ...old, commentsCount: old.commentsCount + 1 } : old,
      );
      setText("");
    } catch (err) {
      alert(errorMessage(err));
    } finally {
      setSending(false);
    }
  };
  return (
    <section className="watch">
      <Button
        variant="ghost"
        className="back-link"
        onClick={() => navigate("home")}
      >
        <Icon name="arrow" />
        返回发现
      </Button>
      <div className="watch-layout">
        <div className="watch-main">
          <div className="player">
            <video
              key={playerKey}
              src={v.playUrl}
              poster={v.coverUrl || undefined}
              controls
              playsInline
              autoPlay
              preload="metadata"
              onError={() => setPlayerError(true)}
            />
            {playerError && (
              <div className="player-error">
                <Icon name="video" />
                <p>视频暂时无法播放</p>
                <Button
                  variant="secondary"
                  onClick={() => {
                    setPlayerError(false);
                    setPlayerKey((k) => k + 1);
                  }}
                >
                  重新加载
                </Button>
              </div>
            )}
          </div>
          <div className="watch-title">
            <div className="watch-meta">
              <span>视频详情</span>
              <time>{date(v.publishedAt)}</time>
            </div>
            <h1>{v.title}</h1>
            <p>
              {v.description.split(/(#[\p{L}\p{N}_]+)/u).map((part, i) =>
                part.startsWith("#") ? (
                  <button
                    className="inline-tag"
                    key={i}
                    onClick={() => navigate("tag", { tag: part.slice(1) })}
                  >
                    {part}
                  </button>
                ) : (
                  part
                ),
              )}
            </p>
            <div className="watch-actions">
              <Button
                variant="secondary"
                className={liked ? "is-liked" : ""}
                aria-pressed={liked}
                disabled={
                  busy || (!!user && (likeState.loading || !!likeState.error))
                }
                onClick={like}
              >
                <Icon name="heart" />
                {v.likesCount} 点赞
              </Button>
              <Button
                variant="secondary"
                onClick={async () => {
                  try {
                    await navigator.clipboard.writeText(window.location.href);
                    alert("链接已复制");
                  } catch {
                    alert("复制失败，请从地址栏复制链接");
                  }
                }}
              >
                <Icon name="share" />
                分享
              </Button>
            </div>
            {likeState.error && (
              <ErrorState message="点赞状态加载失败" retry={likeState.reload} />
            )}
          </div>
        </div>
        <aside className="watch-side">
          <div className="panel-title">
            <Icon name="message" />
            评论<span>{v.commentsCount}</span>
          </div>
          <div className="author-block">
            <button
              className="author-row"
              onClick={() => navigate("profile", { id: String(v.userId) })}
            >
              <Avatar user={v.author} />
              <span>
                <strong>{v.author?.username || "创作者"}</strong>
                <small>查看个人主页</small>
              </span>
            </button>
            <FollowButton
              id={v.userId}
              user={user}
              requireUser={requireUser}
              alert={alert}
            />
          </div>
          <div className="comment-list">
            {comments.loading ? (
              <Loading />
            ) : comments.error ? (
              <ErrorState message={comments.error} retry={comments.reload} />
            ) : comments.data?.length ? (
              comments.data.map((item) => (
                <div className="comment" key={item.id}>
                  <Avatar user={item.author} />
                  <div>
                    <strong>{item.author?.username}</strong>
                    <p>{item.body}</p>
                    <div className="comment-foot">
                      <time>{date(item.createdAt)}</time>
                      {user?.id === item.userId && (
                        <button
                          disabled={deleting !== null}
                          onClick={async () => {
                            setDeleting(item.id);
                            try {
                              await api("/comments/" + item.id, {
                                method: "DELETE",
                              });
                              comments.setData((old) =>
                                (old || []).filter((c) => c.id !== item.id),
                              );
                              video.setData((old) =>
                                old
                                  ? {
                                      ...old,
                                      commentsCount: Math.max(
                                        0,
                                        old.commentsCount - 1,
                                      ),
                                    }
                                  : old,
                              );
                            } catch (e) {
                              alert(errorMessage(e));
                            } finally {
                              setDeleting(null);
                            }
                          }}
                        >
                          删除
                        </button>
                      )}
                    </div>
                  </div>
                </div>
              ))
            ) : (
              <div className="quiet-state">
                <Icon name="message" />
                <p>暂无评论</p>
                <span>发表你的看法。</span>
              </div>
            )}
          </div>
          <form className="comment-compose" onSubmit={comment}>
            <label className="sr-only" htmlFor="comment">
              评论内容
            </label>
            <textarea
              id="comment"
              placeholder="输入评论…"
              value={text}
              onChange={(e) => setText(e.target.value)}
              maxLength={1000}
            />
            <Button type="submit" disabled={sending || !text.trim() || comments.loading || !!comments.error}>
              {sending ? "发送中…" : "发表评论"}
            </Button>
          </form>
        </aside>
      </div>
    </section>
  );
}

type ProfileData = {
  user: User;
  videos: number;
  followers: number;
  following: number;
  likes: number;
};
function Profile({
  id,
  navigate,
  user,
  alert,
  requireUser,
}: Common & { id?: string }) {
  const profile = useResource<ProfileData>(id ? "/users/" + id : null);
  const videos = useResource<Video[]>(id ? "/users/" + id + "/videos" : null);
  const [relation, setRelation] = useState<"followers" | "following" | null>(
    null,
  );
  const relations = useResource<User[]>(
    relation && id ? "/users/" + id + "/" + relation : null,
  );
  if (!id)
    return (
      <Empty
        title="未指定用户"
        body="请从视频或评论中打开用户主页。"
        action="返回发现"
        onAction={() => navigate("home")}
      />
    );
  if (profile.error)
    return <ErrorState message={profile.error} retry={profile.reload} />;
  if (!profile.data) return <Loading />;
  const p = profile.data;
  return (
    <section>
      <div className="profile-hero">
        <div className="profile-portrait">
          <Avatar user={p.user} />
        </div>
        <div className="profile-info">
          <span className="eyebrow">个人主页</span>
          <h1>{p.user.username}</h1>
          <p>{p.user.bio || "暂无简介"}</p>
          <div className="profile-stats">
            <span>
              <strong>{p.videos}</strong>作品
            </span>
            <button onClick={() => setRelation("followers")}>
              <strong>{p.followers}</strong>粉丝
            </button>
            <button onClick={() => setRelation("following")}>
              <strong>{p.following}</strong>关注
            </button>
            <span>
              <strong>{p.likes}</strong>获赞
            </span>
          </div>
        </div>
        <div className="profile-buttons">
          {user?.id === p.user.id ? (
            <Button variant="secondary" onClick={() => navigate("settings")}>
              <Icon name="settings" />
              编辑资料
            </Button>
          ) : (
            <>
              <FollowButton
                id={p.user.id}
                user={user}
                alert={alert}
                requireUser={requireUser}
                onFollow={profile.reload}
              />
              <Button
                variant="secondary"
                onClick={() => {
                  if (requireUser()) navigate("messages", { peer: id });
                }}
              >
                <Icon name="message" />
                私信
              </Button>
            </>
          )}
        </div>
      </div>
      {relation && (
        <section className="relation-panel">
          <div className="panel-title">
            {relation === "followers" ? "粉丝" : "关注"}
            <button
              className="icon-button"
              aria-label="关闭列表"
              onClick={() => setRelation(null)}
            >
              <Icon name="close" />
            </button>
          </div>
          {relations.loading ? (
            <Loading />
          ) : relations.error ? (
            <ErrorState message={relations.error} retry={relations.reload} />
          ) : (
            <div className="relation-list">
              {relations.data?.length ? (
                relations.data.map((u) => (
                  <button
                    key={u.id}
                    onClick={() => navigate("profile", { id: String(u.id) })}
                  >
                    <Avatar user={u} />
                    {u.username}
                  </button>
                ))
              ) : (
                <p>暂无用户</p>
              )}
            </div>
          )}
        </section>
      )}
      <PageHeading title={user?.id === p.user.id ? "我的作品" : "作品"} />
      {videos.loading ? (
        <Loading cards />
      ) : videos.error ? (
        <ErrorState message={videos.error} retry={videos.reload} />
      ) : videos.data?.length ? (
        <VideoGrid
          videos={videos.data}
          open={(videoId) => navigate("watch", { id: String(videoId) })}
        />
      ) : (
        <Empty title="暂无作品" body="发布的视频会显示在这里。" />
      )}
    </section>
  );
}

function Account({
  navigate,
  alert,
  onLogin,
}: Common & { onLogin: () => Promise<void> }) {
  const [mode, setMode] = useState<"login" | "register">("login");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [invite, setInvite] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    setError("");
    try {
      await api<User>("/auth/" + mode, {
        method: "POST",
        body: json({ username, password, invite }),
      });
      await onLogin();
      alert(mode === "login" ? "登录成功" : "账号已创建");
      navigate("home");
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <section className="account-page">
      <div className="account-window">
        <div className="panel-title">
          <Icon name="video" />
          OWLET / 账号
          <span className="window-lights" aria-hidden="true">
            — □
          </span>
        </div>
        <div className="account-content">
          <div className="account-logo">
            owlet<span>®</span>
          </div>
          <h1>{mode === "login" ? "登录 Owlet" : "创建账号"}</h1>
          <p>
            {mode === "login"
              ? "登录后即可点赞、评论和发布视频。"
              : "使用一次性邀请码注册 Owlet。"}
          </p>
          <form onSubmit={submit}>
            {error && <ErrorState message={error} />}
            <label>
              用户名
              <input
                required
                minLength={3}
                maxLength={40}
                autoComplete="username"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                placeholder="输入用户名"
              />
            </label>
            <label>
              密码
              <input
                required
                type="password"
                minLength={mode === "register" ? 12 : 1}
                autoComplete={
                  mode === "register" ? "new-password" : "current-password"
                }
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder={mode === "register" ? "至少 12 位" : "输入密码"}
              />
            </label>
            {mode === "register" && (
              <label>
                邀请码
                <input
                  required
                  value={invite}
                  onChange={(e) => setInvite(e.target.value)}
                  placeholder="输入管理员提供的邀请码"
                />
              </label>
            )}
            <Button type="submit" disabled={busy}>
              {busy ? "处理中…" : mode === "login" ? "登录" : "创建账号"}
            </Button>
          </form>
          <button
            className="swap-mode"
            disabled={busy}
            onClick={() => {
              setMode(mode === "login" ? "register" : "login");
              setError("");
            }}
          >
            {mode === "login" ? "有邀请码？创建账号" : "已有账号？登录"}
          </button>
        </div>
        <div className="window-status">
          <span className="status-dot" />
          OWLET VIDEO
        </div>
      </div>
    </section>
  );
}

function Messages({ peer, navigate, user, alert }: Common & { peer?: string }) {
  const contacts = useResource<User[]>(user ? "/messages" : null);
  const following = useResource<User[]>(
    user ? "/users/" + user.id + "/following" : null,
  );
  const peerUser = useResource<ProfileData>(
    user && peer ? "/users/" + peer : null,
  );
  const thread = useResource<Message[]>(
    user && peer ? "/messages/" + peer : null,
  );
  const [text, setText] = useState("");
  const [sending, setSending] = useState(false);
  const end = useRef<HTMLDivElement>(null);
  useEffect(() => {
    end.current?.scrollIntoView({ block: "nearest" });
  }, [thread.data]);
  if (!user) return <LoginRequired navigate={navigate} />;
  const people = [
    ...(contacts.data || []),
    ...(following.data || []),
    ...(peerUser.data ? [peerUser.data.user] : []),
  ].filter((u, i, arr) => arr.findIndex((v) => v.id === u.id) === i);
  const send = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!peer || !text.trim() || sending) return;
    setSending(true);
    try {
      const m = await api<Message>("/messages/" + peer, {
        method: "POST",
        body: json({ body: text }),
      });
      thread.setData((old) => [...(old || []), m]);
      setText("");
    } catch (err) {
      alert(errorMessage(err));
    } finally {
      setSending(false);
    }
  };
  return (
    <section>
      <PageHeading title="私信" description="与关注的用户保持联系。" />
      <div className={`messages-layout ${peer ? "has-peer" : ""}`}>
        <aside>
          <div className="panel-title">
            <Icon name="people" />
            联系人
          </div>
          {contacts.loading || following.loading ? (
            <Loading />
          ) : (
            <>
              {(contacts.error || following.error) && (
                <ErrorState
                  message="联系人加载失败"
                  retry={() => {
                    contacts.reload();
                    following.reload();
                  }}
                />
              )}
              <div className="contact-list">
                {people.length
                  ? people.map((u) => (
                      <button
                        className={peer === String(u.id) ? "selected" : ""}
                        key={u.id}
                        onClick={() =>
                          navigate("messages", { peer: String(u.id) })
                        }
                      >
                        <Avatar user={u} />
                        <span>{u.username}</span>
                      </button>
                    ))
                  : !contacts.error &&
                    !following.error && (
                      <p className="subtle">
                        暂无联系人，可从用户主页发起私信。
                      </p>
                    )}
              </div>
            </>
          )}
        </aside>
        <section className="message-thread">
          <div className="thread-head">
            <button
              className="icon-button mobile-back"
              aria-label="返回联系人"
              onClick={() => navigate("messages")}
            >
              <Icon name="arrow" />
            </button>
            {peer
              ? people.find((u) => String(u.id) === peer)?.username ||
                "私信会话"
              : "选择联系人"}
          </div>
          <div className="thread-list">
            {peer ? (
              thread.loading ? (
                <Loading />
              ) : thread.error ? (
                <ErrorState message={thread.error} retry={thread.reload} />
              ) : thread.data?.length ? (
                thread.data.map((m) => (
                  <div
                    key={m.id}
                    className={
                      "bubble " + (m.senderId === user.id ? "mine" : "")
                    }
                  >
                    {m.body}
                    <time>{date(m.createdAt)}</time>
                  </div>
                ))
              ) : (
                <div className="quiet-state">
                  <Icon name="message" />
                  <p>暂无消息</p>
                  <span>发送消息开始聊天。</span>
                </div>
              )
            ) : (
              <div className="quiet-state">
                <Icon name="message" />
                <p>你的私信</p>
                <span>选择联系人查看对话。</span>
              </div>
            )}
            <div ref={end} />
          </div>
          <form onSubmit={send}>
            <label className="sr-only" htmlFor="message">
              消息内容
            </label>
            <input
              id="message"
              value={text}
              maxLength={2000}
              disabled={!peer}
              onChange={(e) => setText(e.target.value)}
              placeholder="输入消息…"
            />
            <Button
              type="submit"
              disabled={
                !peer ||
                !text.trim() ||
                sending ||
                thread.loading ||
                !!thread.error
              }
            >
              {sending ? "发送中…" : "发送"}
            </Button>
          </form>
        </section>
      </div>
    </section>
  );
}

function Notices({
  user,
  navigate,
  alert,
  onRead,
}: Common & { onRead: () => void }) {
  const notices = useResource<Notice[]>(user ? "/notifications" : null);
  const [busy, setBusy] = useState(false);
  const read = async (id?: number) => {
    setBusy(true);
    try {
      await api("/notifications/read", {
        method: "PATCH",
        body: json(id ? { id } : {}),
      });
      notices.setData((list) =>
        (list || []).map((n) =>
          !id || n.id === id ? { ...n, readAt: new Date().toISOString() } : n,
        ),
      );
      onRead();
      return true;
    } catch (e) {
      alert(errorMessage(e));
      return false;
    } finally {
      setBusy(false);
    }
  };
  if (!user) return <LoginRequired navigate={navigate} />;
  return (
    <section>
      <PageHeading title="通知" description="查看点赞、评论和新增关注。">
        <Button
          variant="secondary"
          disabled={busy || !notices.data?.some((n) => !n.readAt)}
          onClick={() => void read()}
        >
          <Icon name="check" />
          全部已读
        </Button>
      </PageHeading>
      {notices.loading ? (
        <Loading />
      ) : notices.error ? (
        <ErrorState message={notices.error} retry={notices.reload} />
      ) : notices.data?.length ? (
        <div className="notice-list">
          {notices.data.map((n) => (
            <button
              key={n.id}
              disabled={busy}
              className={!n.readAt ? "unread" : ""}
              onClick={async () => {
                if (!n.readAt && !(await read(n.id))) return;
                if (n.videoId) navigate("watch", { id: String(n.videoId) });
                else navigate("profile", { id: String(n.actorId) });
              }}
            >
              <span className="notice-symbol">
                <Icon
                  name={
                    n.kind.includes("like")
                      ? "heart"
                      : n.kind.includes("comment")
                        ? "message"
                        : "people"
                  }
                />
              </span>
              <span className="notice-copy">
                <strong>{n.body}</strong>
                <time>{date(n.createdAt)}</time>
              </span>
              {!n.readAt && <span className="unread-label">未读</span>}
            </button>
          ))}
        </div>
      ) : (
        <Empty
          title="暂无通知"
          body="收到点赞、评论或关注后，会在这里通知你。"
        />
      )}
    </section>
  );
}

function Settings({
  user,
  navigate,
  alert,
  refreshUser,
}: Common & { refreshUser: () => Promise<void> }) {
  const [bio, setBio] = useState(user?.bio || "");
  const [username, setUsername] = useState(user?.username || "");
  const [oldPassword, setOld] = useState("");
  const [newPassword, setNew] = useState("");
  const [busy, setBusy] = useState(false);
  if (!user) return <LoginRequired navigate={navigate} />;
  const run = async (work: () => Promise<void>) => {
    setBusy(true);
    try {
      await work();
    } catch (e) {
      alert(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <section>
      <PageHeading title="设置" description="管理个人资料和账号安全。" />
      <div className="settings-grid">
        <section className="panel">
          <div className="panel-title">
            <Icon name="people" />
            个人资料
          </div>
          <form
            className="panel-body"
            onSubmit={(e) => {
              e.preventDefault();
              void run(async () => {
                await api("/me", {
                  method: "PATCH",
                  body: json({ bio, username }),
                });
                await refreshUser();
                alert("资料已保存");
              });
            }}
          >
            <div className="settings-avatar">
              <Avatar user={user} />
              <label>
                更换头像
                <input
                  disabled={busy}
                  type="file"
                  accept="image/png,image/jpeg,image/webp"
                  onChange={(e) => {
                    const file = e.target.files?.[0];
                    if (!file) return;
                    void run(async () => {
                      const form = new FormData();
                      form.set("file", file);
                      await api("/me/avatar", { method: "POST", body: form });
                      await refreshUser();
                      alert("头像已更新");
                    });
                  }}
                />
              </label>
            </div>
            <label>
              用户名
              <input
                required
                minLength={3}
                maxLength={40}
                value={username}
                onChange={(e) => setUsername(e.target.value)}
              />
            </label>
            <label>
              简介
              <textarea
                maxLength={500}
                value={bio}
                onChange={(e) => setBio(e.target.value)}
                placeholder="介绍一下自己"
              />
            </label>
            <Button type="submit" disabled={busy}>
              保存资料
            </Button>
          </form>
        </section>
        <section className="panel">
          <div className="panel-title">
            <Icon name="settings" />
            账号安全
          </div>
          <form
            className="panel-body"
            onSubmit={(e) => {
              e.preventDefault();
              void run(async () => {
                await api("/auth/password", {
                  method: "PATCH",
                  body: json({ oldPassword, newPassword }),
                });
                setOld("");
                setNew("");
                alert("密码已更新");
              });
            }}
          >
            <label>
              当前密码
              <input
                required
                type="password"
                autoComplete="current-password"
                value={oldPassword}
                onChange={(e) => setOld(e.target.value)}
              />
            </label>
            <label>
              新密码
              <input
                required
                type="password"
                autoComplete="new-password"
                minLength={12}
                value={newPassword}
                onChange={(e) => setNew(e.target.value)}
                placeholder="至少 12 位"
              />
            </label>
            <Button type="submit" variant="secondary" disabled={busy}>
              修改密码
            </Button>
            <hr />
            <p className="subtle">退出后仍可浏览公开视频。</p>
            <Button
              variant="ghost"
              disabled={busy}
              onClick={() =>
                void run(async () => {
                  await api("/auth/logout", { method: "POST" });
                  await refreshUser();
                  navigate("home");
                })
              }
            >
              退出登录
            </Button>
          </form>
        </section>
      </div>
    </section>
  );
}

function Collection({
  mode,
  tag,
  navigate,
  user,
}: Common & { mode: "liked" | "tag"; tag?: string }) {
  const videos = useResource<Video[]>(
    mode === "liked"
      ? user
        ? "/me/likes"
        : null
      : tag
        ? "/tags/" + encodeURIComponent(tag) + "/videos"
        : null,
  );
  if (mode === "liked" && !user) return <LoginRequired navigate={navigate} />;
  return (
    <section>
      <PageHeading
        title={mode === "liked" ? "我的喜欢" : "#" + (tag || "话题")}
        description={
          mode === "liked" ? "你点赞过的视频。" : "浏览此话题下的视频。"
        }
      />
      {videos.loading ? (
        <Loading cards />
      ) : videos.error ? (
        <ErrorState message={videos.error} retry={videos.reload} />
      ) : videos.data?.length ? (
        <VideoGrid
          videos={videos.data}
          open={(id) => navigate("watch", { id: String(id) })}
        />
      ) : (
        <Empty
          title={mode === "liked" ? "暂无喜欢的视频" : "暂无相关视频"}
          body={
            mode === "liked"
              ? "点赞后，视频会显示在这里。"
              : "此话题下还没有视频。"
          }
          action="去发现"
          onAction={() => navigate("home")}
        />
      )}
    </section>
  );
}
