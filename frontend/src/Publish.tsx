"use client";

import { useState } from "react";
import SparkMD5 from "spark-md5";
import {
  Button,
  Empty,
  ErrorState,
  Icon,
  PageHeading,
  errorMessage,
} from "./ui";
import { api, json, type User, type Video } from "./api";
import { uploadChunk, uploadProgress } from "./upload";

const CHUNK = 5 * 1024 * 1024;
const MAX = 200 * 1024 * 1024;

async function hashFile(
  file: File,
  progress: (fraction: number) => void,
): Promise<string> {
  const spark = new SparkMD5.ArrayBuffer();
  for (let start = 0; start < file.size; start += CHUNK) {
    spark.append(
      await file.slice(start, Math.min(start + CHUNK, file.size)).arrayBuffer(),
    );
    progress(Math.min((start + CHUNK) / file.size, 1));
  }
  return spark.end();
}

export default function Publish({
  user,
  navigate,
  alert,
}: {
  user: User | null;
  navigate: (
    view: "account" | "watch",
    params?: Record<string, string>,
  ) => void;
  alert: (message: string) => void;
}) {
  const [file, setFile] = useState<File | null>(null);
  const [cover, setCover] = useState<File | null>(null);
  const [title, setTitle] = useState("");
  const [description, setDescription] = useState("");
  const [progress, setProgress] = useState(0);
  const [phase, setPhase] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const publish = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!file || !user || busy) return;
    if (file.size > MAX || file.size < 1 || !/\.mp4$/i.test(file.name)) {
      alert("请选择不超过 200 MB 的 MP4 视频");
      return;
    }
    setBusy(true);
    setError("");
    setProgress(0);
    try {
      setPhase("正在校验视频");
      const md5 = await hashFile(file, (f) => setProgress(Math.round(f * 15)));
      setPhase("正在准备上传");
      const key = `owlet-upload:${user.id}:${md5}`;
      let uploadId = localStorage.getItem(key) || "";
      let uploaded: number[] = [];
      if (uploadId) {
        try {
          const status = await api<{
            uploaded: number[];
            completed: boolean;
            published: boolean;
          }>("/uploads/" + uploadId, { signal: AbortSignal.timeout(30000) });
          if (status.published) {
            uploadId = "";
          } else if (status.completed) {
            uploaded = Array.from(
              { length: Math.ceil(file.size / CHUNK) },
              (_, i) => i,
            );
          } else {
            uploaded = status.uploaded || [];
          }
        } catch (err) {
          if (err instanceof Error && ["upload_not_found", "invalid_upload"].includes(err.message)) uploadId = "";
          else throw err;
        }
      }
      if (!uploadId) {
        const init = await api<{ id: string }>("/uploads", {
          method: "POST",
          signal: AbortSignal.timeout(30000),
          body: json({
            md5,
            size: file.size,
            chunks: Math.ceil(file.size / CHUNK),
          }),
        });
        uploadId = init.id;
        localStorage.setItem(key, uploadId);
      }
      const count = Math.ceil(file.size / CHUNK);
      const done = new Set(uploaded);
      const inFlight = new Map<number, number>();
      const updateProgress = () => setProgress(uploadProgress(file.size, CHUNK, done, inFlight));
      const controller = new AbortController();
      let firstError: unknown;
      let cursor = 0;
      let stopped = false;
      setPhase("正在上传");
      updateProgress();
      const worker = async () => {
        try {
          while (cursor < count && !stopped) {
            const i = cursor++;
            if (done.has(i)) continue;
            const part = file.slice(
              i * CHUNK,
              Math.min((i + 1) * CHUNK, file.size),
            );
            const bytes = await part.arrayBuffer();
            const hash = SparkMD5.ArrayBuffer.hash(bytes);
            if (stopped) return;
            await uploadChunk("/uploads/" + uploadId + "/chunks/" + i, part, hash,
              controller.signal, (loaded) => { inFlight.set(i, loaded); updateProgress(); });
            done.add(i);
            inFlight.delete(i);
            updateProgress();
          }
        } catch (err) {
          if (!stopped) firstError = err;
          stopped = true;
          controller.abort();
          throw err;
        }
      };
      const results = await Promise.allSettled(
        Array.from({ length: Math.min(3, count) }, worker),
      );
      const failure = results.find(
        (r): r is PromiseRejectedResult => r.status === "rejected",
      );
      if (failure) throw firstError || failure.reason;
      setPhase("正在处理视频");
      await api("/uploads/" + uploadId + "/complete", { method: "POST", signal: AbortSignal.timeout(120000) });
      let coverId = "";
      if (cover) {
        setPhase("正在上传封面");
        const form = new FormData();
        form.set("file", cover);
        const result = await api<{ coverId: string }>("/covers", {
          method: "POST",
          signal: AbortSignal.timeout(120000),
          body: form,
        });
        coverId = result.coverId;
      }
      setPhase("正在发布");
      const video = await api<Video>("/videos", {
        method: "POST",
        signal: AbortSignal.timeout(30000),
        body: json({ uploadId, title, description, coverId }),
      });
      localStorage.removeItem(key);
      setProgress(100);
      alert("发布成功");
      navigate("watch", { id: String(video.id) });
    } catch (err) {
      setError(errorMessage(err));
      setPhase("上传中断；重新提交可续传");
    } finally {
      setBusy(false);
    }
  };
  if (!user)
    return (
      <section>
        <PageHeading title="发布视频" />
        <Empty
          title="请先登录"
          body="登录后即可发布视频。注册需要邀请码。"
          action="登录 / 注册"
          onAction={() => navigate("account")}
        />
      </section>
    );
  return (
    <section className="publish-page">
      <PageHeading
        title="发布视频"
        description="上传视频，填写标题和简介后发布。"
      />
      {error && <ErrorState message={error} />}
      <form className="publish-layout" onSubmit={publish}>
        <div className="upload-area">
          <label className="dropzone">
            <span className="dropzone-symbol">
              <Icon name="upload" />
            </span>
            <strong>{file ? file.name : "选择视频文件"}</strong>
            <small>MP4 · 最大 200 MB · 支持断点续传</small>
            <input
              disabled={busy}
              type="file"
              accept="video/mp4,.mp4"
              required
              onChange={(e) => {
                setFile(e.target.files?.[0] || null);
                setProgress(0);
                setPhase("");
                setError("");
              }}
            />
          </label>
          {file && (
            <div className="file-meta">
              <span>{(file.size / 1024 / 1024).toFixed(1)} MB</span>
              <span role="status">{phase || "准备上传"}</span>
            </div>
          )}
          {(busy || progress > 0) && (
            <div
              className="progress"
              role="progressbar"
              aria-label="视频上传进度"
              aria-valuemin={0}
              aria-valuemax={100}
              aria-valuenow={progress}
            >
              <span style={{ width: progress + "%" }} />
            </div>
          )}
        </div>
        <div className="publish-fields">
          <label>
            视频标题
            <input
              disabled={busy}
              required
              maxLength={160}
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              placeholder="输入视频标题"
            />
          </label>
          <label>
            视频简介
            <textarea
              disabled={busy}
              maxLength={2000}
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              placeholder="介绍视频内容，可使用 #话题 添加标签"
            />
          </label>
          <label>
            封面图（可选）
            <input
              disabled={busy}
              type="file"
              accept="image/jpeg,image/png,image/webp"
              onChange={(e) => setCover(e.target.files?.[0] || null)}
            />
          </label>
          <p className="publish-note">请发布你有权分享的内容。</p>
          <Button type="submit" disabled={busy || !file}>
            <Icon name="upload" />
            {busy ? `${phase} · ${progress}%` : "发布视频"}
          </Button>
        </div>
      </form>
    </section>
  );
}
