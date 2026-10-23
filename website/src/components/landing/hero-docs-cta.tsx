"use client";

import type { Copy } from "@/config/copy";
import { site } from "@/config/site";
import { usePublicStatus } from "@/lib/public-status";

/**
 * Hero's secondary CTA. Docs are only reachable when the deployment exposes
 * them, so the link is withheld until `/api/status` proves it is configured.
 */
export function HeroDocsCta({ copy }: { copy: Copy }) {
  const { docsEnabled } = usePublicStatus();

  if (!docsEnabled) return null;

  return (
    <a className="btn btn-ghost" href={site.docsHref}>
      {copy.hero.secondaryCta}
    </a>
  );
}

export default HeroDocsCta;
