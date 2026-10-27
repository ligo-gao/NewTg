import { codeToHtml } from "shiki";

export function highlightCode(code: string, lang: string): Promise<string> {
  return codeToHtml(code, {
    lang,
    themes: {
      light: "github-light",
      dark: "github-dark-default",
    },
    defaultColor: false,
  });
}
