import { copy } from "@/config/copy";
import { vendors, type Model } from "@/config/models";

interface ModelCardProps {
  model: Model;
  checked: boolean;
  onToggle: (id: string) => void;
}

function vendorColor(id: Model["vendor"]): string {
  return vendors.find((vendor) => vendor.id === id)?.color ?? "#8797aa";
}

export function ModelCard({ model, checked, onToggle }: ModelCardProps) {
  const stats = [
    { label: copy.models.stats.context, value: model.context },
    { label: copy.models.stats.latency, value: model.latency },
    { label: copy.models.stats.input, value: model.inputPrice },
    { label: copy.models.stats.output, value: model.outputPrice },
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
            <h3 className="truncate font-mono text-sm font-medium text-ink">{model.name}</h3>
          </div>
          <p className="mt-1 text-[11.5px] text-faint">{model.vendorName}</p>
        </div>
        {model.badge ? (
          <span className="shrink-0 rounded-[4px] border border-cyan/30 px-1.5 py-px font-mono text-[10.5px] text-cyan">
            {model.badge}
          </span>
        ) : null}
      </div>

      <dl className="grid grid-cols-2 gap-x-3.5 gap-y-3 border-t border-line pt-3.5 xwide:gap-x-3 xwide:gap-y-2.5">
        {stats.map((stat) => (
          <div key={stat.label}>
            <dt className="text-[10.5px] leading-snug text-faint">{stat.label}</dt>
            <dd className="mt-0.5 font-mono text-[12.5px] tabular-nums text-ink-2">{stat.value}</dd>
          </div>
        ))}
      </dl>

      <div className="flex items-center justify-between gap-2.5">
        <label className="inline-flex cursor-pointer select-none items-center gap-[7px] text-xs text-muted">
          <span className="relative inline-flex size-3.5">
            <input
              type="checkbox"
              checked={checked}
              onChange={() => onToggle(model.id)}
              aria-label={`${copy.models.compare} ${model.name}`}
              className="peer size-3.5 cursor-pointer appearance-none rounded-[3px] border border-line-2 bg-transparent transition-colors checked:border-cyan checked:bg-cyan"
            />
            <svg
              aria-hidden="true"
              viewBox="0 0 12 12"
              className="pointer-events-none absolute left-1/2 top-1/2 size-2.5 -translate-x-1/2 -translate-y-1/2 text-[#03121a] opacity-0 peer-checked:opacity-100"
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
          aria-label={`${copy.models.call} ${model.name}`}
        >
          {copy.models.call}
        </a>
      </div>
    </article>
  );
}
