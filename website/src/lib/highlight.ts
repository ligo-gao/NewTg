import { codeToHtml } from "shiki";

export function highlightCurl(code: string): Promise<string> {
  return codeToHtml(code, {
    lang: "bash",
    theme: "github-dark-default",
  });
}
