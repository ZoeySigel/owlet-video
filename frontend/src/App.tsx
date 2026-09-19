'use client';

import { useCallback, useEffect, useState } from 'react';
import { api, json, type Comment, type Feed, type Message, type Notice, type User, type Video } from './api';
import Publish from './Publish';

type View = 'home' | 'watch' | 'profile' | 'account' | 'publish' | 'messages' | 'notices' | 'settings' | 'liked' | 'tag';
type Route = { view: View; id?: string; tag?: string; peer?: string };
const parseRoute = (): Route => {
  if (typeof window === 'undefined') return { view: 'home' };
  const p = new URLSearchParams(window.location.search);
  const view = p.get('view') as View;
  return { view: ['home','watch','profile','account','publish','messages','notices','settings','liked','tag'].includes(view) ? view : 'home', id: p.get('id') || undefined, tag: p.get('tag') || undefined, peer: p.get('peer') || undefined };
};
const date = (value: string) => new Intl.DateTimeFormat('zh-CN', { month: 'short', day: 'numeric' }).format(new Date(value));
const portrait = (u?: User) => <span className="avatar" aria-hidden="true">{u?.avatarUrl ? <img src={u.avatarUrl} alt="" /> : (u?.username?.[0] || 'O').toUpperCase()}</span>;

export default function App() {
  const [route, setRoute] = useState<Route>({ view: 'home' });
  const [user, setUser] = useState<User | null>(null);
  const [noticeCount, setNoticeCount] = useState(0);
  const [toast, setToast] = useState('');
  useEffect(() => { setRoute(parseRoute()); const onPop = () => setRoute(parseRoute()); window.addEventListener('popstate', onPop); return () => window.removeEventListener('popstate', onPop); }, []);
  const refreshUser = useCallback(async () => { try { setUser(await api<User>('/auth/me')); } catch { try { await api('/auth/refresh', { method:'POST' }); setUser(await api<User>('/auth/me')); } catch { setUser(null); } } }, []);
  useEffect(() => { void refreshUser(); }, [refreshUser]);
  useEffect(() => {
    if (!user) return;
    const update = () => void api<{count:number}>('/notifications/unread').then(r => setNoticeCount(r.count)).catch(() => {});
    update(); const stream = new EventSource('/api/v1/notifications/stream', { withCredentials: true });
    stream.addEventListener('notification', update);
    return () => stream.close();
  }, [user]);
  const navigate = (view: View, params: Record<string, string> = {}) => {
    const p = new URLSearchParams({ view, ...params });
    window.history.pushState({}, '', view === 'home' && !Object.keys(params).length ? '/' : '/?' + p.toString());
    setRoute({ view, ...params }); window.scrollTo({ top: 0, behavior: 'smooth' });
  };
  const alert = (message: string) => { setToast(message); window.setTimeout(() => setToast(''), 4200); };
  const requireUser = () => { if (user) return true; alert('请先用邀请码注册或登录'); navigate('account'); return false; };
  const props = { navigate, alert, user, requireUser };

  return <>
    <header className="site-header">
      <button className="brand" onClick={() => navigate('home')} aria-label="Owlet 首页"><span className="brand-mark">✳</span><span>OWLET <small>影像札记</small></span></button>
      <nav aria-label="主导航">
        <button className={route.view==='home'?'active':''} onClick={() => navigate('home')}>发现</button>
        <button onClick={() => navigate('home', { sort: 'hot' })}>热榜</button>
        <button onClick={() => navigate('publish')}>发布</button>
        {user && <button onClick={() => navigate('messages')}>私信</button>}
      </nav>
      <div className="header-actions">
        {user ? <>
          <button className="notice-button" aria-label="通知" onClick={() => navigate('notices')}>✦{noticeCount > 0 && <sup>{noticeCount}</sup>}</button>
          <button className="account-pill" onClick={() => navigate('profile', { id: String(user.id) })}>{portrait(user)}<span>{user.username}</span></button>
        </> : <button className="join-button" onClick={() => navigate('account')}>进入小屋 ↗</button>}
      </div>
    </header>

    <main>
      {route.view === 'home' && <Home key={typeof window !== 'undefined' ? window.location.search : 'home'} {...props} />}
      {route.view === 'watch' && <Watch id={route.id} {...props} />}
      {route.view === 'profile' && <Profile id={route.id} {...props} />}
      {route.view === 'account' && <Account onLogin={refreshUser} {...props} />}
      {route.view === 'publish' && <Publish user={user} navigate={navigate} alert={alert} />}
      {route.view === 'messages' && <Messages peer={route.peer} {...props} />}
      {route.view === 'notices' && <Notices {...props} />}
      {route.view === 'settings' && <Settings onLogout={refreshUser} {...props} />}
      {route.view === 'liked' && <Liked {...props} />}
      {route.view === 'tag' && <Tag tag={route.tag} {...props} />}
    </main>
    <footer className="site-footer"><span>OWLET <i>✳</i> 影像札记</span><span>给每一段值得停留的瞬间。</span><span>© {new Date().getFullYear()}</span></footer>
    {toast && <div className="toast" role="status">{toast}</div>}
  </>;
}

