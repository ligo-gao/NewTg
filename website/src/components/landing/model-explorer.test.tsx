import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import { copy } from "@/config/copy";
import { vendors } from "@/config/models";
import { ModelExplorer } from "./model-explorer";

function articleFor(name: string) {
  const heading = screen.getByRole("heading", { name });
  const article = heading.closest("article");
  if (!(article instanceof HTMLElement)) {
    throw new Error(`Missing model card for ${name}`);
  }
  return within(article);
}

describe("ModelExplorer", () => {
  it("renders all six models and the total count", () => {
    render(<ModelExplorer />);

    expect(screen.getAllByRole("article")).toHaveLength(6);
    expect(screen.getByText(copy.models.countUnit)).toHaveTextContent("6 个模型");
  });

  it("filters the grid while typing in the labelled search box", async () => {
    const user = userEvent.setup();
    render(<ModelExplorer />);

    await user.type(screen.getByLabelText(copy.models.searchLabel), "deepseek");

    expect(screen.getAllByRole("article")).toHaveLength(1);
    expect(screen.getByRole("heading", { name: "DeepSeek-V3.2" })).toBeInTheDocument();
    expect(screen.getByText(copy.models.countUnit)).toHaveTextContent("1 个模型");
  });

  it("shows the empty state and resets the filters from it", async () => {
    const user = userEvent.setup();
    const google = vendors.find((vendor) => vendor.id === "google");
    if (!google) {
      throw new Error("Missing Google vendor");
    }

    render(<ModelExplorer />);

    await user.click(screen.getByRole("button", { name: google.name }));
    const search = screen.getByLabelText(copy.models.searchLabel);
    await user.type(search, "zzz");

    expect(screen.queryAllByRole("article")).toHaveLength(0);
    expect(screen.getByText(copy.models.emptyTitle)).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: copy.models.emptyAction }));

    expect(screen.getAllByRole("article")).toHaveLength(6);
    expect(screen.getByRole("button", { name: copy.models.allVendors })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(search).toHaveValue("");
    expect(search).toHaveFocus();
  });

  it("compares two selected models in the dialog and clears the selection", async () => {
    const user = userEvent.setup();
    render(<ModelExplorer />);

    const gptCard = articleFor("GPT-4o");
    const claudeCard = articleFor("Claude Sonnet 4.5");

    await user.click(
      gptCard.getByRole("checkbox", { name: `${copy.models.compare} GPT-4o` }),
    );
    await user.click(
      claudeCard.getByRole("checkbox", {
        name: `${copy.models.compare} Claude Sonnet 4.5`,
      }),
    );

    const bar = screen.getByRole("region", { name: copy.models.compareBar.ariaLabel });
    expect(bar).toHaveTextContent("已选 2 个模型");

    await user.click(within(bar).getByRole("button", { name: copy.models.compareBar.open }));

    const dialog = screen.getByRole("dialog", { name: copy.models.dialog.title });
    expect((dialog as HTMLDialogElement).open).toBe(true);

    const inDialog = within(dialog);
    expect(inDialog.getAllByRole("columnheader")).toHaveLength(3);
    expect(inDialog.getByRole("columnheader", { name: "GPT-4o" })).toBeInTheDocument();
    expect(inDialog.getByRole("columnheader", { name: "Claude Sonnet 4.5" })).toBeInTheDocument();
    expect(inDialog.queryByRole("columnheader", { name: "Gemini 2.5 Pro" })).not.toBeInTheDocument();

    await user.click(inDialog.getByRole("button", { name: copy.models.dialog.close }));
    expect((dialog as HTMLDialogElement).open).toBe(false);

    await user.click(within(bar).getByRole("button", { name: copy.models.compareBar.clear }));

    expect(screen.queryByRole("region", { name: copy.models.compareBar.ariaLabel })).not.toBeInTheDocument();
    expect(
      gptCard.getByRole("checkbox", { name: `${copy.models.compare} GPT-4o` }),
    ).not.toBeChecked();
    expect(
      claudeCard.getByRole("checkbox", {
        name: `${copy.models.compare} Claude Sonnet 4.5`,
      }),
    ).not.toBeChecked();
  });
});
