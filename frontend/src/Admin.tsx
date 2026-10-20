"use client";

import { useEffect, useRef, useState, type FormEvent, type ReactNode } from "react";
import { api, json, type User } from "./api";
import { Avatar, Button, Empty, ErrorState, Icon, Loading, errorMessage, useResource } from "./ui";

type ManagedUser = User & { role: string; status: number; videoCount: number };
type ManagedVideo = { id: number; userId: number; username: string; title: string; description: string; status: number; playUrl: string; coverUrl: string; size: number; likesCount: number; commentsCount: number; createdAt: string };
type Overview = { users: number; disabledUsers: number; videos: number; publishedVideos: number; offlineVideos: number; mediaBytes: number };
type Page<T> = { items: T[]; total: number; page: number; pageSize: number };
type Editor = { kind: "user"; item: ManagedUser } | { kind: "password"; item: ManagedUser } | { kind: "video"; item: ManagedVideo } | { kind: "preview"; item: ManagedVideo } | { kind: "user-status"; item: ManagedUser } | { kind: "video-status"; item: ManagedVideo };
const formatDate = (date: string) => new Date(date).toLocaleDateString("zh-CN");
function size(bytes: number) { return bytes >= 1024 ** 3 ? `${(bytes / 1024 ** 3).toFixed(2)} GB` : `${(bytes / 1024 ** 2).toFixed(1)} MB`; }

export default function Admin({ user, onLogin, refreshUser, notify }: { user: User | null; onLogin: () => void; refreshUser: () => Promise<void>; notify: (message: string) => void }) {
  if (!user) return <Empty title="登录管理后台" body="请使用已授权的管理员账号登录。" action="前往登录" onAction={onLogin} />;
  if (!user.isAdmin) return <Empty title="需要管理员权限" body="当前账号尚未获得授权，请联系站点管理员。" />;
  return <AdminWorkspace key={user.id} user={user} refreshUser={refreshUser} notify={notify} />;
}

