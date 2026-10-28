import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { RootDocument } from "./root-document";

describe("RootDocument", () => {
  it("renders the route's language on the html element before hydration", () => {
    const html = renderToStaticMarkup(
      <RootDocument lang="ja" fontClassName="font-test">
        <p>content</p>
      </RootDocument>,
    );

    expect(html).toContain('<html lang="ja"');
    expect(html).toContain("font-test");
    expect(html).toContain("content");
  });
});
