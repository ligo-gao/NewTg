import { codeToHtml } from "shiki";

export function highlightCurl(code: string): Promise<string> {
  return codeToHtml(code, {
    lang: "bash",
    themes: {
      light: "github-light",
      dark: "github-dark-default",
    },
    defaultColor: false,
  });
}
