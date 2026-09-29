"use client";

import { useEffect, useRef, type VideoHTMLAttributes } from "react";
import { api, json } from "./api";

/** Native player with the GCFeed playback/exposure protocol. Counts playing
 * wall time rather than currentTime, so seeking does not fabricate interest. */
export default function CorePlayback({ videoId, enabled, ...props }: VideoHTMLAttributes<HTMLVideoElement> & { videoId: number; enabled: boolean }) {
  const element = useRef<HTMLVideoElement>(null);
  useEffect(() => {
    const player = element.current;
    if (!enabled || !player) return;
    const requestID = crypto.randomUUID();
    const started = performance.now();
    let playingSince: number | null = null;
    let watchMs = 0;
    let firstFrameMs: number | undefined;
    let stutters = 0;
    let exposed = false;
    let completed = false;
    let disposed = false;
    let reported = false;
    const preloadElements: HTMLVideoElement[] = [];
    const stopClock = () => {
      if (playingSince !== null) watchMs += Math.max(0, performance.now() - playingSince);
      playingSince = null;
    };
    const event = (eventType: string) => {
      void api("/video-view-events", { method: "POST", keepalive: true, body: json({ video_id: videoId, scene: "watch", request_id: requestID, event_type: eventType, watch_ms: Math.round(watchMs), completed }) }).catch(() => {});
    };
    const playing = () => {
      if (document.hidden) return;
      playingSince ??= performance.now();
      firstFrameMs ??= Math.round(performance.now() - started);
      if (!exposed) { exposed = true; event("exposed"); }
    };
    const waiting = () => { stopClock(); if (exposed) stutters++; };
    const report = () => {
      stopClock();
      if (!exposed || reported) return;
      reported = true;
      event(completed ? "complete" : watchMs > 0 ? "play" : "skip");
      void api("/playback-qos-reports", { method: "POST", keepalive: true, headers: { "Idempotency-Key": requestID }, body: json({ video_id: videoId, first_frame_ms: firstFrameMs, stutter_count: stutters, watch_ms: Math.round(watchMs) }) }).catch(() => {});
    };
    const ended = () => { completed = true; report(); };
    const visibility = () => { if (document.hidden) stopClock(); else if (!player.paused && !player.ended) playing(); };
    player.addEventListener("playing", playing);
    player.addEventListener("pause", stopClock);
    player.addEventListener("waiting", waiting);
    player.addEventListener("seeking", stopClock);
    player.addEventListener("ended", ended);
    window.addEventListener("pagehide", report);
    document.addEventListener("visibilitychange", visibility);
    if (!player.paused && player.readyState >= 3) playing();
    void api<{ preload_count: number; buffer_ms: number }>("/playback-config?platform=web&network_type=default").then(async (config) => {
      if (disposed || config.preload_count <= 0) return;
      const data = await api<{ items: { media_url: string }[] }>(`/preload-videos?current_video_id=${videoId}&limit=${Math.min(3, config.preload_count)}`);
      if (disposed) return;
      for (const item of data.items || []) {
        const next = document.createElement("video");
        next.preload = "metadata";
        next.src = item.media_url;
        next.load();
        preloadElements.push(next);
      }
    }).catch(() => {});
    return () => {
      disposed = true;
      report();
      player.removeEventListener("playing", playing);
      player.removeEventListener("pause", stopClock);
      player.removeEventListener("waiting", waiting);
      player.removeEventListener("seeking", stopClock);
      player.removeEventListener("ended", ended);
      window.removeEventListener("pagehide", report);
      document.removeEventListener("visibilitychange", visibility);
      for (const next of preloadElements) { next.removeAttribute("src"); next.load(); }
    };
  }, [videoId, enabled]);
  return <video ref={element} {...props} />;
}
