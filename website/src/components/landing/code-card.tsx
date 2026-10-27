"use client";

import { useCallback, useEffect, useRef, useState, type TransitionEvent } from "react";
import type { Copy } from "@/config/copy";

const DWELL_MS = 5500;
// The clone card is visually identical to the first one; if `transitionend` never
// arrives (zero-width track, reduced motion) this timer still restarts the loop.
const REWIND_FALLBACK_MS = 900;

type CopyResult = "idle" | "copied" | "failed";

export type CodeCardSample = {
  id: string;
  label: string;
  code: string;
  html: string;
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

type MethodCardProps = {
  copy: Copy;
  sample: CodeCardSample;
  active?: boolean;
  hidden?: boolean;
};

function MethodCard({ copy, sample, active = false, hidden = false }: MethodCardProps) {
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
        await navigator.clipboard.writeText(sample.code);
        succeeded = true;
      } else {
        succeeded = legacyCopy(sample.code);
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
    <article
      className="code-card flex flex-col"
      aria-hidden={hidden || undefined}
      aria-label={sample.label}
      data-active={active ? "true" : "false"}
    >
      <div className="code-card-head flex h-[46px] items-center justify-between gap-3 border-b border-line pl-4 pr-3.5">
        <div className="flex min-w-0 items-center gap-2.5 overflow-hidden font-mono text-xs text-muted">
          <span className="shrink-0 rounded border border-cyan/[0.28] bg-cyan/[0.07] px-1.5 py-px text-[11px] text-cyan">
            {sample.label}
          </span>
          <span className="hidden truncate min-[621px]:inline">
            <span className="text-faint">{copy.hero.method}</span> {copy.hero.endpoint}
          </span>
        </div>
        <button
          type="button"
          onClick={() => {
            void handleCopy();
          }}
          tabIndex={hidden ? -1 : undefined}
          aria-label={`${copy.hero.copyLabel} ${sample.label}`}
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

      <div className="code-block grow" dangerouslySetInnerHTML={{ __html: sample.html }} />

      <div className="code-card-trace border-t border-line px-4 pb-4 pt-3.5">
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
    </article>
  );
}

type CodeCardProps = {
  copy: Copy;
  samples: CodeCardSample[];
};

export function CodeCard({ copy, samples }: CodeCardProps) {
  const count = samples.length;
  // 0..count; `count` is the cloned first card used for the seamless loop.
  const [index, setIndex] = useState(0);
  const [animate, setAnimate] = useState(false);
  const [step, setStep] = useState(0);
  const [hovered, setHovered] = useState(false);
  const [focused, setFocused] = useState(false);
  const [pageHidden, setPageHidden] = useState(false);
  const [reducedMotion, setReducedMotion] = useState(false);
  const trackRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const track = trackRef.current;
    if (!track) {
      return;
    }

    const measure = () => {
      const [first, second] = Array.from(track.children) as HTMLElement[];
      if (!first || !second) {
        return;
      }
      setStep(second.offsetLeft - first.offsetLeft);
    };

    measure();
    if (typeof ResizeObserver === "undefined") {
      window.addEventListener("resize", measure);
      return () => window.removeEventListener("resize", measure);
    }

    const observer = new ResizeObserver(measure);
    observer.observe(track);
    Array.from(track.children).forEach((child) => observer.observe(child));
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    const query = window.matchMedia("(prefers-reduced-motion: reduce)");
    const sync = () => setReducedMotion(query.matches);
    sync();

    const onVisibility = () => setPageHidden(document.hidden);
    onVisibility();

    query.addEventListener("change", sync);
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      query.removeEventListener("change", sync);
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, []);

  const paused = hovered || focused || pageHidden;

  useEffect(() => {
    if (paused || index >= count) {
      return;
    }
    const timer = setTimeout(() => setIndex((current) => current + 1), DWELL_MS);
    return () => clearTimeout(timer);
  }, [paused, index, count]);

  const rewind = useCallback(() => {
    setAnimate(false);
    setIndex(0);
  }, []);

  useEffect(() => {
    if (index < count) {
      return;
    }
    const timer = setTimeout(rewind, REWIND_FALLBACK_MS);
    return () => clearTimeout(timer);
  }, [index, count, rewind]);

  // Re-enable the slide on the frame after the jump back, so it does not animate.
  useEffect(() => {
    if (animate || index !== 0) {
      return;
    }
    const frame = requestAnimationFrame(() => setAnimate(true));
    return () => cancelAnimationFrame(frame);
  }, [animate, index]);

  const handleTransitionEnd = (event: TransitionEvent<HTMLDivElement>) => {
    if (event.target !== event.currentTarget || event.propertyName !== "transform") {
      return;
    }
    if (index >= count) {
      rewind();
    }
  };

  return (
    <div
      className="code-carousel"
      onPointerEnter={() => setHovered(true)}
      onPointerLeave={() => setHovered(false)}
      onFocus={() => setFocused(true)}
      onBlur={(event) => {
        const next = event.relatedTarget;
        if (!(next instanceof Node) || !event.currentTarget.contains(next)) {
          setFocused(false);
        }
      }}
    >
      <div
        ref={trackRef}
        className="code-track"
        data-animate={animate && !reducedMotion ? "true" : "false"}
        style={{ transform: `translateX(-${index * step}px)` }}
        onTransitionEnd={handleTransitionEnd}
      >
        {samples.map((sample, cardIndex) => (
          <MethodCard
            key={sample.id}
            copy={copy}
            sample={sample}
            active={index === cardIndex}
          />
        ))}
        <MethodCard copy={copy} sample={samples[0]} hidden />
      </div>
    </div>
  );
}
