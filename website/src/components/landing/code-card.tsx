"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { copy } from "@/config/copy";

type CopyResult = "idle" | "copied" | "failed";

type CodeCardProps = {
  html: string;
  code: string;
};

const RESET_DELAY_MS = 1800;

function legacyCopy(text: string): boolean {
  if (typeof document === "undefined") {
    return false;
  }

  const area = document.createElement("textarea");
  area.value = text;
  area.setAttribute("readonly", "");
  area.style.position = "fixed";
  area.style.top = "0";
  area.style.left = "0";
  area.style.opacity = "0";
  document.body.appendChild(area);

  let succeeded = false;
  try {
    area.select();
    succeeded =
      typeof document.execCommand === "function" && document.execCommand("copy");
  } catch {
    succeeded = false;
  } finally {
    document.body.removeChild(area);
  }

  return succeeded;
}

export function CodeCard({ html, code }: CodeCardProps) {
  const [result, setResult] = useState<CopyResult>("idle");
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const clearTimer = useCallback(() => {
    if (timerRef.current !== null) {
      clearTimeout(timerRef.current);
      timerRef.current = null;
    }
  }, []);

  useEffect(() => clearTimer, [clearTimer]);

  const handleCopy = async () => {
    clearTimer();

    let succeeded = false;
    try {
      if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(code);
        succeeded = true;
      } else {
        succeeded = legacyCopy(code);
      }
    } catch {
      succeeded = false;
    }

    if (succeeded) {
      setResult("copied");
      timerRef.current = setTimeout(() => {
        setResult("idle");
        timerRef.current = null;
      }, RESET_DELAY_MS);
    } else {
      setResult("failed");
    }
  };

  const label =
    result === "copied"
      ? copy.hero.copiedLabel
      : result === "failed"
        ? copy.hero.copyFailedLabel
        : copy.hero.copyLabel;

  const announcement =
    result === "copied"
      ? copy.hero.copySuccessAnnouncement
      : result === "failed"
        ? copy.hero.copyFailureAnnouncement
        : "";

  return (
    <div className="overflow-hidden rounded-[10px] border border-line-2 bg-[linear-gradient(180deg,#0c131d,#080d15)] shadow-[0_30px_70px_-30px_rgba(0,0,0,0.9),inset_0_0_0_1px_rgba(76,215,232,0.04)]">
      <div className="flex h-[46px] items-center justify-between gap-3 border-b border-line bg-white/[0.015] pl-4 pr-3.5">
        <div className="flex min-w-0 items-center gap-2.5 overflow-hidden font-mono text-xs text-muted">
          <span className="shrink-0 rounded border border-cyan/[0.28] bg-cyan/[0.07] px-1.5 py-px text-[11px] text-cyan">
            {copy.hero.method}
          </span>
          <span className="truncate">{copy.hero.endpoint}</span>
        </div>
        <button
          type="button"
          onClick={() => {
            void handleCopy();
          }}
          data-copied={result === "copied" ? "true" : undefined}
          className="inline-flex h-7 shrink-0 items-center gap-1.5 rounded-[5px] border border-line-2 px-2.5 text-xs text-muted transition-colors hover:border-cyan-dim hover:text-ink data-[copied=true]:border-green/40 data-[copied=true]:text-green"
        >
          <svg
            width="12"
            height="12"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.8"
            aria-hidden="true"
          >
            <rect x="9" y="9" width="12" height="12" rx="2" />
            <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" />
          </svg>
          <span>{label}</span>
        </button>
      </div>

      <div className="code-block" dangerouslySetInnerHTML={{ __html: html }} />

      <div className="border-t border-line bg-cyan/[0.022] px-4 pb-4 pt-3.5">
        <div className="flex items-center gap-2 font-mono text-[11.5px] text-muted">
          <span
            className="live-dot h-1.5 w-1.5 shrink-0 rounded-full bg-green"
            aria-hidden="true"
          />
          <span>{copy.hero.requestLabel}</span>
          <span>{copy.hero.okLabel}</span>
        </div>
        <dl className="mt-3 grid grid-cols-3 gap-3 max-[620px]:grid-cols-1 max-[620px]:gap-1.5">
          {copy.hero.trace.map((entry) => (
            <div key={entry.label}>
              <dt className="text-[11px] leading-[1.4] text-faint">{entry.label}</dt>
              <dd className="mt-[3px] whitespace-nowrap font-mono text-xs text-ink-2">
                {entry.value}
              </dd>
            </div>
          ))}
        </dl>
      </div>

      <span role="status" aria-live="polite" className="sr-only">
        {announcement}
      </span>
    </div>
  );
}
