import type { ReactNode } from "react";
import { THEME_INIT_SCRIPT } from "@/lib/theme";

/**
 * 唯一的文档骨架。两个根布局（默认语言与 `/<locale>/`）各自传入自己的
 * `lang`，静态导出的 HTML 在首帧就带正确的语言，不再靠水合后改属性。
 */
export function RootDocument({
  lang,
  fontClassName,
  children,
}: {
  lang: string;
  fontClassName: string;
  children: ReactNode;
}) {
  return (
    <html lang={lang} className={fontClassName} suppressHydrationWarning>
      <head>
        <script dangerouslySetInnerHTML={{ __html: THEME_INIT_SCRIPT }} />
      </head>
      <body>{children}</body>
    </html>
  );
}
