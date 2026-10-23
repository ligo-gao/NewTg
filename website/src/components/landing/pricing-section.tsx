import { copy } from "@/config/copy";
import { models } from "@/config/models";

const container = "mx-auto w-full max-w-[1520px] px-4 min-[621px]:px-6 wide:px-10";

function factClasses(index: number): string {
  if (index === 0) {
    return "bg-bg-2 px-[22px] py-5";
  }
  return "border-t border-line bg-bg-2 px-[22px] py-5 nav:border-t-0 nav:border-l nav:border-line";
}

export function PricingSection() {
  return (
    <section id="pricing" className="scroll-mt-20 border-t border-line py-16">
      <div className={container}>
        <div className="mb-7">
          <h2 className="text-[clamp(1.7rem,3vw,2.4rem)] leading-[1.25] font-semibold tracking-[-0.03em]">
            {copy.pricing.title}
          </h2>
          <p className="mt-2.5 max-w-[34em] text-[14.5px] text-muted">
            {copy.pricing.subtitle}
          </p>
        </div>

        <dl className="grid grid-cols-1 overflow-hidden rounded-lg border border-line nav:grid-cols-3">
          {copy.pricing.facts.map((fact, index) => (
            <div key={fact.title} className={factClasses(index)}>
              <dt className="text-[13px] font-semibold text-ink">{fact.title}</dt>
              <dd className="mt-1.5 text-[12.5px] leading-[1.65] text-muted">
                {fact.description}
              </dd>
            </div>
          ))}
        </dl>

        <div className="mt-6 overflow-x-auto rounded-lg border border-line bg-bg-2">
          <table className="w-full min-w-[660px] border-collapse text-[13.5px]">
            <caption className="caption-top border-b border-line px-5 py-4 text-left text-[12.5px] text-muted">
              {copy.pricing.caption}
            </caption>
            <thead>
              <tr>
                <th
                  scope="col"
                  className="border-b border-line px-5 py-3 text-left text-[11.5px] font-medium whitespace-nowrap text-faint"
                >
                  {copy.pricing.columns.model}
                </th>
                <th
                  scope="col"
                  className="border-b border-line px-5 py-3 text-left text-[11.5px] font-medium whitespace-nowrap text-faint"
                >
                  {copy.pricing.columns.vendor}
                </th>
                <th
                  scope="col"
                  className="border-b border-line px-5 py-3 text-left text-[11.5px] font-medium whitespace-nowrap text-faint"
                >
                  {copy.pricing.columns.input}
                </th>
                <th
                  scope="col"
                  className="border-b border-line px-5 py-3 text-left text-[11.5px] font-medium whitespace-nowrap text-faint"
                >
                  {copy.pricing.columns.output}
                </th>
              </tr>
            </thead>
            <tbody className="[&>tr:last-child>td]:border-b-0">
              {models.map((model) => (
                <tr key={model.id} className="transition-colors hover:bg-white/[0.018]">
                  <td className="border-b border-line px-5 py-3.5 font-mono text-[12.5px] text-ink">
                    {model.name}
                  </td>
                  <td className="border-b border-line px-5 py-3.5 text-muted">
                    {model.vendorName}
                  </td>
                  <td className="border-b border-line px-5 py-3.5 font-mono text-[12.5px] text-ink tabular-nums">
                    {model.inputPrice}
                  </td>
                  <td className="border-b border-line px-5 py-3.5 font-mono text-[12.5px] text-ink tabular-nums">
                    {model.outputPrice}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>

        <p className="mt-3.5 text-[11.5px] text-faint">{copy.pricing.footnote}</p>
      </div>
    </section>
  );
}
