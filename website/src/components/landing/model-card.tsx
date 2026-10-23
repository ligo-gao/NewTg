import type { Copy } from "@/config/copy";
import type { PricingMeta, PricingModel } from "@/lib/pricing-api";
import { formatPrice, formatTokenCount } from "@/lib/pricing-format";

interface ModelCardProps {
  copy: Copy;
  model: PricingModel;
  meta: PricingMeta | null;
  checked: boolean;
  onToggle: (id: string) => void;
}

const VENDOR_COLORS: Array<{ match: RegExp; color: string }> = [
  { match: /openai/i, color: "#10a37f" },
  { match: /anthropic|claude/i, color: "#d97757" },
  { match: /google|gemini/i, color: "#4285f4" },
  { match: /deepseek/i, color: "#4d6bfe" },
  { match: /meta|llama/i, color: "#0866ff" },
  { match: /阿里|通义|qwen/i, color: "#ff6a00" },
];

function vendorColor(vendor: string): string {
  return VENDOR_COLORS.find((entry) => entry.match.test(vendor))?.color ?? "var(--cyan)";
}

function badgeFor(model: PricingModel, copy: Copy): string | null {
  if (model.is_free) {
    return copy.models.badgeFree;
  }
  if (model.mode === "per_request") {
    return copy.models.badgePerRequest;
  }
  return null;
}

export function ModelCard({ copy, model, meta, checked, onToggle }: ModelCardProps) {
  const displayName = model.display_name || model.model_id;
  const badge = badgeFor(model, copy);
  const stats = [
    { label: copy.models.stats.context, value: formatTokenCount(model.context_length) },
    { label: copy.models.stats.maxOutput, value: formatTokenCount(model.max_output) },
    { label: copy.models.stats.input, value: formatPrice(model.prompt_price, meta, model.mode) },
    {
      label: copy.models.stats.output,
      value: formatPrice(model.completion_price, meta, model.mode),
    },
  ];

  return (
    <article className="flex flex-col gap-4 rounded-[10px] border border-line bg-linear-to-b from-panel to-bg-2 p-[18px] transition-[border-color,transform,background] duration-200 hover:-translate-y-0.5 hover:border-line-2 hover:from-panel-2">
      <div className="flex items-start justify-between gap-2.5">
        <div className="min-w-0">
          <div className="flex min-w-0 items-center gap-2">
            <span
              aria-hidden="true"
              className="size-1.5 shrink-0 rounded-full"
              style={{ backgroundColor: vendorColor(model.vendor) }}
            />
            <h3 className="truncate font-mono text-sm font-medium text-ink">{displayName}</h3>
          </div>
          <p className="mt-1 truncate text-[11.5px] text-faint">{model.vendor}</p>
        </div>
        {badge ? (
          <span className="shrink-0 rounded-[4px] border border-cyan/30 px-1.5 py-px font-mono text-[10.5px] text-cyan">
            {badge}
          </span>
        ) : null}
      </div>

      <dl className="grid grid-cols-2 gap-x-3.5 gap-y-3 border-t border-line pt-3.5 xwide:gap-x-3 xwide:gap-y-2.5">
        {stats.map((stat) => (
          <div key={stat.label}>
            <dt className="text-[10.5px] leading-snug text-faint">{stat.label}</dt>
            <dd className="mt-0.5 truncate font-mono text-[12.5px] tabular-nums text-ink-2">
              {stat.value}
            </dd>
          </div>
        ))}
      </dl>

      <div className="flex items-center justify-between gap-2.5">
        <label className="inline-flex cursor-pointer select-none items-center gap-[7px] text-xs text-muted">
          <span className="relative inline-flex size-3.5">
            <input
              type="checkbox"
              checked={checked}
              onChange={() => onToggle(model.model_id)}
              aria-label={`${copy.models.compare} ${displayName}`}
              className="peer size-3.5 cursor-pointer appearance-none rounded-[3px] border border-line-2 bg-transparent transition-colors checked:border-[var(--action-bg)] checked:bg-[var(--action-bg)]"
            />
            <svg
              aria-hidden="true"
              viewBox="0 0 12 12"
              className="pointer-events-none absolute left-1/2 top-1/2 size-2.5 -translate-x-1/2 -translate-y-1/2 text-on-action opacity-0 peer-checked:opacity-100"
            >
              <path
                d="M2 6.5 4.5 9 10 3"
                fill="none"
                stroke="currentColor"
                strokeWidth="1.8"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
          </span>
          {copy.models.compare}
        </label>
        <a
          className="text-xs text-cyan transition-colors hover:text-cyan-soft"
          href={copy.models.callHref}
          aria-label={`${copy.models.call} ${displayName}`}
        >
          {copy.models.call}
        </a>
      </div>
    </article>
  );
}