type Common = { navigate: (view: View, params?: Record<string,string>) => void; alert: (message:string) => void; user: User|null; requireUser: () => boolean };

function Empty({ title, body, action, onAction }: { title:string; body:string; action?:string; onAction?:()=>void }) {
  return <div className="empty"><div className="empty-scene"><span className="empty-moon" /><span className="empty-hill one" /><span className="empty-hill two" /><span className="empty-bird">⌁</span></div><h3>{title}</h3><p>{body}</p>{action && <button className="button dark" onClick={onAction}>{action} ↗</button>}</div>;
}

function VideoCard({ video, navigate, index }: {video:Video; navigate:Common['navigate']; index:number}) {
  return <button className="video-card" onClick={() => navigate('watch', {id:String(video.id)})}>
    <span className="video-art">{video.coverUrl ? <img src={video.coverUrl} alt="" /> : <span className="video-art-fallback">✳</span>}<span className="play-glyph">▶</span></span>
    <span className="video-meta"><span className="index">{String(index+1).padStart(2,'0')} / FILM</span><span>{date(video.publishedAt)}</span></span>
    <strong>{video.title}</strong><span className="video-foot">{video.author?.username || '创作者'} <span>♡ {video.likesCount}</span></span>
  </button>;
}

function Home({ navigate, user }: Common) {
  const [sort, setSort] = useState<'latest'|'hot'|'likes'|'following'>(() => typeof window !== 'undefined' && window.location.search.includes('sort=hot') ? 'hot' : 'latest');
  const [items, setItems] = useState<Video[]>([]); const [cursor,setCursor]=useState(''); const [loading,setLoading]=useState(true); const [error,setError]=useState('');
  const load = useCallback(async (next='') => {
    setLoading(true); setError('');
    try { const r = await api<Feed>(`/videos?sort=${sort}${next ? '&cursor='+encodeURIComponent(next) : ''}`); setItems(old => next ? [...old,...r.items] : r.items); setCursor(r.nextCursor); }
    catch(e) { setError((e as Error).message); } finally { setLoading(false); }
  }, [sort]);
  useEffect(() => { void load(); }, [load]);
  return <>
    <section className="hero"><div className="hero-copy"><div className="eyebrow"><span className="asterisk">✳</span> A LITTLE WORLD IN MOTION <span>·</span> VOL. 01</div><h1>在风与<br /><em>故事之间。</em></h1><p>一处收藏流动影像与微小冒险的地方。翻开下一页，也许就是你未曾抵达的远方。</p><div className="hero-links"><button className="button dark" onClick={() => document.getElementById('feed')?.scrollIntoView({behavior:'smooth'})}>开始漫游 <span>↗</span></button><button className="text-link" onClick={() => navigate('publish')}>分享你的片刻 ↗</button></div><div className="hero-bottom"><span>SCROLL TO EXPLORE ↓</span><span>59° 54′ N &nbsp; 10° 45′ E</span></div></div><div className="hero-scene" aria-hidden="true"><div className="scene-disc" /><div className="scene-star star-a">✦</div><div className="scene-star star-b">✳</div><div className="scene-mountain far"/><div className="scene-mountain mid"/><div className="scene-mountain near"/><div className="scene-card"><span>FIELD NOTE / 001</span><strong>看看世界<br/>正发生什么</strong><small>OWLET STORIES</small></div><div className="scene-caption">一帧风景，一段旅程。</div></div></section>
    <section className="feed-section" id="feed"><div className="section-heading"><div><div className="eyebrow">THE MOVING ARCHIVE / 影像档案</div><h2>正在发生的<em>故事</em></h2></div><p>每一段影像都有自己的天气。<br/>挑选一条路，慢慢往前走。</p></div>
      <div className="tabs" role="tablist" aria-label="影像分类">{([['latest','最新'],['hot','热度'],['likes','最受喜欢'],['following','我的关注']] as const).map(([value,label])=><button key={value} role="tab" aria-selected={sort===value} className={sort===value?'selected':''} onClick={()=>setSort(value)}>{label}<span>↗</span></button>)}</div>
      {error && <div className="error-line">{error === 'login_required' ? '请先登录，查看关注的创作者。' : '暂时无法载入影像，请稍后重试。'} <button onClick={()=>void load()}>重试</button></div>}
      {items.length ? <><div className="video-grid">{items.map((v,i)=><VideoCard key={v.id} video={v} index={i} navigate={navigate}/>)}</div>{cursor && <button className="button outline load-more" disabled={loading} onClick={()=>void load(cursor)}>{loading?'正在寻找…':'继续浏览 ↓'}</button>}</> : !loading && !error && <Empty title={sort==='following'?'你的关注列表还很安静':'第一帧，还在路上'} body={sort==='following'?'去发现一些有趣的创作者吧。':'这里即将收录来自各处的影像。受邀创作者可以发布第一段故事。'} action={user?'发布第一段影像':'了解邀请制'} onAction={()=>navigate(user?'publish':'account')}/>}</section>
    <section className="editorial-strip"><span>✳</span><p>留意那些小小的、<br/><em>值得记住的瞬间。</em></p><button onClick={()=>navigate('publish')}>写下你的故事 ↗</button></section>
  </>;
}

