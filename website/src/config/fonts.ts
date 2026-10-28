import { Inter, JetBrains_Mono, Noto_Sans_SC } from "next/font/google";

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

/** Class list the two root layouts put on `<html>`. */
export const fontVariables = `${inter.variable} ${jetbrainsMono.variable} ${notoSansSc.variable}`;
