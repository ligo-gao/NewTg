import path from "node:path";
import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  output: "export",
  trailingSlash: true,
  images: { unoptimized: true },
  // The public header/footer live in ../packages/public-shell and are shared
  // with the SPA; Next must transpile that source and look for it from the
  // repository root rather than this app's folder.
  transpilePackages: ["@newtg/public-shell"],
  turbopack: {
    root: path.resolve(__dirname, ".."),
  },
};

export default nextConfig;