function AdminWorkspace({ user, refreshUser, notify }: { user: User; refreshUser: () => Promise<void>; notify: (message: string) => void }) {
  const [tab, setTab] = useState<"users" | "videos">("users");
  const [draft, setDraft] = useState("");
  const [query, setQuery] = useState("");
  const [status, setStatus] = useState("");
  const [page, setPage] = useState(1);
  const [editor, setEditor] = useState<Editor | null>(null);
  const overview = useResource<Overview>("/admin/overview");
  const params = new URLSearchParams({ q: query, status, page: String(page) });
  const users = useResource<Page<ManagedUser>>(tab === "users" ? `/admin/users?${params}` : null);
  const videos = useResource<Page<ManagedVideo>>(tab === "videos" ? `/admin/videos?${params}` : null);
  const current = tab === "users" ? users : videos;
  function reload() { users.reload(); videos.reload(); overview.reload(); }
  function switchTab(next: "users" | "videos") { setTab(next); setStatus(""); setPage(1); setQuery(""); setDraft(""); }
  async function saved(reauthenticate = false) {
    setEditor(null); reload();
    if (reauthenticate) { notify("已保存，请重新登录。"); await refreshUser(); } else { notify("更改已保存。"); await refreshUser(); }
  }
  const total = current.data?.total ?? 0;
  return <section className="admin-workspace">
    <div className="admin-heading"><div><span className="admin-eyebrow">OWLET / CONTROL ROOM</span><h1>管理后台<span className="admin-live"><i />管理员</span></h1><p>管理社区账号与视频，让每一条内容各得其所。</p></div><Button variant="secondary" onClick={reload}><Icon name="arrow" />刷新数据</Button></div>
    {overview.error && <ErrorState message={overview.error} retry={overview.reload} />}
    <div className="admin-overview" aria-label="站点概况">
      <Metric label="社区账号" value={overview.data?.users} detail={`${overview.data?.disabledUsers ?? "—"} 个已停用`} icon="people" />
      <Metric label="视频总数" value={overview.data?.videos} detail={`${overview.data?.publishedVideos ?? "—"} 个正在展示`} icon="video" />
      <Metric label="已下架视频" value={overview.data?.offlineVideos} detail="保留内容，可随时恢复" icon="grid" />
      <Metric label="视频文件总量" value={overview.data ? size(overview.data.mediaBytes) : "—"} detail="包含保留的视频文件" icon="upload" />
    </div>
    <div className="admin-panel">
      <div className="admin-panel-top"><div className="admin-tabs" role="tablist" aria-label="管理内容"><button role="tab" aria-selected={tab === "users"} onClick={() => switchTab("users")}><Icon name="people" />账号管理</button><button role="tab" aria-selected={tab === "videos"} onClick={() => switchTab("videos")}><Icon name="video" />视频管理</button></div><span className="admin-total">{current.loading ? "正在加载" : `共 ${total} 条记录`}</span></div>
      <form className="admin-filters" onSubmit={e => { e.preventDefault(); setQuery(draft.trim()); setPage(1); }}>
        <label className="admin-search"><Icon name={tab === "users" ? "people" : "video"} /><span className="sr-only">搜索{tab === "users" ? "账号" : "视频"}</span><input maxLength={160} value={draft} onChange={e => setDraft(e.target.value)} placeholder={tab === "users" ? "搜索用户名或账号 ID" : "搜索视频标题、作者或视频 ID"} /></label>
        <label className="admin-select"><span className="sr-only">状态筛选</span><select value={status} onChange={e => { setStatus(e.target.value); setPage(1); }}><option value="">全部状态</option>{tab === "users" ? <><option value="1">正常账号</option><option value="0">已停用</option></> : <><option value="2">展示中</option><option value="3">已下架</option></>}</select></label>
        <Button type="submit">搜索</Button>
        {(query || status) && <Button variant="ghost" onClick={() => { setQuery(""); setDraft(""); setStatus(""); setPage(1); }}>清除筛选</Button>}
      </form>
      {current.error ? <ErrorState message={current.error} retry={current.reload} /> : current.loading ? <Loading /> : !total ? <Empty title="没有匹配的记录" body="尝试其他关键词，或清除筛选查看全部记录。" /> : <div className="admin-table-wrap"><table role="table" aria-label={tab === "users" ? "账号列表" : "视频列表"} className={`admin-table admin-table-${tab}`}><thead>{tab === "users" ? <tr role="row"><th>账号</th><th>状态</th><th>视频数</th><th>注册时间</th><th className="admin-actions-head">管理操作</th></tr> : <tr role="row"><th>视频内容</th><th>作者</th><th>状态</th><th>互动</th><th className="admin-actions-head">管理操作</th></tr>}</thead><tbody role="rowgroup">
        {tab === "users" ? users.data?.items.map(item => <tr role="row" key={item.id}><td role="cell"><div className="admin-identity"><Avatar user={item} /><div><strong>{item.username}{item.id === user.id && <span className="admin-self">你</span>}</strong><small>#{item.id} · {item.role === "admin" ? "管理员" : "社区成员"}</small></div></div></td><td role="cell" data-label="状态"><Status active={item.status === 1}>{item.status === 1 ? "正常" : "已停用"}</Status></td><td role="cell" className="admin-number" data-label="视频数">{item.videoCount}</td><td role="cell" className="admin-date" data-label="注册时间">{formatDate(item.createdAt)}</td><td role="cell"><div className="admin-row-actions"><button onClick={() => setEditor({ kind: "user", item })}>编辑资料</button><button onClick={() => setEditor({ kind: "password", item })}>重置密码</button><button disabled={item.role === "admin"} title={item.role === "admin" ? "管理员账号不能停用" : undefined} className={item.status === 1 ? "admin-danger" : ""} onClick={() => setEditor({ kind: "user-status", item })}>{item.status === 1 ? "停用" : "恢复"}</button></div></td></tr>) : videos.data?.items.map(item => <tr role="row" key={item.id}><td role="cell"><button className="admin-video-cell" onClick={() => setEditor({ kind: "preview", item })}><span className="admin-thumbnail">{item.coverUrl ? <img src={item.coverUrl} alt="" /> : <Icon name="video" />}<span><Icon name="play" /></span></span><span><strong>{item.title}</strong><small>#{item.id} · {size(item.size)} · {formatDate(item.createdAt)}</small></span></button></td><td role="cell" data-label="作者"><strong className="admin-author">{item.username}</strong><small className="admin-subtext">账号 #{item.userId}</small></td><td role="cell" data-label="状态"><Status active={item.status === 2}>{item.status === 2 ? "展示中" : "已下架"}</Status></td><td role="cell" data-label="互动"><small className="admin-subtext">{item.likesCount} 点赞<br />{item.commentsCount} 评论</small></td><td role="cell"><div className="admin-row-actions"><button onClick={() => setEditor({ kind: "preview", item })}>预览</button><button onClick={() => setEditor({ kind: "video", item })}>编辑</button><button className={item.status === 2 ? "admin-danger" : ""} onClick={() => setEditor({ kind: "video-status", item })}>{item.status === 2 ? "下架" : "恢复展示"}</button></div></td></tr>)}
      </tbody></table></div>}
      <div className="admin-pagination"><span>每页 20 条{total > 0 && ` · 第 ${page} / ${Math.ceil(total / 20)} 页`}</span><div><Button variant="secondary" disabled={current.loading || page <= 1} onClick={() => setPage(p => p - 1)}>上一页</Button><Button variant="secondary" disabled={current.loading || page * 20 >= total} onClick={() => setPage(p => p + 1)}>下一页</Button></div></div>
    </div>
    <p className="admin-footnote"><Icon name="check" />停用账号会撤销登录会话；下架视频保留原文件，便于恢复。</p>
    {editor && <AdminEditor key={`${editor.kind}-${editor.item.id}`} editor={editor} close={() => setEditor(null)} saved={saved} />}
  </section>;
}

function Status({ active, children }: { active: boolean; children: ReactNode }) { return <span className={`admin-status ${active ? "is-active" : ""}`}><i />{children}</span>; }
function Metric({ label, value, detail, icon }: { label: string; value: string | number | undefined; detail: string; icon: string }) { return <div className="admin-metric"><div><span>{label}</span><Icon name={icon} /></div><strong>{value ?? "—"}</strong><small>{detail}</small></div>; }

function AdminEditor({ editor, close, saved }: { editor: Editor; close: () => void; saved: (reauthenticate?: boolean) => Promise<void> }) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [name, setName] = useState(editor.kind === "user" ? editor.item.username : editor.kind === "video" ? editor.item.title : "");
  const [body, setBody] = useState(editor.kind === "user" ? editor.item.bio : editor.kind === "video" ? editor.item.description : "");
  const [password, setPassword] = useState("");
  useEffect(() => { const el = dialog.current; const focused = document.activeElement as HTMLElement | null; el?.showModal(); return () => { el?.close(); focused?.focus(); }; }, []);
  const titles = { user: "编辑账号资料", password: "重置账号密码", video: "编辑视频", preview: "视频预览", "user-status": editor.item.status === 1 ? "停用账号" : "恢复账号", "video-status": editor.item.status === 2 ? "下架视频" : "恢复视频展示" };
  async function submit(e: FormEvent) {
    e.preventDefault(); setBusy(true); setError("");
    try {
      const isUser = editor.kind === "user" || editor.kind === "password" || editor.kind === "user-status";
      const path = `/admin/${isUser ? "users" : "videos"}/${editor.item.id}${editor.kind === "password" ? "/password" : ""}`;
      const data = editor.kind === "user" ? { username: name, bio: body } : editor.kind === "password" ? { password } : editor.kind === "video" ? { title: name, description: body } : editor.kind === "user-status" ? { status: editor.item.status === 1 ? 0 : 1 } : { status: editor.item.status === 2 ? 3 : 2 };
      const result = await api<{ reauthenticate?: boolean }>(path, { method: editor.kind === "password" ? "POST" : "PATCH", body: json(data) });
      await saved(result.reauthenticate);
    } catch (err) { setError(errorMessage(err)); setBusy(false); }
  }
  return <dialog aria-labelledby="admin-dialog-title" className={`admin-dialog ${editor.kind === "preview" ? "admin-preview-dialog" : ""}`} ref={dialog} onCancel={e => { e.preventDefault(); if (!busy) close(); }} onClick={e => { if (e.target === e.currentTarget && !busy) close(); }}>
    <div className="admin-dialog-heading"><div><span className="admin-eyebrow">记录 #{editor.item.id}</span><h2 id="admin-dialog-title">{titles[editor.kind]}</h2></div><button className="icon-button" aria-label="关闭弹窗" disabled={busy} onClick={close}><Icon name="close" /></button></div>
    {editor.kind === "preview" ? <div className="admin-preview"><video controls playsInline preload="metadata" src={editor.item.playUrl} poster={editor.item.coverUrl || undefined} /><h3>{editor.item.title}</h3><p>{editor.item.description || "暂无简介"}</p><small>由 {editor.item.username} 发布 · {formatDate(editor.item.createdAt)}</small></div> : <form onSubmit={submit} className="admin-edit-form">
      {(editor.kind === "user" || editor.kind === "video") && <><label>{editor.kind === "user" ? "用户名" : "视频标题"}<input autoFocus required maxLength={editor.kind === "user" ? 40 : 160} minLength={editor.kind === "user" ? 3 : 1} pattern={editor.kind === "user" ? "[a-zA-Z0-9_]{3,40}" : undefined} value={name} onChange={e => setName(e.target.value)} disabled={busy} /></label><label>{editor.kind === "user" ? "个人简介" : "视频简介"}<textarea rows={5} maxLength={editor.kind === "user" ? 500 : 2000} value={body} onChange={e => setBody(e.target.value)} disabled={busy} /></label>{editor.kind === "user" && <p className="admin-form-note">修改用户名后，该账号需要重新登录。</p>}</>}
      {editor.kind === "password" && <><p>为 <strong>{editor.item.username}</strong> 设置新密码，所有已登录设备将退出。</p><label>新密码<input autoFocus required type="password" minLength={12} maxLength={72} autoComplete="new-password" value={password} onChange={e => setPassword(e.target.value)} disabled={busy} /></label><p className="admin-form-note">密码需为 12–72 字节，请通过可靠方式告知账号持有人。</p></>}
      {editor.kind === "user-status" && <p>{editor.item.status === 1 ? <>停用 <strong>{editor.item.username}</strong> 后，该账号将退出登录，无法登录、发布或互动。资料和视频会保留。</> : <>恢复后，<strong>{editor.item.username}</strong> 可以重新登录并使用账号。</>}</p>}
      {editor.kind === "video-status" && <p>{editor.item.status === 2 ? <>将「<strong>{editor.item.title}</strong>」从站内列表及详情中下架。原媒体链接仍可访问，视频文件和互动记录会保留。</> : <>恢复「<strong>{editor.item.title}</strong>」的站内展示，重新进入内容列表。</>}</p>}
      {error && <div role="alert" className="error-line">{error}</div>}
      <div className="admin-dialog-actions"><Button variant="secondary" onClick={close} disabled={busy}>取消</Button><Button type="submit" disabled={busy}>{busy ? "正在保存…" : editor.kind.endsWith("status") ? "确认更改" : "保存更改"}</Button></div>
    </form>}
  </dialog>;
}
