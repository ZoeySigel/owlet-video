'use client';

import { useState } from 'react';
import SparkMD5 from 'spark-md5';
import { api, json, type User, type Video } from './api';

const CHUNK = 5 * 1024 * 1024;
const MAX = 200 * 1024 * 1024;

async function hashFile(file: File, progress: (fraction:number)=>void): Promise<string> {
  const spark = new SparkMD5.ArrayBuffer();
  for (let start=0; start<file.size; start+=CHUNK) {
    spark.append(await file.slice(start,Math.min(start+CHUNK,file.size)).arrayBuffer());
    progress(Math.min((start+CHUNK)/file.size,1));
  }
  return spark.end();
}

export default function Publish({user,navigate,alert}:{user:User|null;navigate:(view:'account'|'watch',params?:Record<string,string>)=>void;alert:(message:string)=>void}) {
  const [file,setFile]=useState<File|null>(null);const [cover,setCover]=useState<File|null>(null);const [title,setTitle]=useState('');const [description,setDescription]=useState('');const [progress,setProgress]=useState(0);const [phase,setPhase]=useState('');const [busy,setBusy]=useState(false);
  const publish=async(e:React.FormEvent)=>{
    e.preventDefault();if(!file||!user)return;
    if(file.size>MAX||file.size<1){alert('请选择不超过 200 MB 的 MP4 视频');return}
    setBusy(true);
    try {
      setPhase('正在校验影像');
      const md5=await hashFile(file,f=>setProgress(Math.round(f*15)));
      const key=`owlet-upload:${user.id}:${md5}`;
      let uploadId=localStorage.getItem(key)||'';
      let uploaded:number[]=[];
      if(uploadId){try{const status=await api<{uploaded:number[];completed:boolean;published:boolean}>('/uploads/'+uploadId);if(status.published){uploadId=''}else if(status.completed){uploaded=Array.from({length:Math.ceil(file.size/CHUNK)},(_,i)=>i)}else{uploaded=status.uploaded}}catch{uploadId=''}}
      if(!uploadId){const init=await api<{id:string}>('/uploads',{method:'POST',body:json({md5,size:file.size,chunks:Math.ceil(file.size/CHUNK)})});uploadId=init.id;localStorage.setItem(key,uploadId)}
      const count=Math.ceil(file.size/CHUNK);const done=new Set(uploaded);let cursor=0;
      setPhase('正在上传影像');
      const worker=async()=>{while(cursor<count){const i=cursor++;if(done.has(i))continue;const part=file.slice(i*CHUNK,Math.min((i+1)*CHUNK,file.size));const bytes=await part.arrayBuffer();const hash=SparkMD5.ArrayBuffer.hash(bytes);await api('/uploads/'+uploadId+'/chunks/'+i,{method:'PUT',headers:{'X-Chunk-MD5':hash},body:part});done.add(i);setProgress(15+Math.round(done.size/count*70))}};
      await Promise.all(Array.from({length:Math.min(3,count)},worker));
      setPhase('正在整理分片');await api('/uploads/'+uploadId+'/complete',{method:'POST'});
      let coverId='';if(cover){setPhase('正在上传封面');const form=new FormData();form.set('file',cover);const result=await api<{coverId:string}>('/covers',{method:'POST',body:form});coverId=result.coverId}
      setPhase('正在发布');const video=await api<Video>('/videos',{method:'POST',body:json({uploadId,title,description,coverId})});
      localStorage.removeItem(key);setProgress(100);alert('影像已发布');navigate('watch',{id:String(video.id)});
    } catch(err) { alert((err as Error).message); setPhase('上传中断；重新提交可续传'); } finally { setBusy(false) }
  };
  if(!user)return <div className="content-shell publish-page"><div className="page-intro"><span className="eyebrow">SHARE A FIELD NOTE</span><h1>带来一段<em>新故事。</em></h1></div><div className="empty"><h3>先进入小屋</h3><p>发布影像需要邀请码注册的账号。</p><button className="button dark" onClick={()=>navigate('account')}>登录或注册 ↗</button></div></div>;
  return <div className="content-shell publish-page"><div className="page-intro"><span className="eyebrow">SHARE A FIELD NOTE / 发布影像</span><h1>把这一刻，<em>留在这里。</em></h1><p>像写下一张旅行明信片，给影像加上名字和几句话。</p></div><form className="publish-layout" onSubmit={publish}><div className="upload-area"><label className="dropzone"><span className="dropzone-symbol">✳</span><strong>{file?file.name:'选择一段影像'}</strong><small>MP4 · 最大 200 MB · 支持断点续传</small><input type="file" accept="video/mp4" required onChange={e=>setFile(e.target.files?.[0]||null)}/></label>{file&&<div className="file-meta"><span>{(file.size/1024/1024).toFixed(1)} MB</span><span>{phase||'准备上传'}</span></div>}{busy&&<div className="progress"><span style={{width:progress+'%'}}/></div>}</div><div className="publish-fields"><label>影像标题<input required maxLength={160} value={title} onChange={e=>setTitle(e.target.value)} placeholder="给这段旅程起个名字"/></label><label>故事简介<textarea maxLength={2000} value={description} onChange={e=>setDescription(e.target.value)} placeholder="发生了什么？可以用 #话题 给故事做标记。"/></label><label>封面图（可选）<input type="file" accept="image/jpeg,image/png,image/webp" onChange={e=>setCover(e.target.files?.[0]||null)}/></label><div className="publish-note">原始文件保存在 Owlet 服务器。请只发布你有权分享的内容。</div><button className="button dark full" disabled={busy||!file}>{busy?`${phase} · ${progress}%`:'发布这段影像 ↗'}</button></div></form></div>;
}