function Watch({id,navigate,alert,user,requireUser}:Common & {id?:string}) {
  const [video,setVideo]=useState<Video|null>(null);const [comments,setComments]=useState<Comment[]>([]);const [text,setText]=useState('');const [liked,setLiked]=useState(false);const [error,setError]=useState('');
  useEffect(()=>{if(!id)return;void api<Video>('/videos/'+id).then(setVideo).catch(()=>setError('这段影像暂时找不到。'));void api<Comment[]>('/videos/'+id+'/comments').then(setComments).catch(()=>{});},[id]);
  useEffect(()=>{if(id&&user)void api<{liked:boolean}>('/videos/'+id+'/like').then(r=>setLiked(r.liked)).catch(()=>{});},[id,user]);
  const like=async()=>{if(!requireUser()||!video)return;try{await api('/videos/'+video.id+'/like',{method:liked?'DELETE':'PUT'});setLiked(!liked);setVideo({...video,likesCount:video.likesCount+(liked?-1:1)});}catch(e){alert((e as Error).message)}};
  const comment=async()=>{if(!requireUser()||!video||!text.trim())return;try{const result=await api<Comment>('/videos/'+video.id+'/comments',{method:'POST',body:json({body:text})});setComments([...comments,result]);setText('');}catch(e){alert((e as Error).message)}};
  if(error)return <div className="content-shell"><Empty title="影像不在这里" body={error} action="返回发现" onAction={()=>navigate('home')}/></div>;
  if(!video)return <div className="content-shell loading">正在寻找这段影像…</div>;
  return <div className="content-shell watch"><button className="back-link" onClick={()=>navigate('home')}>← 返回影像档案</button><div className="watch-layout"><div className="watch-main"><div className="player"><video src={video.playUrl} poster={video.coverUrl||undefined} controls playsInline autoPlay preload="metadata"/></div><div className="watch-title"><span className="eyebrow">A STORY BY {video.author?.username?.toUpperCase()}</span><h1>{video.title}</h1><p>{video.description.split(/(#[\p{L}\p{N}_]+)/u).map((part,i)=>part.startsWith('#')?<button className="inline-tag" key={i} onClick={()=>navigate('tag',{tag:part.slice(1)})}>{part}</button>:part)}</p><div className="watch-actions"><button onClick={like}>{liked?'♥':'♡'} {video.likesCount} 喜欢</button><button onClick={()=>navigator.clipboard.writeText(window.location.href).then(()=>alert('链接已复制'))}>↗ 分享影像</button></div></div></div><aside className="watch-side"><button className="author-row" onClick={()=>navigate('profile',{id:String(video.userId)})}>{portrait(video.author)}<span><small>CREATOR</small><strong>{video.author?.username}</strong></span><span>↗</span></button><div className="comments-head"><h3>沿途留言</h3><span>{comments.length} NOTES</span></div><div className="comment-list">{comments.length?comments.map(item=><div className="comment" key={item.id}>{portrait(item.author)}<div><strong>{item.author?.username}</strong><p>{item.body}</p><small>{date(item.createdAt)}</small>{user?.id===item.userId&&<button onClick={async()=>{await api('/comments/'+item.id,{method:'DELETE'});setComments(comments.filter(c=>c.id!==item.id))}}>删除</button>}</div></div>):<p className="subtle">还没有留言。写下第一句吧。</p>}</div><div className="comment-compose"><textarea placeholder="留下一句沿途感想…" value={text} onChange={e=>setText(e.target.value)} maxLength={1000}/><button className="button dark" onClick={comment}>发布留言 ↗</button></div></aside></div></div>;
}

function Profile({id,navigate,user,alert,requireUser}:Common & {id?:string}) {
  const [profile,setProfile]=useState<{user:User;videos:number;followers:number;following:number;likes:number}|null>(null);
  const [videos,setVideos]=useState<Video[]>([]);const [relations,setRelations]=useState<User[]|null>(null);
  useEffect(()=>{if(!id)return;void api<typeof profile>('/users/'+id).then(setProfile).catch(()=>{});void api<Video[]>('/users/'+id+'/videos').then(setVideos).catch(()=>{});},[id]);
  const follow=async()=>{if(!requireUser()||!profile)return;try{await api('/users/'+id+'/follow',{method:'PUT'});setProfile({...profile,followers:profile.followers+1});alert('已关注');}catch(e){alert((e as Error).message)}};
  if(!profile)return <div className="content-shell loading">正在寻找这位创作者…</div>;
  return <div className="content-shell profile-page"><button className="back-link" onClick={()=>navigate('home')}>← 返回发现</button><div className="profile-hero"><div className="profile-portrait">{portrait(profile.user)}</div><div><span className="eyebrow">CREATOR / OWLET COMMUNITY</span><h1>{profile.user.username}</h1><p>{profile.user.bio||'每个人都在用自己的方式，记录世界。'}</p><div className="profile-stats"><span><strong>{profile.videos}</strong>影像</span><button onClick={async()=>setRelations(await api<User[]>('/users/'+id+'/followers'))}><strong>{profile.followers}</strong>关注者</button><button onClick={async()=>setRelations(await api<User[]>('/users/'+id+'/following'))}><strong>{profile.following}</strong>正在关注</button><span><strong>{profile.likes}</strong>获赞</span></div></div><div className="profile-buttons">{user?.id===profile.user.id?<button className="button outline" onClick={()=>navigate('settings')}>编辑个人资料 ↗</button>:<><button className="button dark" onClick={follow}>＋ 关注</button><button className="button outline" onClick={()=>{if(requireUser())navigate('messages',{peer:id||''})}}>私信 ↗</button></>}</div></div>
    {relations&&<div className="relation-panel"><button onClick={()=>setRelations(null)}>关闭 ×</button>{relations.length?relations.map(u=><button key={u.id} onClick={()=>{setRelations(null);navigate('profile',{id:String(u.id)})}}>{portrait(u)}{u.username} ↗</button>):<p>这里暂时还是空的。</p>}</div>}
    <div className="section-heading small"><div><span className="eyebrow">THE COLLECTION</span><h2>他的<em>影像</em></h2></div></div>{videos.length?<div className="video-grid">{videos.map((v,i)=><VideoCard key={v.id} video={v} index={i} navigate={navigate}/>)}</div>:<Empty title="还没有发布影像" body="下一段故事也许正在路上。"/>}
  </div>;
}

function Account({navigate,alert,onLogin}:Common & {onLogin:()=>Promise<void>}) {
  const [mode,setMode]=useState<'login'|'register'>('login');const [username,setUsername]=useState('');const [password,setPassword]=useState('');const [invite,setInvite]=useState('');const [busy,setBusy]=useState(false);
  const submit=async(e:React.FormEvent)=>{e.preventDefault();setBusy(true);try{const user=await api<User>('/auth/'+mode,{method:'POST',body:json({username,password,invite})});await onLogin();alert(`欢迎来到 Owlet，${user.username}`);navigate('home');}catch(error){alert((error as Error).message)}finally{setBusy(false)}};
  return <div className="content-shell account-page"><div className="account-scene"><span className="eyebrow">A PLACE TO BELONG</span><h1>每段旅程，<br/><em>都始于一扇门。</em></h1><div className="account-moon"/><div className="account-hill"/><p>带上你的故事，来小屋坐坐。</p></div><div className="account-form"><div className="eyebrow">OWLET / 账号</div><h2>{mode==='login'?'欢迎回来':'加入影像小屋'}</h2><p>{mode==='login'?'继续你的影像旅程。':'注册需要一枚一次性邀请码。'}</p><form onSubmit={submit}><label>用户名<input required minLength={3} maxLength={40} autoComplete="username" value={username} onChange={e=>setUsername(e.target.value)} placeholder="your_name"/></label><label>密码<input required type="password" minLength={mode==='register'?12:1} autoComplete={mode==='register'?'new-password':'current-password'} value={password} onChange={e=>setPassword(e.target.value)} placeholder={mode==='register'?'至少 12 位':'你的密码'}/></label>{mode==='register'&&<label>邀请码<input required value={invite} onChange={e=>setInvite(e.target.value)} placeholder="从站长处取得"/></label>}<button className="button dark full" disabled={busy}>{busy?'请稍候…':mode==='login'?'登录小屋 ↗':'创建账号 ↗'}</button></form><button className="swap-mode" onClick={()=>setMode(mode==='login'?'register':'login')}>{mode==='login'?'已有邀请码？创建账号':'已有账号？返回登录'} ↗</button></div></div>;
}

function Messages({peer,navigate,user,alert}:Common & {peer?:string}) {
  const [contacts,setContacts]=useState<User[]>([]);const [thread,setThread]=useState<Message[]>([]);const [text,setText]=useState('');const [selected,setSelected]=useState(peer||'');const [discover,setDiscover]=useState<User[]>([]);
  useEffect(()=>{if(!user)return;void api<User[]>('/messages').then(setContacts).catch(()=>{});void api<User[]>('/users/'+user.id+'/following').then(setDiscover).catch(()=>{});},[user]);
  useEffect(()=>{setSelected(peer||'')},[peer]);
  useEffect(()=>{if(selected)void api<Message[]>('/messages/'+selected).then(setThread).catch(()=>{});},[selected]);
  const send=async(e:React.FormEvent)=>{e.preventDefault();if(!selected||!text.trim())return;try{const m=await api<Message>('/messages/'+selected,{method:'POST',body:json({body:text})});setThread([...thread,m]);setText('');}catch(err){alert((err as Error).message)}};
  if(!user)return <div className="content-shell"><Empty title="先进入小屋" body="私信只对受邀用户开放。" action="登录" onAction={()=>navigate('account')}/></div>;
  const people=[...contacts,...discover].filter((u,i,arr)=>arr.findIndex(v=>v.id===u.id)===i);
  return <div className="content-shell messages-page"><div className="page-intro"><span className="eyebrow">PRIVATE NOTES / 私信</span><h1>寄一封<em>小小的信。</em></h1></div><div className="messages-layout"><aside><h3>同行的人</h3>{people.length?people.map(u=><button className={selected===String(u.id)?'selected':''} key={u.id} onClick={()=>{setSelected(String(u.id));navigate('messages',{peer:String(u.id)})}}>{portrait(u)}<span>{u.username}</span>↗</button>):<p className="subtle">关注创作者后，就能从这里开始聊天。</p>}</aside><section><div className="thread-head">{selected?'与 '+(people.find(u=>String(u.id)===selected)?.username||'创作者')+' 的对话':'选择一个人，开始聊天'}</div><div className="thread-list">{thread.length?thread.map(m=><div key={m.id} className={'bubble '+(m.senderId===user.id?'mine':'')}>{m.body}<small>{date(m.createdAt)}</small></div>):<p className="subtle">还没有消息。说声你好吧。</p>}</div><form onSubmit={send}><input value={text} maxLength={2000} disabled={!selected} onChange={e=>setText(e.target.value)} placeholder="写下想说的话…"/><button className="button dark" disabled={!selected}>发送 ↗</button></form></section></div></div>;
}

function Notices({user,navigate,alert}:Common) {
  const [list,setList]=useState<Notice[]>([]);useEffect(()=>{if(user)void api<Notice[]>('/notifications').then(setList).catch(()=>{});},[user]);
  const read=async(id?:number)=>{try{await api('/notifications/read',{method:'PATCH',body:json(id?{id}:{})});setList(list.map(n=>!id||n.id===id?{...n,readAt:new Date().toISOString()}:n));}catch(e){alert((e as Error).message)}};
  if(!user)return <div className="content-shell"><Empty title="先进入小屋" body="通知只对受邀用户开放。" action="登录" onAction={()=>navigate('account')}/></div>;
  return <div className="content-shell notice-page"><div className="page-intro"><span className="eyebrow">LITTLE SIGNALS / 通知</span><h1>来自远方的<em>回音。</em></h1></div>{list.length?<><button className="text-link" onClick={()=>void read()}>全部标记已读 ↗</button><div className="notice-list">{list.map(n=><button key={n.id} className={!n.readAt?'unread':''} onClick={()=>{void read(n.id);if(n.videoId)navigate('watch',{id:String(n.videoId)});else navigate('profile',{id:String(n.actorId)})}}><span className="notice-symbol">✳</span><span><strong>{n.body}</strong><small>{date(n.createdAt)}</small></span><span>↗</span></button>)}</div></>:<Empty title="一切都很安静" body="有人喜欢、评论或关注时，回音会出现在这里。"/>}</div>;
}

function Settings({user,navigate,alert,onLogout}:Common & {onLogout:()=>Promise<void>}) {
  const [bio,setBio]=useState(user?.bio||'');const [username,setUsername]=useState(user?.username||'');const [oldPassword,setOld]=useState('');const [newPassword,setNew]=useState('');
  if(!user)return <div className="content-shell"><Empty title="先进入小屋" body="登录后可编辑个人资料。" action="登录" onAction={()=>navigate('account')}/></div>;
  const save=async()=>{try{await api('/me',{method:'PATCH',body:json({bio,username})});alert('资料已保存');}catch(e){alert((e as Error).message)}};
  const changePassword=async()=>{try{await api('/auth/password',{method:'PATCH',body:json({oldPassword,newPassword})});setOld('');setNew('');alert('密码已更新');}catch(e){alert((e as Error).message)}};
  return <div className="content-shell settings-page"><div className="page-intro"><span className="eyebrow">YOUR CORNER / 设置</span><h1>布置你的<em>小角落。</em></h1></div><div className="settings-grid"><section><h2>个人资料</h2><label>头像<input type="file" accept="image/png,image/jpeg,image/webp" onChange={async e=>{const file=e.target.files?.[0];if(!file)return;const form=new FormData();form.set('file',file);try{await api('/me/avatar',{method:'POST',body:form});alert('头像已更新')}catch(err){alert((err as Error).message)}}}/></label><label>用户名<input value={username} onChange={e=>setUsername(e.target.value)}/></label><label>简介<textarea maxLength={500} value={bio} onChange={e=>setBio(e.target.value)}/></label><button className="button dark" onClick={save}>保存资料 ↗</button></section><section><h2>账号安全</h2><label>当前密码<input type="password" value={oldPassword} onChange={e=>setOld(e.target.value)}/></label><label>新密码<input type="password" minLength={12} value={newPassword} onChange={e=>setNew(e.target.value)}/></label><button className="button outline" onClick={changePassword}>修改密码 ↗</button><hr/><button className="text-link" onClick={async()=>{await api('/auth/logout',{method:'POST'});await onLogout();navigate('home')}}>退出登录 ↗</button></section></div></div>;
}

function Liked({navigate,user}:Common) {
  const [videos,setVideos]=useState<Video[]>([]);useEffect(()=>{if(user)void api<Video[]>('/me/likes').then(setVideos).catch(()=>{});},[user]);
  return <div className="content-shell"><div className="page-intro"><span className="eyebrow">YOUR FAVOURITES</span><h1>喜欢的<em>影像。</em></h1></div>{videos.length?<div className="video-grid">{videos.map((v,i)=><VideoCard key={v.id} video={v} index={i} navigate={navigate}/>)}</div>:<Empty title="还没有收藏的喜欢" body="看见喜欢的影像，就点亮一颗心吧。" action="去发现" onAction={()=>navigate('home')}/>}</div>;
}

function Tag({tag,navigate}:Common & {tag?:string}) {
  const [videos,setVideos]=useState<Video[]>([]);useEffect(()=>{if(tag)void api<Video[]>('/tags/'+encodeURIComponent(tag)+'/videos').then(setVideos).catch(()=>{});},[tag]);
  return <div className="content-shell"><button className="back-link" onClick={()=>navigate('home')}>← 返回发现</button><div className="page-intro"><span className="eyebrow">A SHARED THREAD</span><h1>#{tag} <em>的故事。</em></h1></div>{videos.length?<div className="video-grid">{videos.map((v,i)=><VideoCard key={v.id} video={v} index={i} navigate={navigate}/>)}</div>:<Empty title="这个话题还很安静" body="或许你可以分享第一段相关的影像。"/>}</div>;
}
