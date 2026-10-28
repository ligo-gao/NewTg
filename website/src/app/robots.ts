import type { MetadataRoute } from "next";
import { site } from "@/config/site";

/** Machine endpoints live on the same host but are not pages to index. */
const MACHINE_PATHS = ["/api/", "/v1/", "/v1beta/", "/pg/", "/readyz", "/healthz"];

export const dynamic = "force-static";

export default function robots(): MetadataRoute.Robots {
  return {
    rules: [{ userAgent: "*", allow: "/", disallow: MACHINE_PATHS }],
    sitemap: `${site.url}/sitemap.xml`,
  };
}
