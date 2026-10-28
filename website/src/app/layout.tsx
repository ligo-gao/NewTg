import type { Metadata, Viewport } from "next";
import type { ReactNode } from "react";
import { Inter, JetBrains_Mono, Noto_Sans_SC } from "next/font/google";
import { LocalePreferenceRoute } from "@/components/locale-preference-route";
import { site } from "@/config/site";
import { THEME_INIT_SCRIPT } from "@/lib/theme";
// Shared public header/footer (packages/public-shell): the same stylesheet
// the SPA loads, so both apps render one shell.
import "@newtg/public-shell/styles.css";
import "./globals.css";

// 西文 Inter、中文 Noto Sans SC；字重全站只有 400 / 500 / 600 三档
// （docs/typography-00-排版标准.md）。CJK 仍优先系统苹方 / 雅黑，见 globals.css。
const inter = Inter({
  subsets: ["latin"],
  weight: ["400", "500", "600"],
  variable: "--font-inter",
  display: "swap",
});

const jetbrainsMono = JetBrains_Mono({
  subsets: ["latin"],
  weight: ["400", "500"],
  variable: "--font-jetbrains",
  display: "swap",
});

const notoSansSc = Noto_Sans_SC({
  subsets: ["latin"],
  weight: ["400", "500", "600"],
  variable: "--font-noto-sc",
  display: "swap",
});

const title = "NEWTG — 一个 API，调用所有主流模型";
const description =
  "统一路由 GPT、Claude、Gemini、DeepSeek 等主流模型，支持智能路由、自动降级与统一计费，一套代码随时切换，成本透明可控。";

export const metadata: Metadata = {
  metadataBase: new URL(site.url),
  title,
  description,
  alternates: {
    canonical: "/",
  },
  openGraph: {
    type: "website",
    siteName: site.name,
    locale: "zh_CN",
    url: "/",
    title,
    description,
    images: [
      {
        url: "/og.png",
        width: 1200,
        height: 630,
        alt: "NEWTG 首页：一个 API，调用所有主流模型",
      },
    ],
  },
  twitter: {
    card: "summary_large_image",
    title,
    description,
    images: ["/og.png"],
  },
  icons: {
    icon: "/icon.svg",
  },
};

export const viewport: Viewport = {
  themeColor: [
    { media: "(prefers-color-scheme: light)", color: "#f3f6f9" },
    { media: "(prefers-color-scheme: dark)", color: "#070b12" },
  ],
};

export default function RootLayout({ children }: Readonly<{ children: ReactNode }>) {
  return (
    <html
      lang="zh-CN"
      className={`${inter.variable} ${jetbrainsMono.variable} ${notoSansSc.variable}`}
      suppressHydrationWarning
    >
      <head>
        <script dangerouslySetInnerHTML={{ __html: THEME_INIT_SCRIPT }} />
      </head>
      <body>
        <LocalePreferenceRoute />
        {children}
      </body>
    </html>
  );
}
